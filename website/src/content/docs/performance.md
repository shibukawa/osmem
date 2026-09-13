---
title: "Performance and footprint"
description: "Measured startup, resident memory, linked application size, Docker, Testcontainers, and Devbox costs, with conditions and limits."
---

One fresh container per test and one copy-on-write fork per test do not pay for the same thing. The averages below separate server readiness, resident memory, and wrapper overhead; they are local measurements, not universal guarantees.

## Local measurements

Measured on 2026-09-13 on an Apple M3, macOS arm64, Go 1.27.0, Node.js 26.8.1, Docker 29.4.0 with OrbStack, and an 8-core Linux arm64 container with a 512 MiB Java heap. Times are wall-clock observations from one machine, not release guarantees.

| Path | Startup / readiness average | Resident memory average | Query | Conditions |
|---|---:|---:|---:|---|
| `osmem-server`, Japanese enabled | 680 ms | 160.4 MiB RSS | — | 5 process starts; 525-byte seed, 3 documents across 2 indices; a Japanese match query ran before the RSS sample |
| `osmem-server`, Japanese disabled | first launch 83 ms; later launches 8.8–13.0 ms | not measured | median 1.53 ms, p95 1.84 ms, p99 2.94 ms | 12 starts; same 525-byte seed; 500 sequential HTTP `match_all`, size 10 requests |
| Go in-process API | read-only clone + count: 270 µs | not measured | filtered search + sort + date histogram: 1.71 ms; exact term: 33.3 µs | `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`; median of three runs |
| Docker OpenSearch | 6.07 s to successful `PUT /benchmark` | 942.2 MiB container RSS | median 4.45 ms, p95 9.76 ms, p99 19.02 ms | 5 warm-image runs; `opensearchproject/opensearch:2.19.0`, linux/arm64, single node, 512 MiB heap; query sample from a separate run |
| Testcontainers + OpenSearch | 8.12 s to successful `PUT /benchmark` | 941.9 MiB container + 88.7 MiB test JVM RSS | — | Testcontainers Java 2.0.5; 5 sequential fresh containers in one Maven/JUnit process; image already pulled |

The query rows do not use the same index or query plan: the osmem HTTP check uses a small seed, the Go benchmark searches 10,000 documents, and the OpenSearch check searches an empty index. They show local path costs, not a controlled engine shootout. Write latency is omitted: these test suites are expected to reuse prebuilt indexes, and write time is not the cost this comparison is meant to explain.

The osmem RSS sample includes the running Go process after the Japanese analyzer has been exercised. Docker RSS comes from `docker stats` after the test index is created. Direct Docker readiness is timed from `docker run` through successful `PUT /benchmark`; the image was warm, single-node, and security demo setup was disabled. Docker and Testcontainers differ slightly in their readiness implementation. Hardware, heap, architecture, storage driver, image cache, and startup policy can change these results substantially.

## Testcontainers and Devbox

Testcontainers is a test-side wrapper around a container runtime, not another search engine. With a warm image and a fresh container for each of five trials, the Java Testcontainers path averaged 8.12 seconds to the same index-ready request. Its OpenSearch container used about the same memory as direct Docker; the test JVM added another 88.7 MiB RSS. The roughly two-second gap from the direct-Docker run is an observed difference between these two harnesses, not a pure library-only overhead measurement. A class-scoped Testcontainers container can amortize one startup across that class's tests.

Devbox is an environment manager rather than a server runtime. A temporary Devbox 0.17.5 environment using Maven 3.9.16 took an average of 156 ms to run a warm no-op command (six trials). Its initial Nix package closure required 194.1 MiB of downloads and 351.7 MiB unpacked. That cost is paid when preparing the development environment or launching a `devbox run` command; Devbox does not start OpenSearch, so its memory is not a server-footprint comparison. In a real test suite, the command wrapper is usually paid once per suite, not once per case.

## Linked application binary and container download size

For library size, this reports the **incremental size in the final linked Go executable**, not source-code or package/archive size. Three minimal Go programs were built on this machine with Go 1.27.0, `-trimpath`, and `-ldflags=-buildid=`: an empty `main`, one that constructs and closes an osmem cluster, and one that also imports the Japanese analyzer. The optional analyzer measurement is cumulative from the empty baseline; its incremental addition over osmem alone is also shown.

| Go executable | Size |
|---|---:|
| Minimal `main` baseline | 1,815,314 bytes (1.73 MiB) |
| osmem-linked executable | 25,668,722 bytes (24.48 MiB); **+23,853,408 bytes (+22.75 MiB)** |
| osmem + Japanese analyzer | 38,529,298 bytes (36.74 MiB); **+36,713,984 bytes (+35.01 MiB)** vs baseline, of which the analyzer adds **12,860,576 bytes (12.26 MiB)** |

These are toolchain- and program-dependent linker results, not a universal package-size guarantee. Node.js and Python use a separate osmem child executable, and Java uses JVM artifacts, so their integration does not have a comparable statically linked application-binary delta.

The OpenSearch container is a separate download-footprint measure: Docker Hub lists the compressed `linux/arm64` image size as [739.3 MB](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0). Docker's local `Size` is expanded storage and is not used as a download-size proxy; actual transfer can be lower when layers are already cached.

Testcontainers uses that same OpenSearch image; it does not link a second search binary into the test application. Devbox's 194.1 MiB figure is the downloaded Maven/JDK environment closure, a separate setup cost rather than an application binary size.

## Reproduce the Go numbers

Run from the repository root:

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

The benchmarks build their 10,000-document fixture before timing the query cases. For service startup and client-transport measurements, record whether compilation, binary extraction, image pulling, and seed loading are inside or outside the timer.
