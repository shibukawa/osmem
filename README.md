# osmem

An OpenSearch-compatible fake that runs entirely inside your Go test process.
No Docker, no JVM, no files on disk. Any OpenSearch or Elasticsearch HTTP
client can talk to it, and a seeded cluster can be cloned per test in
O(1) and thrown away.

```go
var base *osmem.Cluster

func TestMain(m *testing.M) {
    base = osmem.New()
    // seed once: mappings, documents, aliases ...
    if err := base.CreateIndex("products", `{"mappings": {"properties": {
        "name":  {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
        "price": {"type": "double"},
        "tags":  {"type": "keyword"}}}}`); err != nil {
        log.Fatal(err)
    }
    f, _ := os.Open("testdata/products.ndjson") // _bulk format
    if err := base.Bulk(f); err != nil {
        log.Fatal(err)
    }
    os.Exit(m.Run())
}

func TestSearch(t *testing.T) {
    c := base.Clone()          // independent copy, shares data until written
    defer c.Close()
    srv := c.MustServe()       // http://127.0.0.1:<port>
    defer srv.Close()

    client, _ := opensearchapi.NewClient(opensearchapi.Config{
        Client: opensearch.Config{Addresses: []string{srv.URL}},
    })
    res, err := client.Search(ctx, &opensearchapi.SearchReq{
        Indices: []string{"products"},
        Body:    strings.NewReader(`{"query": {"match": {"name": "apple"}}}`),
    })
    // ... the base cluster is untouched by anything this test does
}
```

Every clone has its own indices, mappings, documents and aliases. Cloning
copies nothing; an index is duplicated the first time either side writes
to it (copy-on-write), so read-only tests cost microseconds and a test that
writes pays once for the indices it touches.

## How it works

Documents, mappings, settings, aliases and templates are kept in Go data
structures. [bleve](https://github.com/blevesearch/bleve) (pure Go,
in-memory scorch index) is used only as an inverted index and BM25 scorer:
the OpenSearch mapping decides how every field is analysed and indexed,
the query DSL is translated to bleve queries, and sorting, `_source`
filtering, highlighting and all aggregations are computed in Go from the
stored documents. That keeps the observable behaviour close to OpenSearch
without emulating Lucene.

## Test helpers

`osmemtest` binds lifetimes to a test:

```go
func TestSearch(t *testing.T) {
    c, srv := osmemtest.CloneAndServe(t, base) // closed by t.Cleanup
    ...
}
```

`osmemtest.Clone(t, base)`, `osmemtest.Serve(t, c)` and `osmemtest.New(t)`
are the building blocks. Package `osmem` itself does not import `testing`.

## Other languages: osmem-server

`osmem-server` hosts a base cluster for test suites in Java, Python,
Node.js or anything else that speaks HTTP. Build it with
`go build ./cmd/osmem-server` (Japanese analysis is compiled in; `--no-ja`
turns it off).

```
osmem-server --seed ./testdata/seed [--freeze] [--addr 127.0.0.1:0] [--parent-pid N]
```

When listening it prints one JSON line on stdout:

```json
{"url":"http://127.0.0.1:51132","pid":83231,"version":"2.19.0","japanese":true,"indices":["products"]}
```

It exits when its stdin is closed (spawn it with a pipe and it dies with
the test runner), on SIGTERM/SIGINT, or when `--parent-pid` disappears.

A seed directory contains, applied in this order:

| file | request |
|---|---|
| `<name>.template.json` | `PUT /_index_template/<name>` |
| `<index>.index.json` | `PUT /<index>` (settings, mappings, aliases) |
| `<index>.ndjson` | `POST /<index>/_bulk` (actions may set `_index`) |
| `aliases.json` | `POST /_aliases` |

The same loader is available in Go as `c.LoadSeed(path)`. Instead of (or
in addition to) `--seed`, a suite can seed through the normal REST API and
then `POST /_osmem/base/freeze`.

Per-test isolation uses the management API under `/_osmem`:

| request | effect |
|---|---|
| `POST /_osmem/clones` | clone the base (freezing it), serve the clone on a new loopback port, respond `{"id","url"}` |
| `DELETE /_osmem/clones/{id}` | close that clone and its port |
| `GET /_osmem/clones`, `DELETE /_osmem/clones` | list / close all |
| `POST /_osmem/base/freeze`, `/_osmem/base/unfreeze` | reject / allow writes to the base (403 `osmem_base_frozen`) |
| `GET /_osmem` | version, frozen flag, clone count |

A test session fixture therefore spawns the binary once, and each test
creates a clone, points its OpenSearch client at the clone URL, and deletes
the clone afterwards. Read-only tests can use the base URL directly.

Ready-made helpers that do exactly this live under `packages/`:

| language | package | usage |
|---|---|---|
| Node.js | [`@osmem/core`](packages/node/core) on npm (binary in `@osmem/<platform>` optional dependencies) | `const server = await OsmemServer.start({seed}); await server.withClone(async (c) => ...)` |
| Python | [`osmem-server`](packages/python) on PyPI (platform wheels bundle the binary; import `osmem_server`) | pytest fixtures `osmem_server`, `osmem_clone`, `osmem_url`; `osmem_seed` ini option |
| Java | [`io.github.shibukawa.osmem:osmem`](packages/java) + `osmem-server-binaries` classifier jars | `@RegisterExtension static OsmemExtension osmem = OsmemExtension.seed(path);` then an `OsmemClone` test parameter |

`scripts/build-binaries.sh` cross-compiles the server for macOS (arm64),
Linux and Windows (amd64/arm64) from any host (a Mac builds every artifact,
including the Linux and Windows ones); `scripts/build-npm.sh`, `scripts/build-python-wheels.sh`
and `scripts/build-java-binaries.sh` assemble the per-platform artifacts.
Every helper also honours `OSMEM_SERVER_BIN` for a locally built binary.

## API

| Go | REST |
|---|---|
| `osmem.New(opts...)` | empty cluster (`WithClock`, `WithWarnings`, `WithClusterName`) |
| `c.Clone()`, `c.Close()` | copy-on-write clone |
| `c.Handler()` | `http.Handler` speaking the OpenSearch REST API |
| `c.Serve()` / `c.MustServe()` | HTTP server on a loopback port (`srv.URL`, `srv.Close()`) |
| `c.Do(method, path, body)` | any request without a network round trip |
| `c.CreateIndex`, `c.DeleteIndex`, `c.Index`, `c.Get`, `c.Bulk`, `c.BulkString`, `c.Search`, `c.Count`, `c.LoadSeed` | convenience wrappers |
| `c.Freeze()`, `c.ManagedClone()`, `c.CloseManagedClone(id)` | what the `/_osmem` management API does |

Bodies can be a string, `[]byte`, `io.Reader`, or any value marshalled as
JSON. `Bulk` returns a `*BulkError` listing failed items.

## What is implemented

**Indices:** create (settings, mappings, aliases), delete (wildcards,
`_all`), exists, get, `_mapping` (get/put with type-conflict errors, field
mappings), `_settings` (get/put, `flat_settings`), `_stats`, `_refresh` /
`_flush` / `_forcemerge` / `_open` / `_close` (no-ops), `_analyze`,
`_cat/indices`, `_cat/aliases`, `_cat/health`, `_cat/count`, cluster
health/settings/state, `_nodes`, root info. Composable index templates
(`_index_template`, priority) and legacy `_template` are applied on index
creation, including auto-created indices.

**Documents:** `_doc` (PUT/POST/GET/HEAD/DELETE, auto ids, `op_type`,
`if_seq_no`/`if_primary_term`, external versions), `_create`, `_update`
(`doc`, `doc_as_upsert`, `upsert`, `detect_noop`), `_source`, `_mget`,
`_bulk` (index/create/update/delete with per-item status), `_delete_by_query`,
`_update_by_query` (without script), `_reindex`. Documents are visible
immediately; `refresh` is accepted and ignored.

**Mapping:** text (analyzer, search_analyzer, multi-fields), keyword
(normalizer, ignore_above), all numeric types, boolean, date/date_nanos
(`format` with named formats, Java patterns, `epoch_millis`/`epoch_second`),
geo_point, ip, object/nested (nested is flattened), `null_value`, `copy_to`,
`index: false`, `enabled: false`, `dynamic: true/false/strict`, dynamic
mapping with OpenSearch's rules (strings become `text` with a `.keyword`
sub-field, ISO dates are detected, integers become `long`, decimals `float`),
dotted keys expand to objects.

