---
title: "性能と配布サイズ"
description: "osmemの起動・検索、リンク後のアプリケーションバイナリ増分、Docker上のOpenSearchのローカル計測と条件。"
---

比較したいのは、単一の速度値ではありません。検索エンジンの起動、schema登録、起動済みプロセスへの1リクエスト、データのforkは、それぞれ別のコストです。以下では測定対象を分けています。

## 手元での計測

2026-09-13、Apple M3、macOS arm64、Go 1.27.0、Node.js 26.8.1、OrbStack上のDocker 29.4.0、8コアのLinux arm64コンテナ(Java heap 512 MiB)で計測しました。単一マシンでの観測値であり、リリース時の保証値ではありません。

| 経路 | 準備完了まで | 検索 | 条件 |
|---|---:|---:|---|
| `osmem-server`子プロセス | 初回83ms、以後8.8〜13.0ms | 中央値1.53ms、p95 1.84ms、p99 2.94ms | 12回起動。`--no-ja`、seed file合計525 bytes・6 document。`match_all`、size 10のHTTP requestを500回逐次実行 |
| Go in-process API | 読み取りclone＋count: 270µs | filter＋sort＋date histogram: 1.71ms。完全一致term: 33.3µs | `go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3`。3回の中央値 |
| Docker上のOpenSearch | index作成成功まで9.44秒 | 中央値4.45ms、p95 9.76ms、p99 19.02ms | `opensearchproject/opensearch:2.19.0`、linux/arm64、single node、heap 512MiB。空indexへの検索を500回逐次実行 |

検索の各行は、indexもquery planも揃っていません。osmem HTTPは6件のseed document、Go benchmarkは1万件、OpenSearchは空indexを検索しています。これらは手元での経路規模を示すもので、厳密なengine比較ではありません。テストでは構築済みindexを再利用する想定が高く、今回の比較で見たい起動時間に対してwrite時間は主題ではないため、書き込み計測は載せていません。

Dockerの準備時間は、`docker run`から`PUT /benchmark`が成功するまでと定義しました。コンテナのプロセスが立ち上がる時刻ではありません。security demo設定は無効化しています。実運用に近い設定、heap、CPU architecture、storage driver、ホストによって結果は大きく変わります。

## TestcontainersとDevbox

Testcontainersはテスト側からcontainer runtimeを扱う仕組みで、検索エンジン自体ではありません。このリポジトリにはTestcontainers fixtureがなく、計測環境にはJava build toolもなかったため、Testcontainers固有の上乗せ時間は別途測っていません。Docker欄はOpenSearch container単体の基準値です。Testcontainersの計測ではlibrary version、runtime、image pull/cacheの状態、readiness判定を揃えて記録してください。

Devboxは環境管理ツールであり、サーバーruntimeではありません。Devbox 0.17.5はインストールされていましたが、このrepositoryに`devbox.json`がないため、設定済み環境や依存downloadの時間はありません。benchmarkに必要なtoolを固定することはできますが、Devbox自体がDockerを置き換えたりOpenSearchを起動したりするわけではありません。

## リンク後のアプリケーションバイナリとコンテナのdownload size

ライブラリのサイズは、source codeやpackage/archiveの容量ではなく、**Goアプリケーションをlinkした最終実行ファイルの増分**で示します。Go 1.27.0、`-trimpath`、`-ldflags=-buildid=`を揃え、空の`main`、osmem clusterを生成・終了する`main`、さらに日本語analyzerをimportする`main`の3種類をビルドしました。日本語analyzerの値は空のbaselineからの合計増分に加え、osmem単体への追加分も示します。

| Go実行ファイル | サイズ |
|---|---:|
| 最小`main`のbaseline | 1,815,314 bytes (1.73 MiB) |
| osmemをlinkした実行ファイル | 25,668,722 bytes (24.48 MiB)、**増分+23,853,408 bytes (+22.75 MiB)** |
| osmem + 日本語analyzer | 38,529,298 bytes (36.74 MiB)、baseline比**増分+36,713,984 bytes (+35.01 MiB)**。このうちanalyzerの追加分は**12,860,576 bytes (12.26 MiB)** |

これは特定のtoolchainと最小プログラムにおけるlink結果で、packageサイズの一般的な保証値ではありません。Node.jsとPythonは別プロセスのosmem実行ファイルを使い、JavaはJVM artifactを使うため、静的にlinkしたアプリケーションバイナリ増分とは同じ条件で比較できません。

OpenSearch containerは別のdownload footprintとして扱います。Docker Hub掲載の圧縮済み`linux/arm64` image sizeは[739.3 MB](https://hub.docker.com/r/opensearchproject/opensearch/tags?name=2.19.0)です。Dockerローカルの`Size`は展開後の容量なのでdownload sizeの代用にはしません。layerがcache済みなら実際の転送量はこれより少なくなる場合があります。

## Goの数値を再計測する

repository rootで実行します。

```bash
go test -run '^$' -bench 'Benchmark(CloneReadOnly|Search10k|TermQuery10k)$' -benchmem -count=3
```

query計測の前にbenchmarkが1万documentのfixtureを構築します。サービス起動やclient transportを測るときは、compile、binary展開、image取得、seed loadingを計測時間に含めるか、事前に明記してください。
