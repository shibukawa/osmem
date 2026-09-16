---
title: "Performance and footprint"
description: "Measured startup, process and container memory, linked application size, Docker, Testcontainers, and Devbox costs, with conditions and limits."
---

Japanese analysis makes a visible difference: loading the seed took Go embedded from 2.02 ms to 335 ms. SDK launch adds another boundary—child-process creation, and for Java, extracting the bundled executable from the classpath. These are local measurements, not guarantees.

This page was re-measured on 2026-09-16 after a round of OpenSearch 3.8
REST-API compatibility fixes and two targeted performance changes
(in-memory segment merging for single-document writes, typed top-K sort
execution). Re-measuring surfaced two more regressions that turned out to
be accidental rather than an inherent cost of the compatibility work:
`Cluster.Clone()` and plain queries had gotten slower because the HTTP
route table was being rebuilt on every `New()`/`Clone()` call, and because
every query was filtered through an extra clause meant for nested
documents even on mappings that have none. Both were profiled and fixed in
this same pass (see below); `Clone()` in particular now runs faster than
any previously published number. One cost remains open: bool-query
`_search` latency, which is an inherent trade-off of a Lucene-faithful
score-combination rewrite, not a bug—also covered below.

## Write and sort performance

Two changes landed since the previous measurement:

- **In-memory segment merging** ([decision record](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/in-memory-segment-merge.md)): bleve's scorch index never merges segments unless it owns a directory, so every single-document write used to keep a segment of its own—and each segment reserves buffer space for around 100 documents. Indices now track which documents each segment holds and re-index small segments together instead.
- **Typed, top-K-bounded sort** ([decision record](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/sort-execution.md)): sorting resolved a spec and extracted keys through `[]any` on every hit and ran a full stable sort even when only a page of 10 was requested. Sort keys are now typed and cached at write time, and a bounded heap selects just the requested page.

Measured by building the commit right before this work (`0631107`) and
current `HEAD` on the same machine and running the identical benchmark
back to back; `BenchmarkSingleDocWrites` and `BenchmarkSort100k` did not
exist before this work; their pre-fix numbers below reuse the pre-fix
`Cluster` API under the same workload. Medians of 3–5 runs.

| Benchmark | Before | After |
|---|---:|---:|
| 5,000 single-document writes: heap retained | 2,113 MiB | 26.1 MiB |
| 5,000 single-document writes: term query | 2.44 ms | 43.7 µs |
| 5,000 single-document writes: time per write | 1.83 ms | 480 µs |
| 1,000 single-document writes: heap retained | 421.5 MiB | 12.0 MiB |
| 1,000 single-document writes: term query | 301 µs | 44.8 µs |
| sort 100k docs, page 10, by `double` | 228.8 ms | 73.5 ms |
| sort 100k docs, page 10, by `date` | 268.3 ms | 55.4 ms |
| sort 100k docs, page 10, by `keyword` | 260.0 ms | 70.9 ms |
| sort 100k docs, page 10, by two keys | 323.4 ms | 79.1 ms |

At 1,000 single-document writes, per-write latency is roughly unchanged
(consistently ~450–500 µs either way); the unmerged-segment penalty mainly
shows up in retained heap and query cost at that scale. It grows with
writes: at 5,000 writes the old code was also paying escalating GC cost on
its 2 GiB heap, which the new code avoids by keeping segments bounded at
`documents/1000 + 27`.

## Two regressions found and fixed, one left open

The same session also merged a broad OpenSearch 3.8 response-fidelity pass
(commit `71962c6`): comparing osmem with real OpenSearch 3.8.0 and 2.19.1
over 477 scenarios, differing requests dropped from 3,263 to 308 of 6,139.
Re-measuring this page found that `Cluster.Clone()` and plain queries had
gotten measurably slower since the last measurement. Bisecting commit by
commit on this machine (each built and benchmarked back to back) traced it
to two specific, unrelated causes—neither one actually required by the
compatibility work, both fixed here:

