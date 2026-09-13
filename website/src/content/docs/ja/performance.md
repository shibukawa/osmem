---
title: "性能と配布サイズ"
description: "osmemの起動・常駐メモリ・リンク後サイズ、Docker、Testcontainers、Devboxの計測値と条件。"
---

テストごとに新しいcontainerを起動する場合と、1つのserverからcopy-on-write forkを作る場合では、払うコストが違います。以下では起動、常駐メモリ、wrapperの上乗せを分けて示します。いずれも手元の計測値で、一般的な保証ではありません。

## 手元での計測

2026-09-13、Apple M3、macOS arm64、Go 1.27.0、Node.js 26.8.1、OrbStack上のDocker 29.4.0、8コアのLinux arm64コンテナ(Java heap 512 MiB)で計測しました。単一マシンでの観測値であり、リリース時の保証値ではありません。

| 経路 | 起動・準備完了の平均 | 常駐メモリの平均 | 検索 | 条件 |
|---|---:|---:|---:|---|
| `osmem-server`、日本語有効 | 680ms | RSS 160.4MiB | — | 5回起動。525-byte seed、2 indexに3 document。RSS取得前に日本語match queryを実行 |
| `osmem-server`、日本語無効 | 初回83ms、以後8.8〜13.0ms | 未計測 | 中央値1.53ms、p95 1.84ms、p99 2.94ms | 12回起動。同じ525-byte seed。`match_all`、size 10のHTTP requestを500回逐次実行 |
| Go in-process API | 読み取りclone＋count: 270µs | 未計測 | filter＋sort＋date histogram: 1.71ms。完全一致term: 33.3µs | `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`。3回の中央値 |
| Docker上のOpenSearch | `PUT /benchmark`成功まで6.07秒 | container RSS 942.2MiB | 中央値4.45ms、p95 9.76ms、p99 19.02ms | image取得後の5試行。`opensearchproject/opensearch:2.19.0`、linux/arm64、single node、heap 512MiB。queryは別試行 |
| Testcontainers + OpenSearch | `PUT /benchmark`成功まで8.12秒 | container 941.9MiB + test JVM RSS 88.7MiB | — | Testcontainers Java 2.0.5。1つのMaven/JUnit process内でfresh containerを5回逐次起動。image取得済み |

検索の各行はindexもquery planも揃っていません。osmem HTTPは小さなseed、Go benchmarkは1万件、OpenSearchは空indexを検索しています。経路規模の参考値であり、厳密なengine比較ではありません。テストでは構築済みindexを再利用する想定が高いため、起動時間との比較で主題ではないwrite時間は載せていません。

osmemのRSSは、日本語analyzerを実際に使った後のGo processです。Docker RSSはテスト用indexを作成した後の`docker stats`値です。Docker直起動の準備時間は、warm imageから`docker run`し、`PUT /benchmark`が成功するまでです。single node、heap 512MiB、security demo設定無効で測りました。hardware、heap、architecture、storage driver、image cache、readiness条件によって結果は変わります。

## TestcontainersとDevbox

Testcontainersはテスト側からcontainer runtimeを扱うwrapperで、検索engineではありません。warm imageから5回fresh containerを起動すると、index準備まで平均8.12秒でした。OpenSearch container自体のmemoryはDocker直起動とほぼ同じで、Testcontainers test JVMがさらにRSS 88.7MiBを使いました。Docker直起動との差約2秒は、この2つのharnessで観測した差であり、library単体の純粋なoverheadではありません。class全体でcontainerを共有すれば、起動時間はそのclass内のtestで按分されます。

Devboxは環境管理toolで、server runtimeではありません。Maven 3.9.16を使う一時Devbox 0.17.5環境で、warm状態のno-op `devbox run`は6回平均156msでした。Nix package closureの初回取得は194.1MiB、store展開後は351.7MiBです。これは開発環境の準備と`devbox run`のcostであり、検索serverの起動やmemoryではありません。通常はtest suiteを起動する一度だけshell wrapperを払います。

## リンク後のアプリケーションバイナリとコンテナのdownload size

ライブラリのサイズは、source codeやpackage/archiveの容量ではなく、**Goアプリケーションをlinkした最終実行ファイルの増分**で示します。Go 1.27.0、`-trimpath`、`-ldflags=-buildid=`を揃え、空の`main`、osmem clusterを生成・終了する`main`、さらに日本語analyzerをimportする`main`の3種類をビルドしました。日本語analyzerの値は空のbaselineからの合計増分に加え、osmem単体への追加分も示します。

| Go実行ファイル | サイズ |
|---|---:|
| 最小`main`のbaseline | 1,815,314 bytes (1.73 MiB) |
| osmemをlinkした実行ファイル | 25,668,722 bytes (24.48 MiB)、**増分+23,853,408 bytes (+22.75 MiB)** |
| osmem + 日本語analyzer | 38,529,298 bytes (36.74 MiB)、baseline比**増分+36,713,984 bytes (+35.01 MiB)**。このうちanalyzerの追加分は**12,860,576 bytes (12.26 MiB)** |

これは特定のtoolchainと最小プログラムにおけるlink結果で、packageサイズの一般的な保証値ではありません。Node.jsとPythonは別プロセスのosmem実行ファイルを使い、JavaはJVM artifactを使うため、静的にlinkしたアプリケーションバイナリ増分とは同じ条件で比較できません。

OpenSearch containerは別のdownload footprintとして扱います。Docker Hub掲載の圧縮済み`linux/arm64` image sizeは[739.3 MB](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0)です。Dockerローカルの`Size`は展開後の容量なのでdownload sizeの代用にはしません。layerがcache済みなら実際の転送量はこれより少なくなる場合があります。

Testcontainersも同じOpenSearch imageを使うため、別のserver binaryはlinkしません。Devboxの194.1MiBはMaven/JDK環境のdownloadであり、アプリケーションbinaryのサイズとは別指標です。

## Goの数値を再計測する

repository rootで実行します。

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

query計測の前にbenchmarkが1万documentのfixtureを構築します。サービス起動やclient transportを測るときは、compile、binary展開、image取得、seed loadingを計測時間に含めるか、事前に明記してください。
