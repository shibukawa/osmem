---
title: "性能と配布サイズ"
description: "osmemの起動・プロセスとcontainerのmemory・リンク後サイズ、Docker、Testcontainers、Devboxの計測値と条件。"
---

日本語analyzerを有効にすると、Go組み込みの準備は2.02msから335msへ伸びます。server SDKにはchild processの起動が加わり、Javaではclasspath binaryの展開も待ち時間に含まれます。以下は単一マシンの観測値です。

このページは、OpenSearch 3.8のREST API互換性修正と、2つの性能改善(単一document書き込み時のin-memory segment merge、typedなtop-K sort実行)を経た2026-09-16に再計測しました。再計測の過程で、`Cluster.Clone()`と通常のqueryがさらに2つの理由で遅くなっていることが分かりましたが、どちらも互換性修正に本質的なcostではなく、事故的な回帰でした。原因はHTTP route tableが`New()`/`Clone()`のたびに再構築されていたことと、nested fieldを持たないmappingでもqueryのたびにnested文書除外用の余分な条件を通していたことです。両方ともこのsession内でprofilingして修正しました(下記参照)。`Clone()`は特に、これまで公開したどの数値よりも速くなっています。1つだけ未対応のcostが残っています。bool query使用時の`_search`の遅延で、これはLucene準拠のscore合成へ書き換えたことに伴う本質的なtrade-offであり、bugではありません──こちらも下記で扱います。

## 書き込みとsortの性能

前回計測以降に2つの変更がmergeされました。

- **in-memory segment merge**([decision record](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/in-memory-segment-merge.md)): bleveのscorch indexはpathを持たない限りsegmentをmergeしないため、単一documentの書き込みはそれぞれ専用のsegmentを持ち続けていました。各segmentは約100document分のbuffer領域を確保します。indexは各segmentが保持するdocumentを追跡し、小さなsegment同士をre-indexしてmergeするようになりました。
- **typedでtop-K boundedなsort**([decision record](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/sort-execution.md)): 従来はsort specの解決とkey抽出を`[]any`経由で毎hit行い、size 10のpageしか要らない場合でも全hitをstable sortしていました。sort keyは書き込み時にtypedな形でcacheされ、要求されたpageだけをbounded heapで選択するようになりました。

この作業の直前のcommit(`0631107`)と現在の`HEAD`を同じマシン上でbuildし、同一benchmarkを連続実行して計測しました。`BenchmarkSingleDocWrites`と`BenchmarkSort100k`はこの作業より前には存在しなかったため、below の「修正前」の値は同じworkloadを修正前の`Cluster` APIで再現したものです。3〜5回実行の中央値。

| Benchmark | 修正前 | 修正後 |
|---|---:|---:|
| 単一document書き込み5,000件: 保持heap | 2,113 MiB | 26.1 MiB |
| 同上: term query | 2.44 ms | 43.7 µs |
| 同上: 書き込み1件あたり | 1.83 ms | 480 µs |
| 単一document書き込み1,000件: 保持heap | 421.5 MiB | 12.0 MiB |
| 同上: term query | 301 µs | 44.8 µs |
| 10万documentをsortしてpage 10取得(`double`) | 228.8 ms | 73.5 ms |
| 同上(`date`) | 268.3 ms | 55.4 ms |
| 同上(`keyword`) | 260.0 ms | 70.9 ms |
| 同上(2 key) | 323.4 ms | 79.1 ms |

1,000件の単一document書き込みでは、書き込み1件あたりの時間はほぼ変わりません(どちらも450〜500µs程度)。このscaleではsegment未mergeの代償は主に保持heapとqueryのcostに現れます。書き込み件数が増えるほど差は広がり、5,000件では旧codeが2GiBのheapに対して増大するGC costも払っていたため、segment数を`documents/1000 + 27`に抑える新codeとの差がさらに開きます。

## 二つの回帰を修正し一つを保留

同じsessionでOpenSearch 3.8のresponse忠実度に関する大規模な修正(commit `71962c6`)もmergeされました。実際のOpenSearch 3.8.0と2.19.1に対して477シナリオでosmemと比較し、差異のあるrequestは6,139件中3,263件から308件へ減っています。このページを再計測する中で、`Cluster.Clone()`と通常のqueryが前回計測より明らかに遅くなっていることが分かりました。このマシン上でcommit単位にbuild・計測を繰り返して二分探索したところ、原因は互いに無関係な2つの箇所に特定でき、どちらも互換性修正が本質的に必要としていたものではなく、ここで修正しました。

