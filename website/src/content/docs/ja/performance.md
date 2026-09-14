---
title: "性能と配布サイズ"
description: "osmemの起動・プロセスとcontainerのmemory・リンク後サイズ、Docker、Testcontainers、Devboxの計測値と条件。"
---

日本語analyzerを有効にすると、Go組み込みの準備は2.13msから319.7msへ伸びます。seed読込中にkuromojiを初期化するためです。server SDKにはchild processの起動が加わり、Javaではclasspath binaryの展開も待ち時間に含まれます。以下は単一マシンの観測値です。

## API別の起動時間

大半は2026-09-13にApple M3、macOS arm64、Go 1.27.0、Python 3.14.7、Node.js 26.8.1、Java 25.0.2、OrbStack上のDocker 29.4.0で計測しました。Testcontainersのtmpfs計測は同じGo・Docker versionで2026-09-14に再実施しています。osmemの全経路で同じ525-byte seed（2 index、3 document）を読み込みます。日本語有効ではseed内のkuromoji analyzerを初期化し、無効ではCJK fallbackを使います。

| API経路 | 日本語 | 起動平均 | 範囲 | 計測区間 |
|---|---|---:|---:|---|
| Go組み込み | 無効 | **2.13ms** | 1.54〜2.87ms (n=10) | `osmem.New()` + `LoadSeed()`。起動済みGo test processと実行ファイルlaunchは除外 |
| Go組み込み | 有効 | **319.7ms** | 312.0〜347.8ms (n=10) | 同上。`osmem/ja`をimportして有効化 |
| Python server SDK | 無効 | **23.5ms** | 10.1〜69.4ms (n=5) | `OsmemServer.start()`からchild起動・seed読込完了まで。Python runner起動済み |
| Python server SDK | 有効 | **332.8ms** | 321.1〜366.8ms (n=5) | 同上、日本語analyzer有効 |
| Java server SDK | 無効 | **435.2ms** | 305〜868ms (n=5) | `OsmemServer.start()`でclasspath内binaryを展開し、child起動・seed読込完了まで |
| Java server SDK | 有効 | **730.4ms** | 653〜797ms (n=5) | 同上、日本語analyzer有効 |
| Node.js server SDK | 無効 | **10.8ms** | 10.1〜12.8ms (n=5) | `OsmemServer.start()`からchild起動・seed読込完了まで。Node runner起動済み |
| Node.js server SDK | 有効 | **325.1ms** | 318.6〜344.1ms (n=5) | 同上、日本語analyzer有効 |
| Docker上のOpenSearch | — | **6.07秒** | 5.99〜6.31秒 (n=5) | warmな`opensearchproject/opensearch:2.19.0`、linux/arm64。`PUT /benchmark`成功まで |
| Testcontainers Go + OpenSearch | — | **6.48秒** | 5.77〜8.22秒 (n=5) | Testcontainers-Go 0.44.0。毎回fresh container、image取得済み。data pathは1GiB tmpfs。初回Ryuk起動を含む |
| Devbox管理のOpenSearch | — | **7.94秒** | 6.85〜10.63秒 (n=5) | Devbox 0.17.5 `services up -b`。process-composeから`docker run`、image取得済み |

Python・Java・Node.jsはlanguage runtimeが起動した後に計時しており、test runnerからSDKを呼ぶ条件です。server binaryは計測前にbuild済み。Javaは起動のたびにclasspathからbinaryを一時fileへcopyし、PythonとNode.jsは配置済み実行ファイルを使います。日本語無効のJava計測には868msの外れ値も含まれます。Go組み込みはHTTP listenerを起動せず、Go test process自体の起動も含みません。container系は`PUT /benchmark`成功をready条件とし、seed済みosmemとは準備完了の境界が異なる参考値です。

構築済みindexを使うtestでは、個々のdocument書き込みは起動コストと別です。そこでwrite latencyではなく、seed済みの環境が使えるまでを測りました。

## 変更テスト用cloneの生成時間

日本語有効で構築済みのseedを先に用意し、cloneを作る呼び出しだけを計測しました。fork側を変更しても共有baseには影響しません。document/indexのwriteとcloneのcloseは計時していません。

| API経路 | clone生成平均 | 試行 |
|---|---:|---|
| Go組み込み `Cluster.Clone()` | **14.4µs** | 5 batch × 5,000 clone。seed準備は計測外 |
| Python `server.clone()` | **240µs** | 起動済みseed serverから300回 |
| Java `server.clone()` | **583µs** | 起動済みseed serverから300回 |
| Node.js `server.clone()` | **1.63ms** | 起動済みseed serverから300回 |

language SDKの値にはlocalhostのHTTP management requestとJSON decodeが含まれます。Go組み込みはportを開かないin-process copy-on-write forkです。closeや、その後にテストが行うindex/document変更は含めません。process schedulingによるoutlierがあるため、clone計測の詳細値は平均よりばらつきます。

## 常駐メモリとqueryの参考値

