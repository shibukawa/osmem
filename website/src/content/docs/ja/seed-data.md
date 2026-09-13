---
title: "シードデータ"
description: "ベースクラスタをシードディレクトリやbulkファイルとして記述するか、APIで投入してから凍結する。"
---

ベースクラスタは、ファイルとして記述しておく価値があります。マッピングの変更をプルリクエストでレビューでき、Go、Node.js、Python、Javaのテストスイートが1つのフィクスチャを共有できるからです。osmemは、決まったレイアウトのシードディレクトリか単一のbulkファイルを読み込みます。ベースを凍結する前であれば、通常のAPIで投入することもできます。

## ディレクトリのレイアウト

ファイルは次の順に適用され、各段階の中ではファイル名順です。

| ファイル | リクエスト |
|---|---|
| `<name>.template.json` | `PUT /_index_template/<name>` |
| `<index>.index.json` | `PUT /<index>`(settings、mappings、aliases) |
| `<index>.ndjson` | `POST /<index>/_bulk` |
| `aliases.json` | `POST /_aliases` |

テンプレートを先に適用するのは、その後に作られるインデックスへ反映させるためです。エイリアスを最後にするのは、投入済みのインデックスを指せるようにするためです。bulkのアイテムが1つでも失敗すると、そのエラーで読み込みは中断します。

小さなカタログの例です。

```
testdata/seed/
├── products.index.json
├── products.ndjson
└── aliases.json
```

```json
{"mappings": {"properties": {
  "name":  {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
  "price": {"type": "double"},
  "tags":  {"type": "keyword"},
  "created": {"type": "date"}
}}}
```

```
{"index": {"_id": "1"}}
{"name": "Red Apple", "price": 1.5, "tags": ["fruit", "red"], "created": "2024-01-05T10:00:00Z"}
{"index": {"_id": "2"}}
{"name": "Banana", "price": 0.5, "tags": ["fruit"], "created": "2024-02-20"}
```

```json
{"actions": [{"add": {"index": "products", "alias": "catalog"}}]}
```

NDJSONは`_bulk`のボディそのもので、アクション行とドキュメント行が交互に並びます。インデックスはファイル名がデフォルトで、アクションに`_index`を書けば別のインデックスへ書き込めます。マッピングにないフィールドは、OpenSearchと同じく動的にマッピングされます。

## 単一のbulkファイル

`.ndjson`で終わるパスは、1つの`_bulk`ボディとして読み込まれます。この場合はすべてのアクションに`_index`が必要で、存在しないインデックスは動的マッピングで作られます。

## APIで投入する

フィクスチャをテストコードで組み立てるほうが楽なら、任意のクライアントでベースに書き込み、それから凍結します。

```bash
curl -XPOST http://127.0.0.1:PORT/_osmem/base/freeze
```

凍結は、最初のクローンが作られたときにも暗黙に行われます。以降、ベースへの書き込みは403 `osmem_base_frozen`になります。sessionのfixtureでシードして凍結を忘れたスイートでも、最初のテストがクローンを取った時点からは守られるわけです。

## 読み込み方

- Go: `c.LoadSeed("testdata/seed")`または`c.LoadSeed("dump.ndjson")`。
- `osmem-server --seed testdata/seed --seed extra.ndjson`。複数指定でき、指定順に適用されます。各言語のパッケージは、`seed`オプションをそのまま渡します。

## フィクスチャを役立つ状態に保つ習慣

テストから`GET`できるよう、ドキュメントには明示的なidを付けます。ベースは小さく保ちます。テストが初めて書き込んだとき、クローンはそのインデックスを再インデックスし、1ドキュメントあたり約40マイクロ秒かかるからです。`strict_date_optional_time`形式の日付(`2024-02-20`、`2024-01-05T10:00:00Z`)なら、マッピングに`format`は要りません。`kuromoji`を使うマッピングが正しく解析されるのは、日本語サポートが有効な場合だけです(Goでは`ja`のimport、`osmem-server`ではデフォルトで有効)。無効ならCJKのbigramにフォールバックし、活用形での検索が当たらなくなります。
