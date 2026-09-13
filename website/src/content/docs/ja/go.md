---
title: "Goガイド"
description: "Goのテスト内でosmemを動かす。TestMainでシードし、osmemtestでテストを分離し、プロセス内APIを使う。"
---

Goでは、osmemはテストバイナリの中で動きます。起動するプロセスも、待つポートもありません。クラスタはただの値で、クローンはメソッド呼び出しです。HTTPサーバーは、向き先にするクライアントライブラリのためにだけ存在します。このページでは、TestMainのパターン、`osmemtest`によるテストごとの分離、そしてよく使うオプションを説明します。

## インストール

```bash
go get github.com/shibukawa/osmem
# 下記の公式OpenSearchクライアントを使う場合:
go get github.com/opensearch-project/opensearch-go/v4
```

クライアントは必須ではありません。ネットワークを通さずに試すなら、`Cluster.Do`や各種wrapperで同じRESTの振る舞いを呼び出せます。

## ベースを一度だけ作る

すべてのテストが同じ状態から始まるよう、`TestMain`でシードします。`LoadSeed`は[シードディレクトリ](../seed-data/)を読み込みます。`BulkString`と`CreateIndex`は、Goの値から同じことをします。

```go
package shop_test

import (
    "log"
    "testing"

    "github.com/shibukawa/osmem"
)

var base *osmem.Cluster

func TestMain(m *testing.M) {
    base = osmem.New()
    defer base.Close()
    if err := base.LoadSeed("testdata/seed"); err != nil {
        log.Fatal(err)
    }
    m.Run()
}
```

`TestMain`はそのままreturnしてかまいません。Go 1.15以降は`m.Run`の結果が終了コードになるので、deferした`Close`も実行されます。osmemは、`*testing.M`を受け取るヘルパーをあえて用意していません。PostgreSQL用のpgmemなど他のインメモリフェイクも、同じ関数の中でそれぞれの`defer`とともに準備でき、どのライブラリがテストバイナリを握るかを決める必要がないからです。

これ以降、`base`を変更するものはありません。読むだけのテストは直接使ってよく、書き込むテストはクローンを取ります。

## スキーマを登録し、データを入れて公式クライアントを使う

`LoadSeed`はindex template、index schema、bulk documents、aliasの順に適用します。テスト内でfixtureを組み立てるなら、`CreateIndex`と`BulkString`でも同じ準備ができます。公式の`opensearch-go`クライアントを使うときは、クラスタをloopback portで公開します。

```go
c := osmem.New()
defer c.Close()
if err := c.CreateIndex("products", map[string]any{"mappings": map[string]any{"properties": map[string]any{
    "name": map[string]any{"type": "text", "fields": map[string]any{"keyword": map[string]any{"type": "keyword"}}},
}}}); err != nil {
    log.Fatal(err)
}
if err := c.Index("products", "1", map[string]any{"name": "Red Apple"}); err != nil {
    log.Fatal(err)
}
srv := c.MustServe()
defer srv.Close()

client, err := opensearchapi.NewClient(opensearchapi.Config{
    Client: opensearch.Config{Addresses: []string{srv.URL}},
})
if err != nil { log.Fatal(err) }
ctx := context.Background()
result, err := client.Search(ctx, &opensearchapi.SearchReq{
    Indices: []string{"products"},
    Body: strings.NewReader(`{"query":{"match":{"name":"apple"}}}`),
})
if err != nil { log.Fatal(err) }
```

schema・documents・queryはいずれも通常のOpenSearch REST操作です。実際のクライアントtransportをテストするときだけ`Serve`を使います。in-processのアサーションなら`c.Do`のほうが速く済みます。

## indexを書き換えるテストはcloneする

documentの追加・更新・削除、mappingの変更、indexの作成・削除を行うテストでは、それぞれ専用のcloneを使います。読み取り専用テストなら共有baseを直接検索できます。

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

## テストのライフタイムを選ぶ

- **テストごとに新しいクラスタ:** `osmemtest.New(t)`で空のクラスタを作り、そのテスト内でschemaとdataを準備します。隔離は完全ですが、初期化を繰り返します。
- **package内でseed済みbaseを1つ共有:** 上の`TestMain`で一度だけ読み込みます。読み取り専用テストは共有でき、書き込むテストは`osmemtest.CloneAndServe(t, base)`を使います(HTTP不要なら`Clone`)。
- **既存documentやmappingを書き換えるテスト:** cloneをforkとして扱います。seed済みbaseから始まり、最初の変更時にcopy-on-writeで対象indexが分離されます。closeすれば変更は破棄され、baseは変わりません。大きなindexへの初回writeは、O(1)のcloneよりずっと重くなり得ます。

並行テストではimmutableなbaseを共有し、書き換え可能なcloneは共有しないでください。

独立したテストなら、共通fixtureをそのテスト内で読み込めます。

```go
func TestIsolatedSearch(t *testing.T) {
    c := osmemtest.New(t)
    if err := c.LoadSeed("testdata/seed"); err != nil {
        t.Fatal(err)
    }
    res, err := c.Do(http.MethodPost, "/products/_search", `{"query":{"match_all":{}}}`)
    if err != nil || res.IsError() {
        t.Fatal(err, string(res.Body))
    }
}
```
