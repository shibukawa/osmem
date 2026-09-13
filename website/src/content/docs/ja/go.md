---
title: "Goガイド"
description: "Goのテスト内でosmemを動かす。TestMainでシードし、osmemtestでテストを分離し、プロセス内APIを使う。"
---

Goでは、osmemはテストバイナリの中で動きます。起動するプロセスも、待つポートもありません。クラスタはただの値で、クローンはメソッド呼び出しです。HTTPサーバーは、向き先にするクライアントライブラリのためにだけ存在します。このページでは、TestMainのパターン、`osmemtest`によるテストごとの分離、そしてよく使うオプションを説明します。

## インストール

```bash
go get github.com/shibukawa/osmem
```

## ベースを一度だけ作る

すべてのテストが同じ状態から始まるよう、`TestMain`でシードします。`LoadSeed`は[シードディレクトリ](../seed-data/)を読み込みます。`BulkString`と`CreateIndex`は、Goの値から同じことをします。

```go
package shop_test

import (
    "log"
    "os"
    "testing"

    "github.com/shibukawa/osmem"
)

var base *osmem.Cluster

func TestMain(m *testing.M) {
    base = osmem.New()
    if err := base.LoadSeed("testdata/seed"); err != nil {
        log.Fatal(err)
    }
    code := m.Run()
    base.Close()
    os.Exit(code)
}
```

これ以降、`base`を変更するものはありません。読むだけのテストは直接使ってよく、書き込むテストはクローンを取ります。

## テストごとに1つのクローン

`osmemtest`は`t.Cleanup`を通じてライフタイムをテストに結び付けます。だから、テスト本体に後始末のコードは現れません。

```go
import (
    "context"
    "strings"

    "github.com/opensearch-project/opensearch-go/v4"
    "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
    "github.com/shibukawa/osmem/osmemtest"
)

func TestAddProduct(t *testing.T) {
    ctx := context.Background()
    _, srv := osmemtest.CloneAndServe(t, base)
    client, err := opensearchapi.NewClient(opensearchapi.Config{
        Client: opensearch.Config{Addresses: []string{srv.URL}},
    })
    if err != nil {
        t.Fatal(err)
    }

    // this write never reaches base or any other test
    _, err = client.Index(ctx, opensearchapi.IndexReq{
        Index: "products", DocumentID: "x", Body: strings.NewReader(`{"name": "new"}`),
    })
    if err != nil {
        t.Fatal(err)
    }
    got, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "products", DocumentID: "x"})
    if err != nil || !got.Found {
        t.Fatalf("document not found: %v", err)
    }
}
```

別々に使いたいときは、`osmemtest.Clone(t, base)`と`osmemtest.Serve(t, c)`がその2つの半分です。`osmemtest.New(t)`は、自分でマッピングを作るテスト向けに空のクラスタを返します。

`t.Parallel()`は、書き込むテストがそれぞれ自分のクローンを持つ限り安全です。並行する2つのテストが1つのクローンを共有すれば、共有のOpenSearchを使ったときと同じく、互いのドキュメントが見えてしまいます。

## ネットワークを使わずにクラスタと話す

セットアップやアサーションのコードでは、`Do`がハンドラを通してプロセス内でリクエストを実行します。

```go
res, err := c.Do(http.MethodPost, "/products/_search", `{"query": {"term": {"tags": "red"}}}`)
if err != nil || res.IsError() {
    t.Fatal(err, string(res.Body))
}
```

よくある操作にはラッパーがあります。`CreateIndex`、`Index`、`Get`、`Bulk`、`Search`(構造体にデコード)、`Count`、`DeleteIndex`。どれも、OpenSearch自身のエラー種別と理由をGoのエラーに含めて返します。

## 日本語テキスト

`ja`パッケージを一度importすると、マッピングとインデックス設定で`kuromoji`アナライザ、`kuromoji_tokenizer`とそのフィルタが使えるようになります。実体は、IPA辞書を使う純Goの形態素解析器kagomeです。

```go
import _ "github.com/shibukawa/osmem/ja"
```

importしなければ、`kuromoji`はCJKのbigramにフォールバックします。これは、analysis-kuromojiプラグインを入れていないOpenSearchクラスタと同じ振る舞いです。辞書でテストバイナリは約8MB増え、読み込みはプロセスごとに1回です。

## オプション

- `osmem.WithClock(func() time.Time)`は、日付演算(`now-7d/d`)と作成日時の「現在」を固定します。範囲クエリの結果が再現可能になります。
- `osmem.WithWarnings(func(string))`は、bleveに対応物がないトークンフィルタなど、osmemが近似せざるを得なかった解析設定を報告します。
- `c.Freeze()`は、HTTP経由の書き込みを403で拒否します。サブプロセス形態が自動で適用するのと同じ保護です。Goからの呼び出しは引き続き動きます。凍結が守るのはネットワーク側だけです。
- `c.Handler()`は`http.Handler`を返します。自前のサーバー、たとえば`httptest.NewServer(c.Handler())`を使いたいときのためです。

## リフレッシュとソート

書き込みは、`_refresh`なしで次の検索から見えます。エンドポイント自体は受け付けて、何もしません。一方、ソートはOpenSearchに厳密に従います。`text`フィールドでの`sort`は同じ`illegal_argument_exception`で失敗するので、エラーを回避しようとせず、これまでどおり`.keyword`サブフィールドを使ってください。
