# osmem

**Documentation:** [English](https://shibukawa.github.io/osmem/) · [日本語](https://shibukawa.github.io/osmem/ja/)

An OpenSearch-compatible fake that runs entirely inside your Go test process.
No Docker, no JVM, no files on disk. Any OpenSearch or Elasticsearch HTTP
client can talk to it, and a seeded cluster can be cloned per test in
O(1) and thrown away.

```go
var base *osmem.Cluster

func TestMain(m *testing.M) {
    base = osmem.New()
    defer base.Close()
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
    m.Run()
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
{"url":"http://127.0.0.1:51132","pid":83231,"version":"3.8.0","japanese":true,"indices":["products"]}
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
`_all`), exists, get, `_mapping` (get/put with OpenSearch's parameter
validation and conflict errors), `_settings` (get/put validated against
OpenSearch's settings registry, `include_defaults`, `flat_settings`,
`settings_filter`), `_stats` (all sections, `level=shards`; counters are
zero), `_refresh` / `_flush` / `_forcemerge` (no-ops), `_open` / `_close`
(closed indices reject reads and writes with `index_closed_exception`),
index and cluster blocks, hidden indices and `expand_wildcards`,
`_resolve/index`, `_analyze`, cluster health (`level`, `wait_for_*`, 408 on
timeout), cluster settings/state/stats, `_nodes`, root info. Composable,
component and legacy templates are composed and applied on index creation,
including auto-created indices, and simulation reports overlapping
templates. Data streams are rejected.

**Cat:** `_cat/indices`, `aliases`, `health`, `count`, `nodes`,
`master`/`cluster_manager`, `plugins`, `templates`, `shards`, `segments`,
`recovery`, `allocation`, `thread_pool`, `pending_tasks`, `fielddata`,
`nodeattrs`, `tasks`, `repositories`, `snapshots`, `segment_replication`
(`format`, `h`, `v`, `s`, `help`, `bytes`, `time`, `pri`).

**Documents:** `_doc` (PUT/POST/GET/HEAD/DELETE, auto ids, `op_type`,
`if_seq_no`/`if_primary_term`, external versions including deletes, deletion
tombstones for `index.gc_deletes`), `_create`, `_update` (`doc`,
`doc_as_upsert`, `upsert`, `detect_noop`), `_source`, `_mget`, `_bulk`
(index/create/update/delete with per-item status, parsed line by line),
`_delete_by_query` and `_update_by_query` (a query is required; no script;
`wait_for_completion=false` tasks), `_reindex`. GET and search hits return
`_routing` and `_ignored`. Documents are visible immediately; `refresh` is
accepted and ignored.

**Mapping:** text (analyzer, search_analyzer, multi-fields),
match_only_text, keyword (normalizer, ignore_above), all numeric types with
OpenSearch's value validation and coercion, boolean, date/date_nanos
(`format` with named formats, Java patterns, `epoch_millis`/`epoch_second`,
nanosecond precision), geo_point, ip, object/nested (nested objects are
indexed as documents of their own, as on OpenSearch), field aliases,
`null_value`, `copy_to`, `index: false`, `enabled: false`,
`ignore_malformed`, `dynamic` (including `strict_allow_templates`), dynamic
templates, range types, search_as_you_type, flat_object, join,
rank_feature(s), knn_vector and completion values, `include_in_parent` /
`include_in_root`, dynamic
mapping with OpenSearch's rules (strings become `text` with a `.keyword`
sub-field, dates and numbers are detected according to `date_detection` and
`numeric_detection`), dotted keys expand to objects. Unknown or invalid
mapping parameters fail with OpenSearch's errors.

**Analysis:** standard, simple, whitespace, keyword, stop, pattern and the
language analyzers bleve provides; custom analyzers built from tokenizers
(standard, whitespace, keyword, letter, pattern, ngram, edge_ngram,
char_group, uax_url_email), token filters (lowercase, asciifolding, stop,
ngram, edge_ngram, shingle, stemmer/snowball, porter_stem, truncate, length,
unique, reverse, cjk_bigram, cjk_width) and char filters (html_strip,
pattern_replace); normalizers. Unsupported components are skipped with a
warning (`WithWarnings`, `c.Warnings(index)`). `_analyze` reproduces
OpenSearch's token types, UTF-16 offsets and `explain` output for the
components it implements and rejects the others, so its output can differ
from the terms bleve indexes.

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
zero_terms_query), match_phrase and match_phrase_prefix (token positions,
`slop`), match_bool_prefix, multi_match (all types, `^boost`, wildcard
fields, cross_fields grouped by analyzer), combined_fields, common,
term/terms (typed by mapping, terms lookup, `case_insensitive`), terms_set,
range (numbers, dates with date math, `format`, `time_zone`), exists,
prefix, wildcard, regexp (Lucene syntax, `flags`), fuzzy, ids, bool,
constant_score, dis_max (`tie_breaker`), boosting, function_score (weight,
field_value_factor, random_score, decay functions, score and boost modes),
nested (score_mode, ignore_unmapped, inner_hits with `_nested` identities),
query_string (Lucene's classic syntax) and simple_query_string (flags),
geo_distance, geo_bounding_box, geo_polygon, rank_feature, parent_id,
has_child/has_parent, range queries on range fields, wrapper; `_name` adds
`matched_queries`. Malformed queries fail with OpenSearch's errors, and
unsupported query types return `unsupported_operation_exception` (400)
rather than wrong results.

**Search:** from/size (`index.max_result_window`, 10000 by default), sort
(fields with order/missing/mode/unmapped_type, `_score`, `_doc`, `_id`, multi-index,
OpenSearch's sentinel sort values for missing numbers and dates),
search_after (including date strings and sentinels), scroll, point in time, `_source` filtering, `fields` /
`docvalue_fields`, `version`, `seq_no_primary_term`, `track_total_hits`,
`track_scores`, `min_score`, `post_filter`, `collapse`, rescore,
`indices_boost`, `slice`, `terminate_after` (per shard), highlight (the
unified, plain and fvh highlighters with OpenSearch's fragmenting and
scoring), `_count`, `_msearch`, `typed_keys`, `_validate/query` (`explain`,
`rewrite`), `_explain/{id}`, `preference=_shards:`, `_geo_distance` sort,
`filter_path` (with exclusions and `**`), `pretty`, `rest_total_hits_as_int`,
`q`/`df`/`default_operator` and `source` parameters, gzip request bodies.
Search body keys are validated with OpenSearch's messages.

**Requests and responses:** URL parameters are checked per endpoint as
OpenSearch checks them (unknown parameters and invalid values return 400
with OpenSearch's message and suggestions), bodies need a JSON
`Content-Type` (406 otherwise), duplicate JSON keys are rejected, and wrong
methods return 405 with an `Allow` header. Errors carry OpenSearch's
`root_cause`/`caused_by` chains and shard failures, and parse errors carry
OpenSearch's positions (`[line:col]` reason prefixes and `line`/`col`
fields). Numbers are written as
Java writes them (`1.0`, `1.0E-4`), with `float`/`half_float`/`scaled_float`
precision in sort values, aggregations and `fields`.

**Clients:** tested with opensearch-go v4. `_nodes` reports the real
listen address, so clients that sniff (olivere/elastic) work. Setting the
cluster setting `compatibility.override_main_response_version: true` makes
`GET /` report Elasticsearch 7.10.2 for old Elasticsearch clients;
go-elasticsearch v8's product check header is always sent.

**Aggregations:** terms, multi_terms, range, date_range, ip_range,
geo_distance, histogram, date_histogram (calendar/fixed intervals,
time_zone, offset, format, extended_bounds, min_doc_count 0 gap filling),
filter, filters (keyed/anonymous, other_bucket), adjacency_matrix, missing,
global, nested/reverse_nested, sampler, composite (`after`), geohash_grid,
geotile_grid; avg/sum/min/max/value_count, stats, extended_stats,
cardinality (exact), percentiles/percentile_ranks (t-digest),
median_absolute_deviation, top_hits, weighted_avg, geo_bounds,
geo_centroid; pipelines: cumulative_sum, derivative, bucket_sort,
avg/sum/min/max/stats/extended_stats/percentiles_bucket, serial_diff,
moving_avg (simple/linear/ewma), bucket_script, bucket_selector.
`meta` and `typed_keys` are rendered.
Sub-aggregations nest freely.

**Aliases:** `_aliases` actions (add/remove/remove_index) with filters,
routing, `is_write_index`, `is_hidden` and `must_exist`, applied
atomically; `_alias` get/put/delete/exists, wildcard patterns.

## Differences from OpenSearch

- **Scores are relative, not identical.** bleve's BM25 is not Lucene's.
  Rankings for simple queries agree; exact `_score` values and ties do not.
  Do not assert on score values.
- **Painless is read-only.** `script_fields`, the `script`/`script_score`
  queries, the `script_score` function of `function_score`, sort by
  `_script`, and the `bucket_script`/`bucket_selector` pipelines run: a
  document script reads `doc[...]`, `params` and `_score`; a bucket script
  reads only `params`. Scripts that mutate a document (`ctx._source`
  updates, `_update_by_query`/`_reindex` with a script, `scripted_metric`),
  and `moving_fn`, return 400.
- **No refresh semantics.** Writes are visible to the next search, always.
- **Analyzers are approximations.** Language analyzers use bleve's stemmers
  and stop lists. Japanese is kagome/IPADIC when `osmem/ja` is imported
  (segmentation can differ from kuromoji in details, `kuromoji_number` is
  not implemented); Korean/Chinese are CJK bigrams.
- **Routing does not place documents.** `_routing` is stored and returned,
  but a document is found with any routing value.
- **Not implemented:** suggesters, kNN/neural search, percolator,
  `inner_hits` on join queries, span/intervals/more_like_this queries, geo shapes,
  significant_terms and other statistical aggregations, ingest pipelines,
  data streams, rollover/shrink/split/clone, `_tasks`, security, snapshots,
  node statistics, YAML/CBOR/SMILE bodies and `format=yaml`, `error_trace`
  stack traces.
- **One error per request.** When a body has several problems, osmem can
  report a different one than OpenSearch because it checks keys in sorted
  order rather than in document order.
- `cardinality` is computed exactly; `percentiles` use t-digest as
  OpenSearch does.

## Performance

Measured on an Apple M-series laptop (`go test -bench . -benchmem`, 2026-09-16;
see [Performance and footprint](https://shibukawa.github.io/osmem/performance/)
for conditions and the full before/after):

| | |
|---|---|
| bulk index 10,000 small documents | ~0.45 s |
| clone + read-only search | ~0.18 ms |
| clone + first write to a 10,000-document index | ~0.43 s (re-index) |
| search with bool query, sort and date_histogram over 10,000 documents | ~4.5 ms |
| term query, 10,000 documents | ~45 µs |
| 5,000 single-document writes (`_doc` one at a time), then a term query | ~27 MiB heap, ~520 µs/write, ~43 µs/query |
| sort 100,000 documents, return a page of 10 | 55–79 ms, depending on the sort key |

Each search reads every matching document from Go maps, so very large
indices (millions of documents) are not the target; test fixtures are.

In-memory segment merging (previously: no merging at all, since bleve's
scorch index only merges when given a directory) and typed, top-K-bounded
sort execution (previously: a full stable sort with per-hit `[]any` key
extraction) landed since the last measurement: indexing 5,000 documents one
at a time used to hold 2.1 GiB of heap and answer a term query in 2.4 ms;
sorting 100,000 documents for a 10-hit page used to take 230–320 ms
depending on the sort key.

Re-measuring also caught two accidental regressions from the same-day
OpenSearch 3.8 REST-API compatibility pass, both fixed: the HTTP route
table was being rebuilt on every `New()`/`Clone()` call instead of once per
process, and every query was filtered for root documents even on mappings
with no nested field to filter. `Clone()` alone now runs in under a
microsecond—faster than any previously measured number. One related cost
is *not* fixed: a `bool` query's `_search` (the row above) computes
Lucene-faithful scores by materializing every clause's matches in memory,
which an earlier, less correct implementation didn't need to do; that's an
inherent trade-off of the correctness fix, not a bug. See [Performance and
footprint](https://shibukawa.github.io/osmem/performance/) for the full
before/after and the profiling that found each cause.

## Releasing

CI (`.github/workflows/ci.yml`) runs the Go tests on Linux, macOS and
Windows and the Node.js, Python and Java package tests against a freshly
built binary. Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`,
which stamps the version into every manifest (`scripts/set-version.sh`),
builds all binaries, wheels, npm packages and Java jars, attaches them to a
GitHub Release, publishes to npm and PyPI, and needs:

Future releases are planned to use `v1.<OpenSearch-major>.<osmem-release>`:
the leading `1` is fixed, the second segment follows the OpenSearch major
(currently `9`), and the last segment is osmem's own release number. The
planned series is `v1.9.y`; it is independent of the OpenSearch server/client
version. The initial `0.1.0` package bootstrap predates this scheme.

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
