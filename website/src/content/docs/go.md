---
title: "Go guide"
description: "Run osmem inside Go tests: seed in TestMain, isolate tests with osmemtest, and use the in-process API."
---

In Go, osmem runs inside the test binary. There is no process to start and no port to wait for: a cluster is a value, a clone is a method call, and an HTTP server exists only for the client library you point at it. This page shows the TestMain pattern, per-test isolation with `osmemtest`, and the options you will reach for.

## Install

```bash
go get github.com/shibukawa/osmem
# For the official OpenSearch client shown below:
go get github.com/opensearch-project/opensearch-go/v4
```

The client is optional. `Cluster.Do` and the convenience methods can exercise the same REST behavior without a network connection.

## Build the base once

Seed in `TestMain` so every test starts from the same state. `LoadSeed` reads a [seed directory](../seed-data/); `BulkString` and `CreateIndex` do the same thing from Go values.

```go
package shop_test

import (
    "log"
    "testing"

    "github.com/shibukawa/osmem"
)

var base *osmem.Cluster

func TestMain(m *testing.M) {
    base = osmem.New()
    defer base.Close()
    if err := base.LoadSeed("testdata/seed"); err != nil {
        log.Fatal(err)
    }
    m.Run()
}
```

`TestMain` may simply return: since Go 1.15 the result of `m.Run` becomes the exit code, so the deferred `Close` runs. osmem deliberately offers no helper that takes `*testing.M`. Other in-memory fakes, such as pgmem for PostgreSQL, set themselves up in the same function with their own `defer`, and nothing has to decide which library owns the test binary.

Nothing mutates `base` after this point. Tests that only read may use it directly; tests that write take a clone.

## Register a schema, seed data, and use the official client

`LoadSeed` applies index templates, index schemas, bulk documents, then aliases. The same setup can be written as `CreateIndex` plus `BulkString` if a test needs to build its fixture in code. To use the official `opensearch-go` client, expose the cluster on a loopback port:

```go
c := osmem.New()
defer c.Close()
if err := c.CreateIndex("products", map[string]any{"mappings": map[string]any{"properties": map[string]any{
    "name": map[string]any{"type": "text", "fields": map[string]any{"keyword": map[string]any{"type": "keyword"}}},
}}}); err != nil {
    log.Fatal(err)
}
if err := c.Index("products", "1", map[string]any{"name": "Red Apple"}); err != nil {
    log.Fatal(err)
}
srv := c.MustServe()
defer srv.Close()

client, err := opensearchapi.NewClient(opensearchapi.Config{
    Client: opensearch.Config{Addresses: []string{srv.URL}},
})
if err != nil { log.Fatal(err) }
ctx := context.Background()
result, err := client.Search(ctx, &opensearchapi.SearchReq{
    Indices: []string{"products"},
    Body: strings.NewReader(`{"query":{"match":{"name":"apple"}}}`),
})
if err != nil { log.Fatal(err) }
```

The schema, documents, and queries are ordinary OpenSearch REST operations. `Serve` is only needed when the test must exercise the actual client transport; `c.Do` is faster for direct in-process assertions.

## One clone per test

`osmemtest` binds lifetimes to a test through `t.Cleanup`, so the test body contains no teardown.

```go
import (
    "context"
    "strings"

    "github.com/opensearch-project/opensearch-go/v4"
    "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
    "github.com/shibukawa/osmem/osmemtest"
)

func TestAddProduct(t *testing.T) {
    ctx := context.Background()
    _, srv := osmemtest.CloneAndServe(t, base)
    client, err := opensearchapi.NewClient(opensearchapi.Config{
        Client: opensearch.Config{Addresses: []string{srv.URL}},
    })
    if err != nil {
        t.Fatal(err)
    }

    // this write never reaches base or any other test
    _, err = client.Index(ctx, opensearchapi.IndexReq{
        Index: "products", DocumentID: "x", Body: strings.NewReader(`{"name": "new"}`),
    })
    if err != nil {
        t.Fatal(err)
    }
    got, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "products", DocumentID: "x"})
    if err != nil || !got.Found {
        t.Fatalf("document not found: %v", err)
    }
}
```

`osmemtest.Clone(t, base)` and `osmemtest.Serve(t, c)` are the two halves if you need them separately, and `osmemtest.New(t)` gives an empty cluster for tests that build their own mappings.

`t.Parallel()` is safe as long as each writing test owns its clone. Two parallel tests sharing one clone would see each other's documents, exactly as they would on a shared OpenSearch.

## Talking to the cluster without a network

For setup code and assertions, `Do` runs a request through the handler in-process:

```go
res, err := c.Do(http.MethodPost, "/products/_search", `{"query": {"term": {"tags": "red"}}}`)
if err != nil || res.IsError() {
    t.Fatal(err, string(res.Body))
}
```

Wrappers cover the common cases: `CreateIndex`, `Index`, `Get`, `Bulk`, `Search` (decodes into a struct), `Count`, `DeleteIndex`. All of them return OpenSearch's own error type and reason in the Go error.

## Japanese text

Import the `ja` package once and the `kuromoji` analyzer, `kuromoji_tokenizer` and its filters become available in mappings and index settings, backed by kagome (a pure Go morphological analyzer with the IPA dictionary):

```go
import _ "github.com/shibukawa/osmem/ja"
```

Without the import, `kuromoji` falls back to CJK bigrams, which matches an OpenSearch cluster that does not have the analysis-kuromoji plugin installed. The dictionary adds about 8 MB to the test binary and loads once per process.

## Options

- `osmem.WithClock(func() time.Time)` fixes "now" for date math (`now-7d/d`) and creation dates, which makes range queries reproducible.
- `osmem.WithWarnings(func(string))` reports analysis settings osmem had to approximate, such as a token filter without a bleve equivalent.
- `c.Freeze()` rejects HTTP writes with 403, the same protection the subprocess form applies automatically. Go calls keep working; freezing guards the network side only.
- `c.Handler()` returns the `http.Handler` when you want your own server, for example `httptest.NewServer(c.Handler())`.

## Refresh and sorting

Writes are visible to the next search without `_refresh`; the endpoint is accepted and ignored. Sorting, on the other hand, follows OpenSearch strictly: a `sort` on a `text` field fails with the same `illegal_argument_exception`, so keep using `.keyword` sub-fields rather than working around the error.

## Choose the test lifetime

- **New cluster per test:** call `osmemtest.New(t)` and build that test's schema and data there. This gives complete isolation but repeats setup.
- **One seeded base for the package:** load it in `TestMain`, as above. Read-only tests can share it; writable tests should call `osmemtest.CloneAndServe(t, base)` (or `Clone` when no HTTP client is needed).
- **A test that edits existing documents or mappings:** treat the clone as a fork. It starts from the seeded base, and copy-on-write separates the first modified index. Closing the clone discards those changes; the base stays intact. A write to a large index can cost substantially more than the O(1) clone itself.

For parallel tests, share the immutable base, never the writable clone.

An isolated test can load the shared fixture itself:

```go
func TestIsolatedSearch(t *testing.T) {
    c := osmemtest.New(t)
    if err := c.LoadSeed("testdata/seed"); err != nil {
        t.Fatal(err)
    }
    res, err := c.Do(http.MethodPost, "/products/_search", `{"query":{"match_all":{}}}`)
    if err != nil || res.IsError() {
        t.Fatal(err, string(res.Body))
    }
}
```
