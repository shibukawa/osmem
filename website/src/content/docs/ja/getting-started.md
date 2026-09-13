---
title: "はじめに"
description: "osmemとは何か、ベースとクローンのモデル、Go・Node.js・Python・Javaでの導入方法。"
---

OpenSearchに触れるテストは、たいてい二度コストを払っています。一度はコンテナの起動に。もう一度は、書いたドキュメントが検索できるようになるまでリフレッシュを待つ、テストのたびに。osmemはその両方をなくします。すべてのインデックスをメモリに置くGo製のOpenSearch互換サーバーで、ミリ秒で起動し、書き込みは次の検索からすぐに見えます。このページを読むと、2つある形態のどちらが自分のテストスイートに合うかと、そのインストール方法がわかります。

## できること、できないこと

osmemはOpenSearch 2.xのREST APIを話します。インデックスとドキュメントのCRUD、bulk、クエリDSL、集計、scroll、エイリアス、テンプレート。既存のクライアントはそのまま動きます。ただし、中身はOpenSearchではありません。転置インデックスとBM25のスコアリングは純Goの検索ライブラリbleveが担い、それ以外(マッピングの解釈、ソート、集計、ハイライト)は保存したドキュメントに対してGoで実装し直しています。

この設計が限界を決めます。普通のクエリなら順位はOpenSearchと一致しますが、`_score`の数値は異なるので、テストでスコアの値を検証してはいけません。Painlessスクリプトは未対応で400を返します。nestedドキュメントはフラットに扱われます。既知の差異はすべて[互換性](../compatibility/)のページにまとめてあります。全体を貫く方針は、未対応の機能はそれらしい誤った結果を返さず、はっきりエラーにする、というものです。

## モデル: ベースは1つ、クローンは多数

言語が何であれ、テストスイートの形は同じです。

1. **ベースクラスタ**を一度だけ作る。インデックスを作成し、マッピングを設定し、シードドキュメントを投入する。
2. テストごとに、ベースの**クローン**を取る。クローンは何もコピーしません。どちらかが書き込んだときに初めてそのインデックスが複製されるので、クローン自体はマイクロ秒で済み、書き込むテストは触ったインデックスの分を一度払うだけです。
3. クローンに対してテストを実行し、捨てる。

クローンが一度でも作られると、ベースは**凍結**されます。ベースへのHTTP経由の書き込みは403 `osmem_base_frozen`になります。クローンを取り忘れたテストから、フィクスチャを守るためです。読み取りだけのテストは、ベースを直接検索してかまいません。

## 2つの動かし方

| テストの言語 | 使うもの | サーバーの動き方 |
|---|---|---|
| Go | パッケージ`osmem`と`osmemtest`ヘルパー | テストプロセスの中で動く。ネットワーク不要 |
| Node.js、Python、Java、その他 | 各言語のパッケージ経由の`osmem-server` | テストセッションが起動する子プロセスとして、ループバックのポートでHTTPを話す |

どちらの形態も、同じREST APIと同じシード形式を持ちます。Goの形態は速く、バイナリも要りません。サブプロセスの形態は、HTTPクライアントが使える環境ならどこでも動きます。

## インストール

```bash
# Go
go get github.com/shibukawa/osmem

# Node.js (the binary for your platform arrives as an optional dependency)
npm install --save-dev @osmem/core

# Python (the wheel bundles the binary)
pip install osmem-server

# Java (Maven; add the binaries jar for the platform your tests run on)
#   io.github.shibukawa.osmem:osmem:0.1.0
#   io.github.shibukawa.osmem:osmem-server-binaries:0.1.0:linux-amd64
```

## 次に読むもの

- [Goガイド](../go/)、[Node.jsガイド](../node/)、[Pythonガイド](../python/)、[Javaガイド](../java/)
- [シードデータ](../seed-data/): ベースクラスタをファイルで記述する方法
- [管理API](../management-api/): 各言語パッケージの裏にある`/_osmem`エンドポイント
- [互換性](../compatibility/): 動くもの、近似しているもの、エラーになるもの
