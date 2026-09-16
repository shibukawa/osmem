---
id: metric:local-benchmark
type: metric
title: Local Startup, Query, and Linked Binary Measurements
---
Single-machine observations, mostly from 2026-09-13, with the Testcontainers tmpfs run repeated on 2026-09-14. Re-measured 2026-09-16 (see `remeasured_2026_09_16` fields below) on the same machine, this time shared with other concurrent sessions, after an OpenSearch 3.8 compatibility pass, decision:in-memory-segment-merge, decision:sort-execution, decision:shared-route-table and decision:nested-filter-scoping; the container/Docker-image rows were not re-run (no code in this repo touches them). Values describe distinct workloads and must not be presented as a controlled engine ranking.

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
    remeasured_2026_09_16:
      binary: 45.7 MB server binary (up from ~38.5 MB), post decision:shared-route-table and decision:nested-filter-scoping
      latency_ms: {median: 0.5, p95: 0.6, p99: 1.1}
      note: dominated by loopback HTTP + curl process overhead at this document count, not engine time; starts/ready_ms not re-measured
  osmem_server_rss_ja:
    trials: 5
    seed: internal/serve/testdata/seed, 525 bytes, 3 docs across 2 indices
    japanese: enabled; seed mapping uses kuromoji and a Japanese match query is issued before RSS sample
    process_rss_mib_avg: 160.4
    process_rss_mib_range: [160.0, 161.2]
    remeasured_2026_09_16:
      process_rss_mib_samples: [163.0, 154.3, 164.1, 163.5, 164.2]
      process_rss_mib_avg: 161.8
  osmem_startup_api:
    seed: internal/serve/testdata/seed, 525 bytes, 3 docs across 2 indices
    timer: >-
      Go: osmem.New()+LoadSeed(); Python/Java/Node: SDK start() through child server readiness; language runner startup excluded
    note: >-
      remeasured_2026_09_16 sub-fields below are fresh n=10 (Go)/n=5 (SDK) trials on a busier
      shared machine; startup itself is barely touched by the 2026-09-16 fixes (New() calls the
      route-table build once), so differences from the fields above are mostly machine noise,
      not code changes
    go_embedded_no_ja:
      trials: 10
      mean_ms: 2.1337
      range_ms: [1.536, 2.871]
      remeasured_2026_09_16: {trials: 10, mean_ms: 2.018, range_ms: [1.521, 4.815]}
    go_embedded_ja:
      trials: 10
      mean_ms: 319.7339
      range_ms: [312.027, 347.827]
      remeasured_2026_09_16: {trials: 10, mean_ms: 335.057, range_ms: [313.330, 404.076], note: "first New() after import osmem/ja pays the kagome dictionary load; later calls in the same process cost 0.75-1.0ms, same as ja-off"}
    python_server_no_ja:
      trials: 5
      mean_ms: 23.46
      range_ms: [10.1, 69.4]
      remeasured_2026_09_16: {trials: 5, mean_ms: 126.19, range_ms: [18.2, 549.9], note: "high outlier is the first fresh exec of the (now larger) server binary in the batch, retained in the mean per this page's convention"}
    python_server_ja:
      trials: 5
      mean_ms: 332.82
      range_ms: [321.1, 366.8]
      remeasured_2026_09_16: {trials: 5, mean_ms: 339.61, range_ms: [327.0, 358.3]}
    java_server_no_ja:
      trials: 5
      mean_ms: 435.2
      range_ms: [305, 868]
      includes: default classpath binary extraction and child startup
      samples_ms: [868, 320, 341, 342, 305]
      remeasured_2026_09_16: {trials: 5, mean_ms: 461.96, range_ms: [371.0, 816.7], samples_ms: [816.7, 373.6, 371.0, 371.6, 376.9]}
    java_server_ja:
      trials: 5
      mean_ms: 730.4
      range_ms: [653, 797]
      includes: default classpath binary extraction and child startup
      samples_ms: [653, 776, 724, 702, 797]
      remeasured_2026_09_16: {trials: 5, mean_ms: 769.9, range_ms: [725.7, 837.0], samples_ms: [837.0, 786.2, 760.8, 725.7, 739.8]}
    node_server_no_ja:
      trials: 5
      mean_ms: 10.78
      range_ms: [10.1, 12.8]
      remeasured_2026_09_16: {trials: 5, mean_ms: 22.74, range_ms: [10.9, 68.3], note: "high outlier is the first fresh exec in the batch"}
    node_server_ja:
      trials: 5
      mean_ms: 325.12
      range_ms: [318.6, 344.1]
      remeasured_2026_09_16: {trials: 5, mean_ms: 326.51, range_ms: [320.3, 346.2]}
    readiness: SDK paths load the seed and wait for osmem-server ready line; container comparisons wait for successful PUT /benchmark
  clone_creation:
    seed: same 525-byte fixture; Japanese enabled; base creation outside timer
    go_embedded:
      batches: 5
      clones_per_batch: 5000
      batch_mean_us: [14.938, 13.899, 15.081, 14.064, 14.183]
      mean_us: 14.433
      timed: Cluster.Clone() call only; close excluded
      remeasured_2026_09_16:
        mid_session_regressed_us: 60.1  # after decision:nested-child-documents + the OpenSearch 3.8 compatibility pass, before this session's two fixes
        fixed_batch_mean_ns: [781, 885, 804, 823, 1277]
        fixed_mean_ns: 914
        note: two unrelated causes (decision:shared-route-table, decision:nested-filter-scoping) fixed same-day; now faster than any previously recorded value
    python_server:
      clones: 300
      mean_us: 240.29
      median_us: 167.98
      p95_us: 282.25
      range_us: [136.67, 16360.21]
      remeasured_2026_09_16: {mid_session_regressed_us: 307.9, fixed_mean_us: 214.5}
    java_server:
      clones: 300
      mean_us: 583.05
      median_us: 331.63
      p95_us: 725.17
      range_us: [202.71, 63650.79]
      remeasured_2026_09_16: {mid_session_regressed_us: 627.9, fixed_mean_us: 506.5}
    node_server:
      clones: 300
      mean_us: 1632.38
      median_us: 1500.58
      p95_us: 1774.87
      range_us: [250.04, 43157.17]
      remeasured_2026_09_16: {mid_session_regressed_us: 1873.4, fixed_mean_us: 1771.6, note: "improved but still above the original 1632.38us mean; not investigated further"}
    timed: SDK clone creation includes localhost HTTP management request and response decode; clone close and mutations excluded
  go_in_process_10k:
    repetitions: 3
    aggregation_search_ms: 1.71
    exact_term_us: 33.3
    readonly_clone_and_count_us: 270
    remeasured_2026_09_16:
      repetitions: 5
      aggregation_search_ms: {baseline: 1.78, mid_session_regressed: 4.5, fixed: 4.5}
      exact_term_us: {baseline: 37.7, mid_session_regressed: 72, fixed: 45}
      readonly_clone_and_count_us: {baseline: 315, mid_session_regressed: 407, fixed: 175}
      note: >-
        bisected commit-by-commit (git worktree per commit, same benchmark run back to back) to
        two causes: decision:nested-child-documents's unconditional root-filter query clause
        (fixed: decision:nested-filter-scoping) inflated every query's bleve DocumentMatchPool
        pre-allocation past its 1000-document cap, and decision:shared-route-table's route-table
        rebuild inflated Clone()/New(). aggregation_search_ms (a bool query) is NOT fixed by
        either change: it goes through query_exec.go's evalSearcher/drainSearcher, added by the
        OpenSearch 3.8 pass for Lucene-faithful bool/dis_max/boosting score combination, which
        must fully materialize every clause's matches before combining scores and so cannot
        stream/early-terminate the way a single-clause query can - an inherent correctness
        trade-off, not investigated for a fix this round
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
    trials: 5 sequential fresh containers; cached OpenSearch image
    service: OpenSearch 2.19.0 linux/arm64; single-node; security disabled; 512 MiB heap
    data_tmpfs: /usr/share/opensearch/data
    tmpfs_options: rw,size=1g,uid=1000,gid=1000
    readiness: Testcontainers HTTP GET / returns 200, then successful PUT /benchmark
    ready_ms_samples: [8223, 6240, 6203, 5941, 5774]
    ready_ms_avg: 6476
    ready_ms_range: [5774, 8223]
    first_trial_includes: Ryuk helper startup
    container_memory_source: docker stats --no-stream
    container_memory_mib_samples: [1041.4, 938.9, 934.2, 921.2, 936.1]
    container_memory_mib_avg: 954.4
    runner_rss_before_mib_avg: 19.0
    runner_rss_ready_mib_avg: 20.3
    runner_rss_delta_mib_avg: 1.3
    container_plus_runner_delta_mib_avg: 955.7
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
  remeasured_2026_09_16:
    minimal_main_baseline: 1815298
    osmem_main_total: 32771522
    osmem_increment_over_baseline: 30956224
    osmem_ja_main_total: 45622018
    osmem_ja_increment_over_baseline: 43806720
    ja_increment_over_osmem: 12850496
    note: >-
      osmem-linked increment grew ~6.8MB: internal/engine/aggs_dates.go now blank-imports
      time/tzdata unconditionally (windows-latest CI hang fix, unrelated zone data), plus a
      session's worth of new OpenSearch 3.8 compatibility and segment-merge/sort engine code;
      the ja-specific increment (kagome/IPADIC) is essentially unchanged
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
  - Testcontainers Go runner memory reports only its RSS increase from immediately before container startup; Docker-reported container memory is added to that delta
  - Testcontainers data uses a 1 GiB tmpfs at /usr/share/opensearch/data; these measurements have a different storage condition from Docker and Devbox rows
  - Testcontainers total excludes the Docker daemon and Ryuk helper container; its first startup trial starts Ryuk
  - Devbox service uses process-compose to invoke docker run; its total RSS includes process-compose and the persistent Docker CLI, but excludes the Docker daemon
  - Devbox first Maven/JDK closure download is a one-time toolchain cost; the service-start trials use an already-pulled OpenSearch image
  - Docker, Testcontainers, and Devbox server readiness differ slightly in orchestration, but all finish after a successful PUT /benchmark
  - Go embedded and language-SDK startup paths have different boundaries: Go starts from an already-running test process, SDK launch includes child process creation, and Java also includes default classpath binary extraction
  - SDK startup samples run from already-running language processes; first-trial latency outliers remain included in the mean
  - Clone measurements use an already-seeded base and exclude writes; server SDK clones include localhost HTTP and JSON overhead, while Go Cluster.Clone is in-process
```