| 経路 | ready時のmemory | query | 条件 |
|---|---:|---:|---|
| 日本語analyzer使用後のosmem server | RSS 160.4MiB | — | 同じseedで5 process。日本語match queryの後にRSS取得 |
| Go in-process API | 未計測 | filter + sort + date histogram: 1.71ms。完全一致term: 33.3µs。読み取りclone + count: 270µs | 1万document fixture。`go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`。3回の中央値 |
| 日本語無効のosmem HTTP query | 未計測 | 中央値1.53ms、p95 1.84ms、p99 2.94ms | `match_all`、size 10のrequestを500回逐次実行 |
| Docker上のOpenSearch | container RSS 942.2MiB | 中央値4.45ms、p95 9.76ms、p99 19.02ms | test index作成後に`docker stats`。queryは別試行 |
| Testcontainers Go + OpenSearch | container 954.4MiB + runner RSS増分1.3MiB = **955.7MiB** | — | Go runnerは起動前19.0MiB、ready時20.3MiB。data tmpfsは1GiB。Docker daemonとRyukは除外 |
| Devbox管理のOpenSearch | container 947.6MiB + process-compose/Docker CLI 61.0MiB = **1,008.6MiB** | — | Docker daemonは除外 |

検索行ごとにindexとquery planは異なります。Go benchmarkは1万document、小さなseedを使うosmem HTTP確認、空indexを検索するOpenSearchであり、厳密なengine比較ではありません。Dockerはsingle node、heap 512MiB、security demo設定無効で測りました。hardware、heap、architecture、storage driver、image cache、readiness条件で結果は変わります。

## TestcontainersとDevbox services

Testcontainersはテスト側からcontainer runtimeを扱うwrapperで、検索engineではありません。JVMの大きく変動するbaselineをcontainer消費と誤認しないよう、Go版(`testcontainers-go` 0.44.0)で計測しました。OpenSearchはheap 512MiB、security無効で、`/usr/share/opensearch/data`をUID/GID 1000の1GiB tmpfsにmountしています。image取得済みの状態からfresh containerを5回起動し、index準備まで平均6.476秒（5.774〜8.223秒）でした。初回trialはTestcontainersのRyuk helper起動を含みます。ready時の`docker stats`はcontainer memory 954.4MiBを報告しました。Go runnerのRSSは起動前平均19.0MiB、ready時20.3MiBで、増分1.3MiBを加えた合計は955.7MiBです。Docker daemonとRyukはmemory値から除外しています。このtmpfs条件はDocker・Devboxの行とはstorage条件が異なります。class全体でcontainerを共有すれば、起動時間はそのclass内のtestで按分されます。

リポジトリrootから再計測できます。

```bash
cd bench/testcontainers
go run .
```

計測用harnessはcontainer作成から`PUT /benchmark`成功までを計時し、その後`docker inspect`でtmpfs mountを検証します。既定ではfresh containerを5回起動し、引数でtrial数を指定できます。

Devboxはprocess-compose経由でserviceを管理できます。[公式services guide](https://www.jetify.com/docs/devbox/guides/services)に`devbox services up`とbackground起動の説明があります。ここでは`devbox services up -b`で、同じcache済みOpenSearch imageを`docker run`経由で起動しました。新規起動5回の平均は`PUT /benchmark`成功まで7.94秒。ready時のcontainer RSS平均947.6MiBに、常駐するprocess-composeとDocker CLIの61.0MiBを足して1,008.6MiBです。Docker daemonは除外しています。warm状態のno-op `devbox run`(156ms)とMaven/JDK closureの初回download(194.1MiB、展開後351.7MiB)は、server起動・server imageとは別の開発toolchain costです。

## リンク後のアプリケーションバイナリとコンテナのdownload size

ライブラリのサイズは、source codeやpackage/archiveの容量ではなく、**Goアプリケーションをlinkした最終実行ファイルの増分**で示します。Go 1.27.0、`-trimpath`、`-ldflags=-buildid=`を揃え、空の`main`、osmem clusterを生成・終了する`main`、さらに日本語analyzerをimportする`main`の3種類をビルドしました。日本語analyzerの値は空のbaselineからの合計増分に加え、osmem単体への追加分も示します。

| Go実行ファイル | サイズ |
|---|---:|
| 最小`main`のbaseline | 1,815,314 bytes (1.73 MiB) |
| osmemをlinkした実行ファイル | 25,668,722 bytes (24.48 MiB)、**増分+23,853,408 bytes (+22.75 MiB)** |
| osmem + 日本語analyzer | 38,529,298 bytes (36.74 MiB)、baseline比**増分+36,713,984 bytes (+35.01 MiB)**。このうちanalyzerの追加分は**12,860,576 bytes (12.26 MiB)** |

これは特定のtoolchainと最小プログラムにおけるlink結果で、packageサイズの一般的な保証値ではありません。Node.jsとPythonは別プロセスのosmem実行ファイルを使い、JavaはJVM artifactを使うため、静的にlinkしたアプリケーションバイナリ増分とは同じ条件で比較できません。

トップページでは3種類のサイズを10進MBの同じscaleで比較しています。Go appへのlink増分は**36.7 MB (+35.0 MiB)**、Devbox Maven/JDK環境の初回downloadは**203.5 MB (194.1 MiB)**、Docker Hub掲載の圧縮済み`linux/arm64` image sizeは**739.3 MB**です([Docker Hub](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0))。それぞれlink済み実行ファイルの差分、一度だけ取得するtoolchain、圧縮されたserver image全体であり、対象範囲は異なります。Dockerローカルの`Size`は展開後の容量なのでdownload sizeの代用にはしません。layerがcache済みなら実際の転送量はこれより少なくなる場合があります。

TestcontainersとDevbox serviceも同じOpenSearch imageを使うため、別のserver binaryはlinkしません。Devboxのtoolchain downloadはserver imageとは別指標です。

## Goの数値を再計測する

repository rootで実行します。

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

query計測の前にbenchmarkが1万documentのfixtureを構築します。サービス起動やclient transportを測るときは、compile、binary展開、image取得、seed loadingを計測時間に含めるか、事前に明記してください。