- **The route table was rebuilt on every `New()`/`Clone()` call.** `newHTTPHandler` called `buildRoutes()` (over 150 routes, each a closure) and re-built the routing trie from scratch every time, and had done so since the very first commit; `71962c6` made each route more expensive to construct (added parameter-validation metadata) without anyone noticing the table wasn't cached. No handler closure or trie node is specific to one `Cluster`—every handler takes the `*httpHandler` as an argument and reaches the cluster through it—so the table and trie are now built once per process and shared. [Decision record.](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/shared-route-table.md)
- **Every query was filtered for root documents, even with no nested field in the mapping.** [Nested-as-child-documents](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/nested-child-documents.md) (2026-09-14) added a second query clause to keep nested child documents out of every hit list, unconditionally. A mapping with no nested field can never index a nested child, so the clause was always a no-op there—but it still doubled bleve's `DocumentMatchPool` pre-allocation cost by pushing the request past bleve's normal 1,000-document pre-allocation cap. The clause is now skipped when the mapping has no nested field. [Decision record.](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/nested-filter-scoping.md)

Both were confirmed with `pprof` (`-cpuprofile`/`-memprofile` on the affected benchmarks) before fixing, and `go test -race ./...` passes after. The columns below are the last published baseline, this session's regressed-but-not-yet-fixed measurement, and the fixed current state:

| Benchmark | Last published | Regressed | Fixed (now) |
|---|---:|---:|---:|
| `Cluster.Clone()` alone (no query) | 14.4 µs | 60.1 µs | **0.8 µs** |
| `Cluster.Clone()` + a `_count` query | 315 µs | ~407 µs | **~175 µs** |
| plain term query, 10k docs | 37.7 µs | ~72 µs | **~45 µs** |
| `_search`: bool query + sort + date_histogram, 10k docs | 1.78 ms | 4.5 ms | 4.5 ms (see below) |

`Clone()` alone is now faster than it has ever measured on this page—it
takes no HTTP request and does no query-DSL parsing, so removing the
per-call route rebuild left it as close to a bare in-process
copy-on-write fork as this benchmark gets. The term query lands close to
its original baseline; the small remaining gap is untouched by either fix
and wasn't investigated further this round.

**`_search` with a bool query is not fixed, and isn't a bug.** Its query
goes through a different mechanism, also added by `71962c6`
(`internal/engine/query_exec.go`): bleve's own compound searchers combine
scores by Lucene query norms and coordination factors that don't match
real Lucene, so osmem's `bool`/`dis_max`/`boosting`/`function_score`
queries now compute Lucene-correct scores themselves. Doing that requires
draining every match of every clause into memory before combining them—it
cannot stream or early-terminate the way a single-clause query can. That's
an inherent cost of the correctness fix, not an accident, and profiling
confirms it: `search.NewDocumentMatchPool`, called with the clause's full
match count, is the largest allocator in a `BenchmarkSearch10k` memory
profile. A fix would mean an early-terminating evaluator that still
combines scores the Lucene way—a larger redesign, not attempted here.

## Startup by API

