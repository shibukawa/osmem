---
title: "Performance and footprint"
description: "Local measurements of osmem startup, queries, linked application binary size, and Docker OpenSearch, with conditions and limits."
---

The useful comparison is not a single speed number. Starting a search engine, registering a schema, making one request in a warm process, and forking a dataset are different costs. The measurements below keep those operations separate.

## Local measurements

Measured on 2026-09-13 on an Apple M3, macOS arm64, Go 1.27.0, Node.js 26.8.1, Docker 29.4.0 with OrbStack, and an 8-core Linux arm64 container with a 512 MiB Java heap. Times are wall-clock observations from one machine, not release guarantees.

| Path | Setup / readiness | Query | Conditions |
|---|---:|---:|---|
| `osmem-server` child process | first launch 83 ms; subsequent launches 8.8–13.0 ms | median 1.53 ms, p95 1.84 ms, p99 2.94 ms | 12 starts; `--no-ja`, 525-byte seed files with 6 documents; 500 sequential HTTP `match_all`, size 10 requests |
| Go in-process API | read-only clone + count: 270 µs | filtered search + sort + date histogram: 1.71 ms; exact term: 33.3 µs | `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`; median of the three runs |
| Docker OpenSearch | 9.44 s until index creation succeeds | median 4.45 ms, p95 9.76 ms, p99 19.02 ms | `opensearchproject/opensearch:2.19.0`, linux/arm64, single node, 512 MiB heap; 500 sequential searches of an empty index |

The query rows do not use the same index or query plan: the osmem HTTP test searches six seeded documents, the Go benchmark searches 10,000, and the OpenSearch check searches an empty index. They show the scale of the local paths, not a controlled engine shootout. Write latency is omitted: these test suites are expected to reuse prebuilt indexes, and write time is not the cost this comparison is meant to explain.

Docker readiness is defined as the time from `docker run` to a successful `PUT /benchmark` request, not the container process start. The image ran with the security demo setup disabled. A production-like configuration, a different heap, architecture, storage driver, or host can change this result substantially.

## Testcontainers and Devbox

Testcontainers is a test-side wrapper around a container runtime, not another search engine. This repository has no Testcontainers fixture or Java build tool installed in the measurement environment, so its extra orchestration time was not measured separately. The Docker row is the underlying OpenSearch container baseline; a Testcontainers result should include the library version, runtime, image pull/cache state, and its readiness strategy.

Devbox is an environment manager rather than a server runtime. Devbox 0.17.5 was installed, but this repository has no `devbox.json`, so there is no configured Devbox environment or dependency download to time. It can pin tools for a reproducible benchmark; it does not replace Docker or start OpenSearch by itself.

## Linked application binary and container download size

For library size, this reports the **incremental size in the final linked Go executable**, not source-code or package/archive size. Three minimal Go programs were built on this machine with Go 1.27.0, `-trimpath`, and `-ldflags=-buildid=`: an empty `main`, one that constructs and closes an osmem cluster, and one that also imports the Japanese analyzer. The optional analyzer measurement is cumulative from the empty baseline; its incremental addition over osmem alone is also shown.

| Go executable | Size |
|---|---:|
| Minimal `main` baseline | 1,815,314 bytes (1.73 MiB) |
| osmem-linked executable | 25,668,722 bytes (24.48 MiB); **+23,853,408 bytes (+22.75 MiB)** |
| osmem + Japanese analyzer | 38,529,298 bytes (36.74 MiB); **+36,713,984 bytes (+35.01 MiB)** vs baseline, of which the analyzer adds **12,860,576 bytes (12.26 MiB)** |

These are toolchain- and program-dependent linker results, not a universal package-size guarantee. Node.js and Python use a separate osmem child executable, and Java uses JVM artifacts, so their integration does not have a comparable statically linked application-binary delta.

The OpenSearch container is a separate download-footprint measure: Docker Hub lists the compressed `linux/arm64` image size as [739.3 MB](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0). Docker's local `Size` is expanded storage and is not used as a download-size proxy; actual transfer can be lower when layers are already cached.

## Reproduce the Go numbers

Run from the repository root:

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

The benchmarks build their 10,000-document fixture before timing the query cases. For service startup and client-transport measurements, record whether compilation, binary extraction, image pulling, and seed loading are inside or outside the timer.