**Analysis:** standard, simple, whitespace, keyword, stop, pattern and the
language analyzers bleve provides; custom analyzers built from tokenizers
(standard, whitespace, keyword, letter, pattern, ngram, edge_ngram,
char_group, uax_url_email), token filters (lowercase, asciifolding, stop,
ngram, edge_ngram, shingle, stemmer/snowball, porter_stem, truncate, length,
unique, reverse, cjk_bigram, cjk_width) and char filters (html_strip,
pattern_replace); normalizers. Unsupported components are skipped with a
warning (`WithWarnings`, `c.Warnings(index)`).

**Japanese:** import `github.com/shibukawa/osmem/ja` for its side effects
to get real morphological analysis through
[kagome](https://github.com/ikawaha/kagome) (pure Go, IPA dictionary): the
`kuromoji` analyzer, `kuromoji_tokenizer` (mode normal/search/extended,
`discard_punctuation`, `user_dictionary_rules` in kuromoji's CSV format,
`user_dictionary` file), `kuromoji_baseform`, `kuromoji_part_of_speech`
(`stoptags`), `cjk_width`, `ja_stop` (`stopwords`), `kuromoji_stemmer`
(`minimum_length`), `kuromoji_readingform` (`use_romaji`). Byte offsets are
preserved, so highlighting works. Without the import, `kuromoji`/`nori`/
`smartcn` fall back to bleve's CJK bigram analyzer. The dictionary adds
about 10 MB to the test binary and loads once per process.

```go
import _ "github.com/shibukawa/osmem/ja"
```

**Queries:** match (operator, minimum_should_match, fuzziness,
zero_terms_query), match_phrase, match_phrase_prefix, match_bool_prefix,
match_phrase with `slop`, multi_match (best_fields/most_fields/cross_fields/
phrase/phrase_prefix/bool_prefix, `^boost`, wildcard fields), term (typed by mapping, `case_insensitive`),
terms (including terms lookup), range (numbers, dates with date math,
`format`, `time_zone`, strings), exists, prefix, wildcard, regexp, fuzzy,
ids, bool (must/filter/should/must_not, minimum_should_match), constant_score,
dis_max, boosting and function_score (positive/inner query only), nested
(flattened), query_string and simple_query_string (AND/OR/NOT, +/-, phrases,
`field:value`, wildcards, `~`, ranges, `_exists_`), geo_distance,
geo_bounding_box, wrapper. Unsupported query types return
`unsupported_operation_exception` (400) rather than wrong results.

**Search:** from/size (`max_result_window` = 10000), sort (fields with
order/missing/mode/unmapped_type/format, `_score`, `_doc`, `_id`, multi-index,
OpenSearch's sentinel sort values for missing numbers and dates),
search_after (including date strings and sentinels), scroll, point in time, `_source` filtering, `fields` /
`docvalue_fields`, `version`, `seq_no_primary_term`, `track_total_hits`,
`track_scores`, `min_score`, `post_filter`, `collapse`, highlight (pre/post
tags, fragment_size, number_of_fragments, wildcard fields), `_count`,
`_msearch`, `filter_path`, `pretty`, `rest_total_hits_as_int`,
`q`/`df`/`default_operator` parameters, gzip request bodies.

**Clients:** tested with opensearch-go v4. `_nodes` reports the real
listen address, so clients that sniff (olivere/elastic) work. Setting the
cluster setting `compatibility.override_main_response_version: true` makes
`GET /` report Elasticsearch 7.10.2 for old Elasticsearch clients;
go-elasticsearch v8's product check header is always sent.

**Aggregations:** terms (size, order by count/key/sub-aggregation,
min_doc_count, missing, include/exclude), multi_terms, range, date_range,
histogram, date_histogram (calendar/fixed intervals, time_zone, offset,
format, extended_bounds, min_doc_count 0 gap filling), filter, filters
(keyed/anonymous, other_bucket), missing, global, nested/reverse_nested/
sampler (pass-through), composite (terms/histogram/date_histogram sources,
`after`), avg/sum/min/max/value_count, stats, extended_stats, cardinality
(exact), percentiles, percentile_ranks, top_hits, weighted_avg,
median_absolute_deviation; pipelines: cumulative_sum, derivative,
bucket_sort, avg/sum/min/max/stats_bucket. Sub-aggregations nest freely.

**Aliases:** `_aliases` actions (add/remove/remove_index) with filters and
`is_write_index`, `_alias` get/put/delete/exists, wildcard patterns.

## Differences from OpenSearch

- **Scores are relative, not identical.** bleve's BM25 is not Lucene's.
  Rankings for simple queries agree; exact `_score` values and ties do not.
  Do not assert on score values.
- **No Painless.** `script`, `script_score`, scripted updates,
  `bucket_script`, `bucket_selector` and `_update_by_query` with a script
  return 400. `function_score` runs the inner query only.
- **No refresh semantics.** Writes are visible to the next search, always.
- **Nested documents are flattened.** A `nested` query matches when the
  fields match anywhere in the array, like an `object` field.
- **Analyzers are approximations.** Language analyzers use bleve's stemmers
  and stop lists. Japanese is kagome/IPADIC when `osmem/ja` is imported
  (segmentation can differ from kuromoji in details, `kuromoji_number` is
  not implemented); Korean/Chinese are CJK bigrams.
- **Not implemented:** suggesters, kNN/neural search, percolator, join
  fields, span queries, geo shapes, significant_terms and other statistical
  aggregations, ingest pipelines, security, snapshots, cluster/node
  management beyond stubs, index-level `max_result_window` changes.
- Aggregations are computed exactly over all matching documents, so
  `cardinality` and `percentiles` are exact rather than approximate.

## Performance

Measured on an Apple M-series laptop (`go test -bench .`):

| | |
|---|---|
| bulk index 10,000 small documents | ~0.4 s |
| clone + read-only search | ~0.4 ms |
| clone + first write to a 10,000-document index | ~0.4 s (re-index) |
| search with bool query, sort and date_histogram over 10,000 documents | ~2 ms |
| term query, 10,000 documents | ~50 µs |

Each search reads every matching document from Go maps, so very large
indices (millions of documents) are not the target; test fixtures are.

## Releasing

CI (`.github/workflows/ci.yml`) runs the Go tests on Linux, macOS and
Windows and the Node.js, Python and Java package tests against a freshly
built binary. Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`,
which stamps the version into every manifest (`scripts/set-version.sh`),
builds all binaries, wheels, npm packages and Java jars, attaches them to a
GitHub Release, publishes to npm and PyPI, and needs:

- npm trusted publishers (no token): each of the six packages must list
  `shibukawa/osmem`, workflow `release.yml`, environment `release` under
  Settings > Trusted Publisher on npmjs.com. A trusted publisher can only
  be attached to an existing package, so version 0.1.0 is published once
  by hand from a machine with 2FA: `scripts/build-npm.sh`, then
  `npm publish --access public` in each `packages/node/platforms/*` and in
  `packages/node/core`;
- a PyPI trusted publisher for project `osmem-server` (PyPI rejects `osmem`
  and `os-mem` as too close to an existing project): on pypi.org, Account > Publishing > "Add a new pending
  publisher" with owner `shibukawa`, repository `osmem`, workflow
  `release.yml`, environment `release`. Pending publishers work before the
  first upload, so no manual publish is needed;
- Maven Central through the Central Publisher Portal
  (central.sonatype.com): the namespace `io.github.shibukawa`, verified
  automatically when signing in to the portal with GitHub, a portal user
  token stored as the `CENTRAL_USERNAME` / `CENTRAL_PASSWORD` secrets, and a
  GPG signing key: `GPG_PRIVATE_KEY` (armored private key, its public key
  uploaded to keys.openpgp.org or keyserver.ubuntu.com) and
  `GPG_PASSPHRASE`. Locally: `scripts/build-java-binaries.sh`, then
  `cd packages/java && mvn -Prelease deploy` with the same server id
  `central` in `~/.m2/settings.xml`;
- a GitHub environment named `release` (optionally with required
  reviewers, which then gates every publish).
- Maven Central publishing is manual for now: upload `dist/java/*.jar` and
  the `osmem` jar through the Central publisher portal.

## License

MIT
