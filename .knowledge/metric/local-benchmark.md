---
id: metric:local-benchmark
type: metric
title: Local Startup, Query, and Linked Binary Measurements
---
Single-machine observations from 2026-09-13. Values describe distinct workloads and must not be presented as a controlled engine ranking.

```yaml
host:
  machine: Apple M3
  os: macOS arm64
  go: 1.27.0
  node: 26.8.1
  docker: 29.4.0 OrbStack
  container: linux/arm64, 8 cores, 512 MiB Java heap
workloads:
  osmem_child:
    starts: 12
    first_ready_ms: 83
    later_ready_ms_range: [8.8, 13.0]
    seed: 3 docs across 2 indices, 525 bytes, Japanese disabled
    sequential_http_query: 500 x match_all size 10
    latency_ms: {median: 1.53, p95: 1.84, p99: 2.94}
  osmem_child_ja_default:
    trials: 5
    seed: internal/serve/testdata/seed, 525 bytes, 3 docs across 2 indices
    japanese: enabled; seed mapping uses kuromoji and Japanese match query is issued before RSS sample
    ready_ms_avg: 680
    ready_ms_range: [501, 1238]
    process_rss_mib_avg: 160.4
    process_rss_mib_range: [160.0, 161.2]
  go_in_process_10k:
    repetitions: 3
    aggregation_search_ms: 1.71
    exact_term_us: 33.3
    readonly_clone_and_count_us: 270
  docker_opensearch_2_19_arm64:
    trials: 5
    image_state: pulled and cached before trials
    readiness: successful PUT /benchmark after docker run
    ready_ms_avg: 6074
    ready_ms_range: [5985, 6312]
    container_rss_mib_avg: 942.2
    query: 500 sequential match_all size 10 requests on empty index
    latency_ms: {median: 4.45, p95: 9.76, p99: 19.02}
  testcontainers_opensearch:
    framework: org.testcontainers:testcontainers 2.0.5
    runner: Maven 3.9.16, JUnit 5.12.2, host Java 25.0.2
    trials: 5 containers started sequentially in one test JVM; image cached
    readiness: GenericContainer HTTP GET / returns 200, then successful PUT /benchmark
    container_start_ms_avg: 7892
    put_setup_ms_avg: 229
    ready_ms_avg: 8121
    container_rss_mib_avg: 941.9
    java_test_process_rss_mib_avg: 88.7
  devbox_maven_environment:
    devbox: 0.17.5
    packages: maven 3.9.16; Nix closure includes Zulu JDK 21.0.11
    warm_devbox_run_trials: 6
    warm_devbox_run_ms_avg: 156
    warm_devbox_run_ms_range: [148, 183]
    first_install_download_mib: 194.1
    nix_store_unpacked_mib: 351.7
    note: Maven used host Java 25.0.2; Devbox is environment setup, not a search server
linked_go_executable_bytes:
  toolchain: go1.27.0 darwin/arm64
  flags: [-trimpath, -ldflags=-buildid=]
  minimal_main_baseline: 1815314
  osmem_main_total: 25668722
  osmem_increment_over_baseline: 23853408
  osmem_ja_main_total: 38529298
  osmem_ja_increment_over_baseline: 36713984
  ja_increment_over_osmem: 12860576
  method: "Compare final executable sizes for minimal main, osmem.New()+Close(), and osmem plus blank import of osmem/ja"
  scope: "Linked executable delta; not source, module, package, or archive size"
docker_download_size:
  opensearch_image_docker_hub_compressed_mb: 739.3
  docker_hub_tag_url: https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0
  docker_hub_size_checked: 2026-09-13
limits:
  - OpenSearch query index differs from osmem fixtures; no controlled engine comparison
  - write latency omitted; tests are expected to reuse prebuilt indices and startup is the comparison focus
  - Docker Hub compressed-size total is distinct from bytes actually transferred when layers are cached
  - Node.js and Python use a child executable and Java uses JVM artifacts; linked Go executable delta is not comparable to them
  - Testcontainers trial starts a fresh container each repetition; class-scoped container reuse amortizes this startup across tests
  - Docker and Testcontainers readiness differ slightly: direct Docker polls the root HTTP endpoint before PUT; Testcontainers uses its HTTP wait strategy before PUT
  - Devbox warm command overhead and Nix closure size are environment-manager costs, not per-test server startup or memory
  - Testcontainers JVM RSS excludes Maven parent, Devbox shell, Ryuk helper, and Docker daemon
```
