# osmem と OpenSearch の互換性監査

- 監査日: 2026-09-14
- 比較対象: OpenSearch 2.19.1 と osmem 2.19.0
- 反映先: `main` の `e8e2c39` を含む作業ツリー

## 概要

OpenSearch本体には広範な [REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/main/rest-api-spec/src/main/resources/rest-api-spec/test) があり、互換性調査のケースソースとして有用です。`search`、aggregation、highlight、PIT、scroll、bulk、mapping、`msearch` などを扱っています。テスト形式は[公式README](https://github.com/opensearch-project/OpenSearch/blob/main/rest-api-spec/src/main/resources/rest-api-spec/test/README.md)に説明されています。

YAMLケースはOpenSearch側のテストフレームワークで実行する形式なので、osmemへそのまま `go test` として取り込めるものではありません。OpenSearch 2.19系からosmemが対象とするAPIのケースを選び、同じリクエスト列を両方へ送る差分ランナーを用意するのが現実的です。osmemの既存ベンチマークはOpenSearch 2.19.0を指定しています（`bench/testcontainers/main.go`）。

## 直接比較で見つかり、今回修正した差

下表の「修正後」は追加した回帰テストで確認しています。collapse対応は作業開始時点の `main` に含まれていたため、今回の変更ではありません。

| ケース | OpenSearch 2.19.1 | osmemの修正後 | 回帰テスト |
|---|---|---|---|
| PIT作成後に文書を更新してPIT検索 | 作成時点の文書を返す | PITが作成時のindex状態を保持 | `TestScrollAndPIT` |
| `search_after` の2ページ目、全5件 | `hits.total = 5` | ページング後も `hits.total = 5` | `TestSortingAndPaging` |
| collapse、全5文書を2グループに集約 | 2 hits、totalは5 | mainのnested/collapse変更で同じ挙動 | `nested_test.go` |
| `dynamic_templates` で文字列をkeyword化 | 動的フィールドが `keyword` | dynamic templateを適用 | `TestCompatibilityRegressionFixes` |
| `coerce:false` の整数fieldへ文字列を投入 | 400で拒否 | 400で拒否 | `TestCompatibilityRegressionFixes` |
| `long` の `2^53` と `2^53+1` をterm検索 | 後者の文書だけ一致 | 整数の完全値を使い、後者だけ一致 | `TestCompatibilityRegressionFixes` |
| text fieldに `fielddata:true` を設定してsort | 成功 | 解析後のtermでsort | `TestCompatibilityRegressionFixes` |
| `terminate_after:1`、単一shardで3文書を検索 | 1 hit、`terminated_early:true` | 1 hit、`terminated_early:true` | `TestQueryErrors` |
| URLパラメータ `?from=-1` / `?size=-1` | 400 | 400。slice panicを防止 | `TestQueryErrors` |
| 3 primary shard、0 replicaで検索と書き込み | search total=3、write total=1 | search total=3、write total=1 | `TestCompatibilityRegressionFixes` |

PITの仕様、dynamic template、coercion、`long` の整数幅、fielddataの用途はOpenSearchの[Point in Time](https://docs.opensearch.org/latest/search-plugins/searching-data/point-in-time/)、[Mappings](https://docs.opensearch.org/latest/mappings/)、[coerce](https://docs.opensearch.org/latest/mappings/mapping-parameters/coerce/)、[numeric field types](https://docs.opensearch.org/latest/opensearch/supported-field-types/numeric)、[fielddata](https://docs.opensearch.org/latest/mappings/mapping-parameters/field-data/)資料に記載されています。

## 残っている差・近似

- **`script_score`。** OpenSearchはスクリプトを実行して `_score` を返します。osmemはスクリプトを実行せず、今回から誤った成功応答ではなく `unsupported_operation_exception` (400) を返します。無言で内側queryだけを実行する挙動はなくしましたが、OpenSearchとの機能差は残ります。
- **複数shardの`terminate_after`。** 現在は単一の全体上限として扱います。OpenSearchのshardごとの早期終了を複数shardで完全には再現しません。
- **整数のrangeとsort。** term検索には完全精度を追加しましたが、Bleveの数値fieldと一部の比較処理は`float64`を使います。`long`の大きな値のrange検索・sort、数値型境界、`scaled_float`は追加検証が必要です。
- **`fielddata:true`のaggregation。** text fieldのsortは対応しましたが、text field aggregationは引き続き拒否します。
- **受理して無視するsearchオプション。** `timeout`、`profile`、`rescore`、`script_fields` などは実装されていないものがあります。成功応答だけでOpenSearchと互換と判定しないでください。
- **`ignore_malformed:true`。** mappingでは受理されますが、不正なフィールド値だけを除いて文書全体を保存するOpenSearchの動作は未対応です。現在は不正値で文書全体が拒否されることがあります。
- **事前集計した文書数。** bucket aggregationは`_doc_count`を使わないため、事前集計済み文書の件数はOpenSearchと一致しません。
- **既存の文書化済み近似。** BM25スコア、analyzer、cardinality/percentilesの厳密さ、refresh可視性などは[互換性ページ](website/src/content/docs/compatibility.md)を参照してください。nested objectとcollapseは今回の基点commitで大きく改善されています。

`keep_alive=1s` のPITを6秒後に検索する試験は、OpenSearchとosmemの両方が200を返しました。この試験では期限切れの差を確定していません。

## テスト集の取り込み方

1. OpenSearch 2.19系のREST YAMLケースを固定し、search、mapping、文書API、aggregation、PIT/scrollから対象を選ぶ。
2. YAMLのsetup、操作、assertionを同じ順でOpenSearchとosmemへ送る差分ランナーを作る。
3. `took` などの変動値は無視し、BM25など既知の近似は別判定にする。
4. 今回追加したケースと、`nested_test.go` でOpenSearch応答を構造比較する仕組みを広げる。

全ケースを無条件で実行するのではなく、shard構成・plugin依存のケースとosmem非対応APIを分類してから取り込む必要があります。

## 検証

`GOCACHE=/private/tmp/osmem-gocache go test ./...` が成功しました。sandbox内では既存のHTTP serverテストがlocalhostの一時portを開けなかったため、ローカルnetworkを許可した実行で全パッケージを確認しています。
