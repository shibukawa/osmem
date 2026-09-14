---
title: "Performance and footprint"
description: "Measured startup, process and container memory, linked application size, Docker, Testcontainers, and Devbox costs, with conditions and limits."
---

Japanese analysis makes a visible difference: loading the seed took Go embedded from 2.13 ms to 319.7 ms. SDK launch adds another boundary—child-process creation, and for Java, extracting the bundled executable from the classpath. These are local measurements, not guarantees.

## Startup by API

Most measurements were made on 2026-09-13 on an Apple M3, macOS arm64, Go 1.27.0, Python 3.14.7, Node.js 26.8.1, Java 25.0.2, and Docker 29.4.0 with OrbStack. The Testcontainers tmpfs run was repeated on 2026-09-14 with the same Go and Docker versions. Every osmem path loads the same 525-byte seed (3 documents across 2 indices). Japanese enabled means the seed's kuromoji analyzer is initialized; disabled uses the CJK fallback.

| API path | Japanese | Mean startup | Range | Timing boundary |
|---|---|---:|---:|---|
| Go embedded | off | **2.13 ms** | 1.54–2.87 ms (n=10) | `osmem.New()` + `LoadSeed()`; the already-running Go test process and executable launch are excluded |
| Go embedded | on | **319.7 ms** | 312.0–347.8 ms (n=10) | Same; imports and enables `osmem/ja` |
| Python server SDK | off | **23.5 ms** | 10.1–69.4 ms (n=5) | `OsmemServer.start()` launches the child and waits for seeded-ready; Python runner already running |
| Python server SDK | on | **332.8 ms** | 321.1–366.8 ms (n=5) | Same, Japanese analyzer enabled |
| Java server SDK | off | **435.2 ms** | 305–868 ms (n=5) | `OsmemServer.start()` includes extracting the bundled executable from the classpath, then child startup and seed load |
| Java server SDK | on | **730.4 ms** | 653–797 ms (n=5) | Same, Japanese analyzer enabled |
| Node.js server SDK | off | **10.8 ms** | 10.1–12.8 ms (n=5) | `OsmemServer.start()` launches the child and waits for seeded-ready; Node runner already running |
| Node.js server SDK | on | **325.1 ms** | 318.6–344.1 ms (n=5) | Same, Japanese analyzer enabled |
| Docker OpenSearch | — | **6.07 s** | 5.99–6.31 s (n=5) | Warm `opensearchproject/opensearch:2.19.0`, linux/arm64; until successful `PUT /benchmark` |
| Testcontainers Go + OpenSearch | — | **6.48 s** | 5.77–8.22 s (n=5) | Testcontainers-Go 0.44.0, fresh container each run, cached image; 1 GiB tmpfs at `/usr/share/opensearch/data`; first run starts Ryuk |
| Devbox-managed OpenSearch | — | **7.94 s** | 6.85–10.63 s (n=5) | Devbox 0.17.5 `services up -b`; process-compose runs `docker run`; cached image |

For Python, Java, and Node.js, the language runtime is already alive before timing begins, matching a test runner calling the SDK. The server binary is built before the timer; Java follows the default classpath-resource extraction path and copies the bundled executable to a temporary file on each start, while Python and Node use an already-present executable. One Java no-Japanese trial is a high outlier, retained in the mean. The Go embedded measurement has no HTTP listener and excludes starting the Go test process. Container rows have a different ready condition (`PUT /benchmark`) and are context, not a controlled comparison with the seeded osmem fixture.

Write latency is omitted: suites are expected to reuse prebuilt indexes, and per-test writes are not the startup cost this comparison is meant to explain.

## Clone creation for mutating tests

Clone an already-built Japanese-enabled seed, then measure only clone creation. The test case can mutate its fork without affecting the shared base; no write or cleanup time is included.

| API path | Mean clone creation | Samples |
|---|---:|---|
| Go embedded `Cluster.Clone()` | **14.4 µs** | 5 batches × 5,000 clones; seed setup outside timer |
| Python `server.clone()` | **240 µs** | 300 clones from one running seeded server |
| Java `server.clone()` | **583 µs** | 300 clones from one running seeded server |
| Node.js `server.clone()` | **1.63 ms** | 300 clones from one running seeded server |

The language SDK rows include the local HTTP management request and JSON decoding; the Go embedded row is an in-process copy-on-write fork without a port. Closing the clone and any subsequent index/document mutation are outside the timer. Outliers from process scheduling make the detailed clone samples noisier than the averages imply.

## Runtime memory and query observations