- **route tableが`New()`/`Clone()`のたびに再構築されていた。** `newHTTPHandler`は`buildRoutes()`(150以上のroute、それぞれclosure)を呼び、routing trieを毎回ゼロから構築していました。これは最初のcommitからそうだったのですが、`71962c6`が各routeの構築コストを(parameter validation用の付帯情報を追加して)引き上げたことで、誰も気づかないまま影響が拡大しました。どのhandler closureもtrie nodeも特定の`Cluster`に紐づいていません──全handlerは`*httpHandler`を引数として受け取り、そこを経由してclusterに到達します。そのためroute tableとtrieは今ではprocessごとに一度だけ構築され、共有されます。[decision record。](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/shared-route-table.md)
- **nested fieldのないmappingでも、queryのたびにroot文書だけに絞る条件が付いていた。** [nested-as-child-documents](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/nested-child-documents.md)(2026-09-14)が、nested子文書をhitに含めないための2つ目のquery句を無条件に追加していました。nested fieldを持たないmappingはnested子文書を絶対に持てないため、この句は常にno-opでしたが、それでもrequestをbleveの`DocumentMatchPool`事前確保の上限(1,000document)超えに押し上げ、確保costを倍増させていました。今はmappingにnested fieldがないときこの句を省略します。[decision record。](https://github.com/shibukawa/osmem/blob/main/.knowledge/decision/nested-filter-scoping.md)

どちらも修正前に`pprof`(該当benchmarkへの`-cpuprofile`/`-memprofile`)で確認し、修正後は`go test -race ./...`も通ります。下表は前回公開値・このsessionで回帰した(未修正)状態・修正後の現在値の3点です。

| Benchmark | 前回公開値 | 回帰時 | 修正後(現在) |
|---|---:|---:|---:|
| `Cluster.Clone()`単体(queryなし) | 14.4 µs | 60.1 µs | **0.8 µs** |
| `Cluster.Clone()` + `_count` query | 315 µs | 約407 µs | **約175 µs** |
| 単純なterm query、1万document | 37.7 µs | 約72 µs | **約45 µs** |
| `_search`: bool query + sort + date_histogram、1万document | 1.78 ms | 4.5 ms | 4.5 ms(下記参照) |

`Clone()`単体はこのページで過去最速の計測値になりました。HTTP requestを一切受けずquery DSLの解析も行わないため、呼び出しごとのroute再構築を取り除いたことで、in-process copy-on-write forkそのものに近いcostまで下がっています。term queryは元のbaselineに近い値まで戻りました。残るわずかな差はどちらの修正にも関係がなく、今回はそれ以上追っていません。

**bool queryを使う`_search`は未修正で、これはbugではありません。** このqueryは`71962c6`が追加したもう1つの仕組み(`internal/engine/query_exec.go`)を経由します。bleve自身のcompound searcherはLucene本来のquery normやcoordination factorとは異なる方法でscoreを合成するため、osmemの`bool`/`dis_max`/`boosting`/`function_score`queryは今ではLucene準拠のscoreを自前で計算します。そのためには、合成する前に各句の全matchをmemory上に読み切る必要があり、単一句のqueryのようにstreamしたり早期終了したりできません。これはaccidentではなく正しさを優先した本質的なcostで、profilingでも裏付けられています──`BenchmarkSearch10k`のmemory profileでは、該当句の全match件数を渡された`search.NewDocumentMatchPool`が最大の割り当て元でした。修正するにはLucene通りにscoreを合成しつつ早期終了できるevaluatorへの作り直しが必要で、より大きな設計変更になるため、今回は手を付けていません。

## API別の起動時間

前回計測と同じApple M3 / Go 1.27.0 / Python 3.14.7 / Node.js 26.8.1 / Java 25.0.2 / Docker 29.4.0(OrbStack)のマシンで2026-09-16に再計測しました。ただし今回は他の作業と同居しており、単発の平均値は前回よりnoiseが大きい点に注意してください。起動処理自体はこのsessionのengine変更の影響をほとんど受けません。`New()`/`LoadSeed()`は`New()`を1回呼ぶだけなので、[二つの回帰を修正し一つを保留](#二つの回帰を修正し一つを保留)のroute table修正で浮くのはせいぜい60µs程度で、この表の計測noiseに埋もれる差です(実際に変わった箇所は[書き込みとsortの性能](#書き込みとsortの性能)とその節を参照してください)。container系の行(Docker/Testcontainers/Devbox)とdownload sizeの節は、このsessionで変更していないinfrastructureを計測しているため再計測せず、2026-09-13/14の値のままです。

| API経路 | 日本語 | 起動平均 | 範囲 | 計測区間 |
|---|---|---:|---:|---|
| Go組み込み | 無効 | **2.02ms** | 1.52〜4.82ms (n=10) | `osmem.New()` + `LoadSeed()`。起動済みGo test processと実行ファイルlaunchは除外 |
| Go組み込み | 有効 | **335.1ms** | 313.3〜404.1ms (n=10) | 同上。`osmem/ja`をimportして有効化 |
| Python server SDK | 無効 | **126.2ms** | 18.2〜549.9ms (n=5) | `OsmemServer.start()`からchild起動・seed読込完了まで。Python runner起動済み |
| Python server SDK | 有効 | **339.6ms** | 327.0〜358.3ms (n=5) | 同上、日本語analyzer有効 |
| Java server SDK | 無効 | **462.0ms** | 371.0〜816.7ms (n=5) | `OsmemServer.start()`でclasspath内binaryを展開し、child起動・seed読込完了まで |
| Java server SDK | 有効 | **769.9ms** | 725.7〜837.0ms (n=5) | 同上、日本語analyzer有効 |
| Node.js server SDK | 無効 | **22.7ms** | 10.9〜68.3ms (n=5) | `OsmemServer.start()`からchild起動・seed読込完了まで。Node runner起動済み |
| Node.js server SDK | 有効 | **326.5ms** | 320.3〜346.2ms (n=5) | 同上、日本語analyzer有効 |
| Docker上のOpenSearch | — | **6.07秒** | 5.99〜6.31秒 (n=5) | warmな`opensearchproject/opensearch:2.19.0`、linux/arm64。`PUT /benchmark`成功まで。再計測なし(上記参照) |
| Testcontainers Go + OpenSearch | — | **6.48秒** | 5.77〜8.22秒 (n=5) | Testcontainers-Go 0.44.0。毎回fresh container、image取得済み。data pathは1GiB tmpfs。初回Ryuk起動を含む。再計測なし |
| Devbox管理のOpenSearch | — | **7.94秒** | 6.85〜10.63秒 (n=5) | Devbox 0.17.5 `services up -b`。process-composeから`docker run`、image取得済み。再計測なし |

Python・Java・Node.jsはlanguage runtimeが起動した後に計時しており、test runnerからSDKを呼ぶ条件です。Javaのclasspath展開、Python/Nodeの配置済み実行ファイルという条件も変わっていません。Go組み込みはHTTP listenerを起動せず、Go test process自体の起動も含みません。n=10の各試行は独立したsingle-shot processです。日本語analyzerの初期化がprocessごとに一度きりのcostだと分かったためです(詳細は後述)。container系は`PUT /benchmark`成功をready条件とし、seed済みosmemとは準備完了の境界が異なる参考値です。write latencyは対象外です。構築済みindexを使うtestを想定しているためです。

**新しい知見: 日本語analyzerの初期化はprocessごとに一度きりのcostです。** `osmem/ja`をimportした同一process内で`osmem.New()`を繰り返し呼んでも、kuromoji/kagomeの辞書読込costがかかるのは最初の呼び出しだけで、それ以降は日本語無効時と同じ0.75〜1.0msでした。このprojectのREADMEが示す通り、`osmem/ja`をimportして`TestMain`で1つのbase clusterを構築するGo test suiteなら、約320msのcostはtest binary実行ごとに1回だけで、testごとには発生しません。

## 変更テスト用cloneの生成時間

日本語有効で構築済みのseedを先に用意し、cloneを作る呼び出しだけを計測しました。fork側を変更しても共有baseには影響しません。document/indexのwriteとcloneのcloseは計時していません。

| API経路 | clone生成平均 | 試行 |
|---|---:|---|
| Go組み込み `Cluster.Clone()` | **0.8µs** | 5 batch × 5,000 clone。seed準備は計測外 |
| Python `server.clone()` | **215µs** | 起動済みseed serverから300回 |
| Java `server.clone()` | **507µs** | 起動済みseed serverから300回 |
| Node.js `server.clone()` | **1.77ms** | 起動済みseed serverから300回 |

[二つの回帰を修正し一つを保留](#二つの回帰を修正し一つを保留)の修正後の値です。Go組み込みの行はこのページで過去最速になりました(前回公開値14.4µs、session中の回帰時60.1µs)。language SDKの行も同じin-process `Clone()`を経由しますが、localhostのHTTP requestとJSON decodeがcostを薄めるため改善幅は相対的に小さく(回帰時から5〜30%)、PythonとJavaは前回公開値(240µs、583µs)も下回った一方、Node.jsは改善はしたものの前回の1.63msは上回ったままです。closeや、その後にテストが行うindex/document変更は含めません。

## 常駐メモリとqueryの参考値

| 経路 | ready時のmemory | query | 条件 |
|---|---:|---:|---|
| 日本語analyzer使用後のosmem server | RSS 161.8MiB | — | 同じseedで5 process。日本語match queryの後にRSS取得 |
| Go in-process API | 未計測 | filter + sort + date histogram: 4.5ms。完全一致term: 約45µs。読み取りclone + count: 約175µs | 1万document fixture。`go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=5`。5回の中央値、[二つの回帰を修正し一つを保留](#二つの回帰を修正し一つを保留)の修正後。bool queryの行だけ前回計測(1.78ms)より高いままで、理由は同節を参照 |
| 日本語無効のosmem HTTP query | 未計測 | 中央値0.5ms、p95 0.6ms、p99 1.1ms | 3-documentのseedに対する`match_all`、size 10のrequestを500回逐次実行。このdocument数ではloopback HTTPと`curl`のprocess起動costが支配的で、engine自体の時間ではありません |
| Docker上のOpenSearch | container RSS 942.2MiB | 中央値4.45ms、p95 9.76ms、p99 19.02ms | test index作成後に`docker stats`。queryは別試行。再計測なし |
| Testcontainers Go + OpenSearch | container 954.4MiB + runner RSS増分1.3MiB = **955.7MiB** | — | Go runnerは起動前19.0MiB、ready時20.3MiB。data tmpfsは1GiB。Docker daemonとRyukは除外。再計測なし |
| Devbox管理のOpenSearch | container 947.6MiB + process-compose/Docker CLI 61.0MiB = **1,008.6MiB** | — | Docker daemonは除外。再計測なし |

検索行ごとにindexとquery planは異なります。Go benchmarkは1万document、小さなseedを使うosmem HTTP確認、空indexを検索するOpenSearchであり、厳密なengine比較ではありません。Dockerはsingle node、heap 512MiB、security demo設定無効で測りました。hardware、heap、architecture、storage driver、image cache、readiness条件で結果は変わります。

## TestcontainersとDevbox services

Testcontainersはテスト側からcontainer runtimeを扱うwrapperで、検索engineではありません。JVMの大きく変動するbaselineをcontainer消費と誤認しないよう、Go版(`testcontainers-go` 0.44.0)で計測しました。OpenSearchはheap 512MiB、security無効で、`/usr/share/opensearch/data`をUID/GID 1000の1GiB tmpfsにmountしています。image取得済みの状態からfresh containerを5回起動し、index準備まで平均6.476秒(5.774〜8.223秒)でした。初回trialはTestcontainersのRyuk helper起動を含みます。ready時の`docker stats`はcontainer memory 954.4MiBを報告しました。Go runnerのRSSは起動前平均19.0MiB、ready時20.3MiBで、増分1.3MiBを加えた合計は955.7MiBです。Docker daemonとRyukはmemory値から除外しています。このtmpfs条件はDocker・Devboxの行とはstorage条件が異なります。class全体でcontainerを共有すれば、起動時間はそのclass内のtestで按分されます。ここは前回計測(2026-09-14)から変わっていないため、再計測していません。

リポジトリrootから再計測できます。

```bash
cd bench/testcontainers
go run .
```

計測用harnessはcontainer作成から`PUT /benchmark`成功までを計時し、その後`docker inspect`でtmpfs mountを検証します。既定ではfresh containerを5回起動し、引数でtrial数を指定できます。

Devboxはprocess-compose経由でserviceを管理できます。[公式services guide](https://www.jetify.com/docs/devbox/guides/services)に`devbox services up`とbackground起動の説明があります。ここでは`devbox services up -b`で、同じcache済みOpenSearch imageを`docker run`経由で起動しました。新規起動5回の平均は`PUT /benchmark`成功まで7.94秒。ready時のcontainer RSS平均947.6MiBに、常駐するprocess-composeとDocker CLIの61.0MiBを足して1,008.6MiBです。Docker daemonは除外しています。warm状態のno-op `devbox run`(156ms)とMaven/JDK closureの初回download(194.1MiB、展開後351.7MiB)は、server起動・server imageとは別の開発toolchain costです。ここも前回計測から変わっていないため、再計測していません。

## リンク後のアプリケーションバイナリとコンテナのdownload size

ライブラリのサイズは、source codeやpackage/archiveの容量ではなく、**Goアプリケーションをlinkした最終実行ファイルの増分**で示します。2026-09-16に、Go 1.27.0、`-trimpath`、`-ldflags=-buildid=`を揃え、空の`main`、osmem clusterを生成・終了する`main`、さらに日本語analyzerをimportする`main`の3種類をこのマシン上でビルドしました。日本語analyzerの値は空のbaselineからの合計増分に加え、osmem単体への追加分も示します。

| Go実行ファイル | サイズ |
|---|---:|
| 最小`main`のbaseline | 1,815,298 bytes (1.73 MiB) |
| osmemをlinkした実行ファイル | 32,771,522 bytes (31.26 MiB)、**増分+30,956,224 bytes (+29.52 MiB)** |
| osmem + 日本語analyzer | 45,622,018 bytes (43.51 MiB)、baseline比**増分+43,806,720 bytes (+41.78 MiB)**。このうちanalyzerの追加分は**12,850,496 bytes (12.25 MiB)** |

osmem単体の増分は前回計測(23.85 MB / 22.75 MiB)から約6.8MB増えました。`internal/engine/aggs_dates.go`がGoのIANA timezone databaseを無条件に埋め込む`time/tzdata`をblank importするようになったためです──windows-latestでのCI hangがplatform依存のzone dataに起因していたことへの修正です。これに加え、同じsession内のOpenSearch 3.8互換性修正やsegment merge/sortの作業がまとまった量のengine codeを追加したことも影響しています。日本語analyzer自体の追加分は前回とほぼ変わりません(12.25 MiB、前回12.26 MiB)。これはkagome/IPADIC辞書に依存する部分で、今回の変更では触れていません。

これは特定のtoolchainと最小プログラムにおけるlink結果で、packageサイズの一般的な保証値ではありません。Node.jsとPythonは別プロセスのosmem実行ファイルを使い、JavaはJVM artifactを使うため、静的にlinkしたアプリケーションバイナリ増分とは同じ条件で比較できません。

トップページでは3種類のサイズを10進MBの同じscaleで比較しています。Go appへのlink増分は**43.8 MB (+41.78 MiB)**、Devbox Maven/JDK環境の初回downloadは**203.5 MB (194.1 MiB)**、Docker Hub掲載の圧縮済み`linux/arm64` image sizeは**739.3 MB**です([Docker Hub](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0))。それぞれlink済み実行ファイルの差分、一度だけ取得するtoolchain、圧縮されたserver image全体であり、対象範囲は異なります。Dockerローカルの`Size`は展開後の容量なのでdownload sizeの代用にはしません。layerがcache済みなら実際の転送量はこれより少なくなる場合があります。

TestcontainersとDevbox serviceも同じOpenSearch imageを使うため、別のserver binaryはlinkしません。Devboxのtoolchain downloadはserver imageとは別指標です。

## Goの数値を再計測する

repository rootで実行します。

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k|SingleDocWrites|Sort100k)$' -benchmem -count=3
```

`CloneReadOnly`/`Search10k`/`TermQuery10k`は1万document、`Sort100k`は10万documentのfixtureを、query計測の前にbenchmarkが構築します。`SingleDocWrites`は計測対象そのものとして1,000件または5,000件を1件ずつ書き込みます。サービス起動やclient transportを測るときは、compile、binary展開、image取得、seed loadingを計測時間に含めるか、事前に明記してください。このページのbefore/after表のように変更をbaselineと比較するときは、`git worktree add`で両方のcommitをbuildし、同じマシン上で同じ`-bench`commandを連続実行してください。共有マシンや負荷のあるマシンでの単発計測は([API別の起動時間](#api別の起動時間)の通り)ばらつきが大きいため、日付の異なる表の値同士を比較するより、同一session・同一マシンでの比較の方が信頼できます。同じ手法を複数のcommitに繰り返し適用して、[上記の2つの回帰](#二つの回帰を修正し一つを保留)を「ここ数日のどこか」ではなく特定のcommitまで絞り込みました。
