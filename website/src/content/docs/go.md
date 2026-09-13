---
title: "Go guide"
description: "Run osmem inside Go tests: seed in TestMain, isolate tests with osmemtest, and use the in-process API."
---

In Go, osmem runs inside the test binary. There is no process to start and no port to wait for: a cluster is a value, a clone is a method call, and an HTTP server exists only for the client library you point at it. This page shows the TestMain pattern, per-test isolation with `osmemtest`, and the options you will reach for.

## Install

```bash
go get github.com/shibukawa/osmem
```

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