| Path | Memory at ready | Query | Conditions |
|---|---:|---:|---|
| osmem server, Japanese analyzer exercised | 160.4 MiB RSS | — | 5 processes; same seed; sample taken after a Japanese match query |
| Go in-process API | not measured | filtered search + sort + date histogram: 1.71 ms; exact term: 33.3 µs; read-only clone + count: 270 µs | 10,000-document fixture; `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`; median of three runs |
| osmem HTTP query, Japanese disabled | not measured | median 1.53 ms, p95 1.84 ms, p99 2.94 ms | 500 sequential `match_all`, size 10 requests |
| Docker OpenSearch | 942.2 MiB container RSS | median 4.45 ms, p95 9.76 ms, p99 19.02 ms | `docker stats` after test index creation; query sample from a separate run |
| Testcontainers Go + OpenSearch | 954.4 MiB container + 1.3 MiB runner RSS increase = **955.7 MiB** | — | Go runner: 19.0 MiB before startup, 20.3 MiB ready; 1 GiB data tmpfs; excludes Docker daemon and Ryuk |
| Devbox-managed OpenSearch | 947.6 MiB container + 61.0 MiB process-compose/Docker CLI = **1,008.6 MiB** | — | Excludes Docker daemon |

The query rows do not use the same index or query plan: the Go benchmark searches 10,000 documents, the osmem HTTP check uses a small seed, and OpenSearch searches an empty index. They show local path costs, not a controlled engine shootout. Docker uses one node, a 512 MiB heap, and disabled security demo setup. Hardware, heap, architecture, storage driver, image cache, and startup policy can change these results substantially.

## Testcontainers and Devbox services

Testcontainers is a test-side wrapper around a container runtime, not another search engine. This comparison uses the Go implementation (`testcontainers-go` 0.44.0) to avoid attributing a large, noisy JVM baseline to the container. Five fresh-container trials averaged 6.476 seconds to the index-ready request (5.774–8.223 seconds). OpenSearch used a 512 MiB heap with security disabled, and `/usr/share/opensearch/data` was mounted as a 1 GiB tmpfs with UID/GID 1000. The OpenSearch image was cached; the first trial also started Testcontainers' Ryuk helper. At readiness, `docker stats` reported 954.4 MiB of container memory. The Go runner averaged 19.0 MiB RSS before startup and 20.3 MiB when ready, so the 1.3 MiB increase gives a combined figure of 955.7 MiB. The Docker daemon and Ryuk are excluded. These tmpfs-backed figures use a different storage condition from the Docker and Devbox rows. A class-scoped Testcontainers container can amortize one startup across that class's tests.

Reproduce the Testcontainers measurement from the repository root:

```bash
cd bench/testcontainers
go run .
```

The harness times container creation through a successful `PUT /benchmark`, then verifies the tmpfs mount with `docker inspect`. It prints five fresh-container trials by default; pass a positive number to change the trial count.

Devbox can manage a service through process-compose; the [official services guide](https://www.jetify.com/docs/devbox/guides/services) describes `devbox services up` and background mode. Here, `devbox services up -b` starts the same cached OpenSearch image through a process-compose `docker run` service. Five fresh starts averaged 7.94 seconds to a successful `PUT /benchmark`. Once ready, the container averaged 947.6 MiB RSS and process-compose plus its persistent Docker CLI averaged 61.0 MiB, for 1,008.6 MiB total. The Docker daemon is excluded. The warm no-op `devbox run` measurement (156 ms) and first Maven/JDK closure download (194.1 MiB, 351.7 MiB unpacked) are separate development-toolchain costs—not server startup or server image size.

## Linked application binary and container download size

For library size, this reports the **incremental size in the final linked Go executable**, not source-code or package/archive size. Three minimal Go programs were built on this machine with Go 1.27.0, `-trimpath`, and `-ldflags=-buildid=`: an empty `main`, one that constructs and closes an osmem cluster, and one that also imports the Japanese analyzer. The optional analyzer measurement is cumulative from the empty baseline; its incremental addition over osmem alone is also shown.

| Go executable | Size |
|---|---:|
| Minimal `main` baseline | 1,815,314 bytes (1.73 MiB) |
| osmem-linked executable | 25,668,722 bytes (24.48 MiB); **+23,853,408 bytes (+22.75 MiB)** |
| osmem + Japanese analyzer | 38,529,298 bytes (36.74 MiB); **+36,713,984 bytes (+35.01 MiB)** vs baseline, of which the analyzer adds **12,860,576 bytes (12.26 MiB)** |

These are toolchain- and program-dependent linker results, not a universal package-size guarantee. Node.js and Python use a separate osmem child executable, and Java uses JVM artifacts, so their integration does not have a comparable statically linked application-binary delta.

The homepage puts three sizes on one decimal-MB scale: the linked Go app's **36.7 MB** increment (**35.0 MiB**), the first Devbox Maven/JDK environment download (**203.5 MB**, or 194.1 MiB), and the OpenSearch image's **739.3 MB** compressed `linux/arm64` size listed by [Docker Hub](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0). These are deliberately distinct scopes: a linked executable delta, a one-time toolchain download, and a complete compressed server image. Docker's local `Size` is expanded storage and is not used as a download-size proxy; actual transfer can be lower when layers are already cached.

Testcontainers uses that same OpenSearch image; it does not link a second search binary into the test application. Devbox's 194.1 MiB figure is the downloaded Maven/JDK environment closure, a separate setup cost rather than an application binary size.

## Reproduce the Go numbers

Run from the repository root:

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

The benchmarks build their 10,000-document fixture before timing the query cases. For service startup and client-transport measurements, record whether compilation, binary extraction, image pulling, and seed loading are inside or outside the timer.