Re-measured on 2026-09-16 on the same Apple M3 / Go 1.27.0 / Python 3.14.7 / Node.js 26.8.1 / Java 25.0.2 / Docker 29.4.0 (OrbStack) machine as the previous pass, this time sharing the machine with other concurrent work, so treat single means as noisier than before. Startup itself is barely touched by this session's engine changes: `New()`/`LoadSeed()` calls `New()` once, so the route-table fix in [Two regressions found and fixed, one left open](#two-regressions-found-and-fixed-one-left-open) saves on the order of 60 µs here, well under this table's measurement noise (see [Write and sort performance](#write-and-sort-performance) and that section for what actually moved this round). The container rows (Docker/Testcontainers/Devbox) and the download-size section weren't re-run since they measure infrastructure this session didn't touch, and keep their 2026-09-13/14 figures.

| API path | Japanese | Mean startup | Range | Timing boundary |
|---|---|---:|---:|---|
| Go embedded | off | **2.02 ms** | 1.52–4.82 ms (n=10) | `osmem.New()` + `LoadSeed()`; the already-running Go test process and executable launch are excluded |
| Go embedded | on | **335.1 ms** | 313.3–404.1 ms (n=10) | Same; imports and enables `osmem/ja` |
| Python server SDK | off | **126.2 ms** | 18.2–549.9 ms (n=5) | `OsmemServer.start()` launches the child and waits for seeded-ready; Python runner already running |
| Python server SDK | on | **339.6 ms** | 327.0–358.3 ms (n=5) | Same, Japanese analyzer enabled |
| Java server SDK | off | **462.0 ms** | 371.0–816.7 ms (n=5) | `OsmemServer.start()` includes extracting the bundled executable from the classpath, then child startup and seed load |
| Java server SDK | on | **769.9 ms** | 725.7–837.0 ms (n=5) | Same, Japanese analyzer enabled |
| Node.js server SDK | off | **22.7 ms** | 10.9–68.3 ms (n=5) | `OsmemServer.start()` launches the child and waits for seeded-ready; Node runner already running |
| Node.js server SDK | on | **326.5 ms** | 320.3–346.2 ms (n=5) | Same, Japanese analyzer enabled |
| Docker OpenSearch | — | **6.07 s** | 5.99–6.31 s (n=5) | Warm `opensearchproject/opensearch:2.19.0`, linux/arm64; until successful `PUT /benchmark`; not re-run, see above |
| Testcontainers Go + OpenSearch | — | **6.48 s** | 5.77–8.22 s (n=5) | Testcontainers-Go 0.44.0, fresh container each run, cached image; 1 GiB tmpfs at `/usr/share/opensearch/data`; first run starts Ryuk; not re-run, see above |
| Devbox-managed OpenSearch | — | **7.94 s** | 6.85–10.63 s (n=5) | Devbox 0.17.5 `services up -b`; process-compose runs `docker run`; cached image; not re-run, see above |

For Python, Java, and Node.js, the language runtime is already alive before timing begins, matching a test runner calling the SDK; Java's classpath-extraction step and Python/Node's already-present executable are unchanged. The Go embedded measurement has no HTTP listener and excludes starting the Go test process; each of its n=10 trials is a fresh single-shot process, since Japanese analyzer initialization turned out to be a one-time cost per process (see below), not per call. Container rows have a different ready condition (`PUT /benchmark`) and are context, not a controlled comparison with the seeded osmem fixture. Write latency is omitted: suites are expected to reuse prebuilt indexes.

**New observation: Japanese analyzer initialization is a one-time cost per process.** Calling `osmem.New()` repeatedly after importing `osmem/ja` in the same process pays the kuromoji/kagome dictionary load only on the first call; every subsequent call was 0.75–1.0 ms in a quick check, the same range as Japanese-disabled. A Go test suite that imports `osmem/ja` and builds one base cluster in `TestMain`, as this project's own README does, pays the ~320 ms cost once per test binary run, not once per test.

## Clone creation for mutating tests

Clone an already-built Japanese-enabled seed, then measure only clone creation. The test case can mutate its fork without affecting the shared base; no write or cleanup time is included.

| API path | Mean clone creation | Samples |
|---|---:|---|
| Go embedded `Cluster.Clone()` | **0.8 µs** | 5 batches × 5,000 clones; seed setup outside timer |
| Python `server.clone()` | **215 µs** | 300 clones from one running seeded server |
| Java `server.clone()` | **507 µs** | 300 clones from one running seeded server |
| Node.js `server.clone()` | **1.77 ms** | 300 clones from one running seeded server |

Measured after the fixes in [Two regressions found and fixed, one left open](#two-regressions-found-and-fixed-one-left-open); the Go embedded row is now the fastest ever measured on this page (was 14.4 µs previously, 60.1 µs mid-session before the fix). The language SDK rows go through the same in-process `Clone()`, diluted by the local HTTP request and JSON decoding, so they improve less in relative terms (5–30% off the mid-session regression); Python and Java also land below their previous published measurement (240 µs, 583 µs), while Node.js improves but stays above its previous 1.63 ms. Closing the clone and any subsequent index/document mutation are outside the timer.

## Runtime memory and query observations

| Path | Memory at ready | Query | Conditions |
|---|---:|---:|---|
| osmem server, Japanese analyzer exercised | 161.8 MiB RSS | — | 5 processes; same seed; sample taken after a Japanese match query |
| Go in-process API | not measured | filtered search + sort + date histogram: 4.5 ms; exact term: ~45 µs; read-only clone + count: ~175 µs | 10,000-document fixture; `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=5`; median of five runs, measured after the fixes in [Two regressions found and fixed, one left open](#two-regressions-found-and-fixed-one-left-open); the bool-query row is still elevated versus the previous measurement (was 1.78 ms) for the reason explained there |
| osmem HTTP query, Japanese disabled | not measured | median 0.5 ms, p95 0.6 ms, p99 1.1 ms | 500 sequential `match_all`, size 10 requests against the 3-document seed; dominated by loopback HTTP and `curl` process overhead at this document count, not engine time |
| Docker OpenSearch | 942.2 MiB container RSS | median 4.45 ms, p95 9.76 ms, p99 19.02 ms | `docker stats` after test index creation; query sample from a separate run; not re-run, see [Startup by API](#startup-by-api) |
| Testcontainers Go + OpenSearch | 954.4 MiB container + 1.3 MiB runner RSS increase = **955.7 MiB** | — | Go runner: 19.0 MiB before startup, 20.3 MiB ready; 1 GiB data tmpfs; excludes Docker daemon and Ryuk; not re-run |
| Devbox-managed OpenSearch | 947.6 MiB container + 61.0 MiB process-compose/Docker CLI = **1,008.6 MiB** | — | Excludes Docker daemon; not re-run |

The query rows do not use the same index or query plan: the Go benchmark searches 10,000 documents, the osmem HTTP check uses a small seed, and OpenSearch searches an empty index. They show local path costs, not a controlled engine shootout. Docker uses one node, a 512 MiB heap, and disabled security demo setup. Hardware, heap, architecture, storage driver, image cache, and startup policy can change these results substantially.

## Testcontainers and Devbox services

Testcontainers is a test-side wrapper around a container runtime, not another search engine. This comparison uses the Go implementation (`testcontainers-go` 0.44.0) to avoid attributing a large, noisy JVM baseline to the container. Five fresh-container trials averaged 6.476 seconds to the index-ready request (5.774–8.223 seconds). OpenSearch used a 512 MiB heap with security disabled, and `/usr/share/opensearch/data` was mounted as a 1 GiB tmpfs with UID/GID 1000. The OpenSearch image was cached; the first trial also started Testcontainers' Ryuk helper. At readiness, `docker stats` reported 954.4 MiB of container memory. The Go runner averaged 19.0 MiB RSS before startup and 20.3 MiB when ready, so the 1.3 MiB increase gives a combined figure of 955.7 MiB. The Docker daemon and Ryuk are excluded. These tmpfs-backed figures use a different storage condition from the Docker and Devbox rows. A class-scoped Testcontainers container can amortize one startup across that class's tests. None of this changed since the previous measurement (2026-09-14); it is not re-run here.

Reproduce the Testcontainers measurement from the repository root:

```bash
cd bench/testcontainers
go run .
```

The harness times container creation through a successful `PUT /benchmark`, then verifies the tmpfs mount with `docker inspect`. It prints five fresh-container trials by default; pass a positive number to change the trial count.

Devbox can manage a service through process-compose; the [official services guide](https://www.jetify.com/docs/devbox/guides/services) describes `devbox services up` and background mode. Here, `devbox services up -b` starts the same cached OpenSearch image through a process-compose `docker run` service. Five fresh starts averaged 7.94 seconds to a successful `PUT /benchmark`. Once ready, the container averaged 947.6 MiB RSS and process-compose plus its persistent Docker CLI averaged 61.0 MiB, for 1,008.6 MiB total. The Docker daemon is excluded. The warm no-op `devbox run` measurement (156 ms) and first Maven/JDK closure download (194.1 MiB, 351.7 MiB unpacked) are separate development-toolchain costs—not server startup or server image size. None of this changed since the previous measurement; it is not re-run here.

## Linked application binary and container download size

For library size, this reports the **incremental size in the final linked Go executable**, not source-code or package/archive size. Three minimal Go programs were built on this machine on 2026-09-16 with Go 1.27.0, `-trimpath`, and `-ldflags=-buildid=`: an empty `main`, one that constructs and closes an osmem cluster, and one that also imports the Japanese analyzer. The optional analyzer measurement is cumulative from the empty baseline; its incremental addition over osmem alone is also shown.

| Go executable | Size |
|---|---:|
| Minimal `main` baseline | 1,815,298 bytes (1.73 MiB) |
| osmem-linked executable | 32,771,522 bytes (31.26 MiB); **+30,956,224 bytes (+29.52 MiB)** |
| osmem + Japanese analyzer | 45,622,018 bytes (43.51 MiB); **+43,806,720 bytes (+41.78 MiB)** vs baseline, of which the analyzer adds **12,850,496 bytes (12.25 MiB)** |

The osmem-linked increment grew by about 6.8 MB since the previous
measurement (was 23.85 MB / 22.75 MiB): `internal/engine/aggs_dates.go` now
blank-imports `time/tzdata` to embed Go's IANA timezone database
unconditionally—the fix for a windows-latest CI hang traced to
platform-dependent zone data—plus the OpenSearch 3.8 compatibility and
segment-merge/sort work added a substantial amount of engine code in the
same session. The Japanese analyzer's own incremental cost is essentially
unchanged (12.25 MiB vs 12.26 MiB previously): it depends on the kagome/IPADIC
dictionary, which this round did not touch.

These are toolchain- and program-dependent linker results, not a universal package-size guarantee. Node.js and Python use a separate osmem child executable, and Java uses JVM artifacts, so their integration does not have a comparable statically linked application-binary delta.

The homepage puts three sizes on one decimal-MB scale: the linked Go app's **43.8 MB** increment (**41.78 MiB**), the first Devbox Maven/JDK environment download (**203.5 MB**, or 194.1 MiB), and the OpenSearch image's **739.3 MB** compressed `linux/arm64` size listed by [Docker Hub](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0). These are deliberately distinct scopes: a linked executable delta, a one-time toolchain download, and a complete compressed server image. Docker's local `Size` is expanded storage and is not used as a download-size proxy; actual transfer can be lower when layers are already cached.

Testcontainers uses that same OpenSearch image; it does not link a second search binary into the test application. Devbox's 194.1 MiB figure is the downloaded Maven/JDK environment closure, a separate setup cost rather than an application binary size.

## Reproduce the Go numbers

Run from the repository root:

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k|SingleDocWrites|Sort100k)$' -benchmem -count=3
```

The benchmarks build their document fixture (10,000 documents for
`CloneReadOnly`/`Search10k`/`TermQuery10k`, 100,000 for `Sort100k`) before
timing the query cases; `SingleDocWrites` indexes its own 1,000 or 5,000
documents one at a time as part of what it measures. For service startup
and client-transport measurements, record whether compilation, binary
extraction, image pulling, and seed loading are inside or outside the
timer. To compare a change against a baseline the way this page's
before/after tables do, build both commits with `git worktree add` and run
the same `-bench` command back to back on the same machine—single-point
measurements on a shared or otherwise busy machine vary enough (see
[Startup by API](#startup-by-api)) that a same-session, same-machine
comparison is more trustworthy than diffing two dated table rows. The same
technique, repeated across several intermediate commits, is how [the two
regressions above](#two-regressions-found-and-fixed-one-left-open) were
traced to specific commits rather than "somewhere in the last three days."
