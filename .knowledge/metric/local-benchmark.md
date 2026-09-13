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
  python: 3.14.7
  node: 26.8.1
  java: 25.0.2
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
  osmem_server_rss_ja:
    trials: 5
    seed: internal/serve/testdata/seed, 525 bytes, 3 docs across 2 indices
    japanese: enabled; seed mapping uses kuromoji and a Japanese match query is issued before RSS sample
    process_rss_mib_avg: 160.4
    process_rss_mib_range: [160.0, 161.2]
  osmem_startup_api:
    seed: internal/serve/testdata/seed, 525 bytes, 3 docs across 2 indices
    timer: >-
      Go: osmem.New()+LoadSeed(); Python/Java/Node: SDK start() through child server readiness; language runner startup excluded
    go_embedded_no_ja:
      trials: 10
      mean_ms: 2.1337
      range_ms: [1.536, 2.871]
    go_embedded_ja:
      trials: 10
      mean_ms: 319.7339
      range_ms: [312.027, 347.827]
    python_server_no_ja:
      trials: 5
      mean_ms: 23.46
      range_ms: [10.1, 69.4]
    python_server_ja:
      trials: 5
      mean_ms: 332.82
      range_ms: [321.1, 366.8]
    java_server_no_ja:
      trials: 5
      mean_ms: 435.2
      range_ms: [305, 868]
      includes: default classpath binary extraction and child startup
      samples_ms: [868, 320, 341, 342, 305]
    java_server_ja:
      trials: 5
      mean_ms: 730.4
      range_ms: [653, 797]
      includes: default classpath binary extraction and child startup
      samples_ms: [653, 776, 724, 702, 797]
    node_server_no_ja:
      trials: 5
      mean_ms: 10.78
      range_ms: [10.1, 12.8]
    node_server_ja:
      trials: 5
      mean_ms: 325.12
      range_ms: [318.6, 344.1]
    readiness: SDK paths load the seed and wait for osmem-server ready line; container comparisons wait for successful PUT /benchmark
  clone_creation:
    seed: same 525-byte fixture; Japanese enabled; base creation outside timer
    go_embedded:
      batches: 5
      clones_per_batch: 5000
      batch_mean_us: [14.938, 13.899, 15.081, 14.064, 14.183]
      mean_us: 14.433
      timed: Cluster.Clone() call only; close excluded
    python_server:
      clones: 300
      mean_us: 240.29
      median_us: 167.98
      p95_us: 282.25
      range_us: [136.67, 16360.21]
    java_server:
      clones: 300
      mean_us: 583.05
      median_us: 331.63
      p95_us: 725.17
      range_us: [202.71, 63650.79]
    node_server:
      clones: 300
      mean_us: 1632.38
      median_us: 1500.58
      p95_us: 1774.87
      range_us: [250.04, 43157.17]
    timed: SDK clone creation includes localhost HTTP management request and response decode; clone close and mutations excluded
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
  testcontainers_opensearch_go:
    framework: github.com/testcontainers/testcontainers-go v0.44.0
    runner: Go 1.27.0 darwin/arm64
    trials: 5 sequential fresh containers; cached OpenSearch image and shared warm Ryuk helper after trial 1
    readiness: Testcontainers HTTP GET / returns 200, then successful PUT /benchmark
    ready_ms_avg: 6262
    ready_ms_range: [5793, 6886]
    first_trial_includes: cold Ryuk helper startup
    container_rss_mib_avg: 952.9
    runner_rss_before_mib_avg: 15.3
    runner_rss_ready_mib_avg: 19.9
    runner_rss_delta_mib_avg: 4.5
    container_plus_runner_delta_mib_avg: 957.5
  devbox_services_opensearch:
    devbox: 0.17.5
    command: devbox services up -b; process-compose service invokes docker run
    service: opensearchproject/opensearch:2.19.0, linux/arm64, single-node, 512 MiB heap
    trials: 5 fresh service starts; image cached
    readiness: successful PUT /benchmark after Devbox command invocation
    ready_ms_avg: 7942
    ready_ms_range: [6847, 10632]
    container_rss_mib_avg: 947.6
    process_compose_and_docker_cli_rss_mib_avg: 61.0
    total_service_rss_mib_avg: 1008.6
  devbox_maven_environment:
    devbox: 0.17.5
    packages: maven 3.9.16; Nix closure includes Zulu JDK 21.0.11
    warm_devbox_run_trials: 6
    warm_devbox_run_ms_avg: 156
    warm_devbox_run_ms_range: [148, 183]
    first_install_download_mib: 194.1
    first_install_download_decimal_mb: 203.5
    nix_store_unpacked_mib: 351.7
    note: Maven used host Java 25.0.2; this toolchain download is separate from the Devbox-managed OpenSearch service
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
  - Testcontainers Go runner memory reports only its RSS increase from immediately before container startup; container RSS is added to that delta
  - Testcontainers total excludes the Docker daemon and Ryuk helper container; its first startup trial includes the cold Ryuk helper
  - Devbox service uses process-compose to invoke docker run; its total RSS includes process-compose and the persistent Docker CLI, but excludes the Docker daemon
  - Devbox first Maven/JDK closure download is a one-time toolchain cost; the service-start trials use an already-pulled OpenSearch image
  - Docker, Testcontainers, and Devbox server readiness differ slightly in orchestration, but all finish after a successful PUT /benchmark
  - Go embedded and language-SDK startup paths have different boundaries: Go starts from an already-running test process, SDK launch includes child process creation, and Java also includes default classpath binary extraction
  - SDK startup samples run from already-running language processes; first-trial latency outliers remain included in the mean
  - Clone measurements use an already-seeded base and exclude writes; server SDK clones include localhost HTTP and JSON overhead, while Go Cluster.Clone is in-process
```
