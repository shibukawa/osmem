# osmem と OpenSearch の互換性監査

- 監査日: 2026-09-15
- 比較対象: OpenSearch 2.19.1 と osmem 2.19.0
- 比較基点: `main` の `4fc6350` (`v0.1.2`)

## 概要

OpenSearch本体には広範な [2.19系REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/2.19/rest-api-spec/src/main/resources/rest-api-spec/test) があり、互換性調査のケースソースとして有用です。`search`、aggregation、highlight、PIT、scroll、bulk、mapping、`msearch` などを扱っています。テスト形式は[2.19系のREADME](https://github.com/opensearch-project/OpenSearch/blob/2.19/rest-api-spec/src/main/resources/rest-api-spec/test/README.md)に説明されています。

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

## 追加調査で直接再現した差

2026-09-14に、OpenSearch 2.19.1とosmem 2.19.0をローカルHTTPサーバーとして起動し、同じindex設定・文書・RESTリクエストを両方へ送って比較しました。書き込み後はrefreshを行い、`took`などの変動値ではなく設定結果、応答フィールド、scoreの関係、結果集合の差を確認しています。以下は比較基点 `4fc6350` で再現できた追加差です。表中の「OS」はOpenSearchを指します。現在の作業ツリーでの対応状況は次節に記載します。

| 分野 | 再現した入力・操作 | OpenSearch 2.19.1 | osmem 2.19.0 | 影響 |
|---|---|---|---|---|
| `_source` | `_source.enabled:false` または `_source.excludes:["private"]` のmappingで文書を保存してGET/search | sourceを非表示、または除外フィールドを返さない | `_source` mapping設定を無視して元のsourceを返す | source抑止・除外をレスポンスからの秘匿手段に使うと、秘匿フィールドが返る |
| stored fields | `_source.enabled:false` と `public` の `store:true` を指定し、`stored_fields=public` でGET/search | 保存フィールドを `fields.public` として返す | GETはsourceを返し、searchは指定フィールドを返さない | stored fieldを使うクライアントで結果が欠け、GETではsourceが漏れる |
| mapping無効化 | root mapping `enabled:false` で混在型の任意sourceをindex | 201、dynamic field mappingを作らない | 混在値のdynamic mappingを試み、400になるケースを確認 | raw payloadの保管用途で書き込み可否が異なる |
| dynamic mapping | `dynamic:"runtime"` を含むmappingを作成 | 400（2.19.1ではbooleanへ変換できない） | 200、通常のdynamic fieldとしてmapping | 受理可否と後続検索でのfieldの扱いが異なる |
| doc values | keywordを `doc_values:false` にし、sortまたはterms aggregation | fielddata非対応として400 | 成功し、source値からsort/bucketを生成 | OpenSearchで失敗するクエリが osmem では成功し、負荷・意味が変わる |
| routing | `_routing.required:true` でroutingなしの書き込み、routingなしGET | 書き込み・GETとも失敗 | 書き込みを受理し、routingなしGETも成功 | routing必須制約とroutingによる文書到達性を再現しない |
| hit metadata | `term` queryに `_name` を付ける | hitに `matched_queries:["named-x"]` | `matched_queries` を返さない | named queryに依存した後処理が機能しない |
| scoring | `boosting` のnegative query/`negative_boost` と、両節に一致する文書を使った `dis_max.tie_breaker` | negative scoreを減衰し、tie breakerに応じて両節一致scoreを増加 | どちらのオプションもscoreに反映しない | 順位とscoreが異なる |
| sequence/shards metadata | 初回書き込みと、default replica設定でのwrite response | 初回 `_seq_no` は0、`_shards.total` は2 | 初回 `_seq_no` は1、`_shards.total` は常に1 | 同期・複製状態をmetadataで判断するクライアントに差が出る |
| immutable mapping | `norms:false` で作成後、同じfieldを `norms:true` に更新 | 400（parameter conflict） | 200 | 不可能なmapping変更を成功扱いする |
| alias / create | 存在しない具体index宛てに `require_alias=true` で書き込み | 404、indexを作らない | optionを無視してindexを作成、201 | alias必須運用が破られ、意図しないindexが生成される |
| `_id` 制限 | UTF-8で513 byteのIDを指定して書き込み | 400（512 byte上限） | 201 | 同じ入力の受理可否が異なる |
| versioned delete | 現version 5の文書を `version_type=external&version=4` でDELETE、その後削除済み文書をversion 5で再作成 | DELETEは409で文書維持。version 6で削除後もtombstoneによりversion 5の再作成は409 | 古いversionのDELETEで削除し、その後version 5で再作成可能 | 古いイベントや再送が現行文書を消す・復活させる可能性がある |
| mapping limits | `index.mapping.total_fields.limit:1` で2つのdynamic fieldをindex、または `index.mapping.nested_objects.limit:1` でnested childを2つ投入 | どちらも400 | どちらも201 | indexのfield数・nested object数上限が強制されない |
| sampler aggregation | 101文書をindexして既定の `sampler` の下でterms aggregation | sampleは100文書 | 101文書すべて | samplerの件数・bucketが異なり、サンプリング用途にならない |
| diversified sampler | 値 `same,same,other` の3文書を既定の `diversified_sampler` で集計 | sample `doc_count` は2、child bucket `same` は1 | sample `doc_count` は3、child bucket `same` は2 | 既定の重複値制限を適用せず、sampleとbucketの件数が異なる |
| composite aggregation | 2つのterms sourceでキー `ab`,`c` と `a`,`bc` の組を集計 | 2 bucket | キーの連結が衝突し、count 2の1 bucket | composite bucketのキーと件数を誤る |
| histogram order | histogram aggregationに `order:{"_key":"desc"}` | bucket key降順 | bucket key昇順 | バケット順序を指定するクライアントでページング・表示が異なる |
| highlight | queryは `body` に一致し、`title` に同じ語がある文書で `require_field_match:false` を指定 | `title` にhighlightを返す | `title` にhighlightを返さない | query fieldと異なるfieldを強調する検索画面で表示が欠ける |
| weighted average | 1文書の値を10、weightを複数値 `[2,3]` にして `weighted_avg` | 500（weightが複数値というエラー） | 200、値10を返す | multi-valued weightの不正入力を拒否せず、異なる集計結果になる |
| analyzer | `asciifolding` filterに `café résumé` を入力 | `cafe`, `resume` | accentを保持した `café`, `résumé` | analyzer結果、term検索、aggregation keyが異なる |
| analyzer | `ngram` tokenizerに `token_chars:["letter"]` を指定して `ab12cd` を解析 | `ab`, `cd` | `ab12cd` | token_charsによる分割を再現しない |

この追加プローブでは `date_histogram` の既定 `min_doc_count` と、weight欠落を含む `weighted_avg` は両実装で同じ結果でした。`weighted_avg` の複数値weightだけは上表のとおり差を再現しています。これら以外に、`bool.should` と明示的な `minimum_should_match:0` も今回の直接比較では差を再現できず、PITの期限切れも未確定のままです。

この結果は上記2バージョンと各プローブ条件での比較です。未試験のparameter組み合わせや他のOpenSearchバージョンに一般化する前に、再現リクエストを回帰fixtureとして保存する必要があります。関連仕様はOpenSearchの[`_source`](https://docs.opensearch.org/2.19/field-types/metadata-fields/source/)、[stored fields](https://docs.opensearch.org/2.19/field-types/mapping-parameters/store/)、[doc values](https://docs.opensearch.org/2.19/field-types/mapping-parameters/doc-values/)、[routing](https://docs.opensearch.org/2.19/field-types/metadata-fields/routing/)、[`_id`](https://docs.opensearch.org/2.19/field-types/metadata-fields/id/)、[boosting query](https://docs.opensearch.org/2.19/query-dsl/compound/boosting/)、[dis_max query](https://docs.opensearch.org/2.19/query-dsl/compound/disjunction-max/)、[highlight options](https://docs.opensearch.org/2.19/search-plugins/searching-data/highlight/)、[histogram](https://docs.opensearch.org/2.19/aggregations/bucket/histogram/)、[sampler](https://docs.opensearch.org/2.19/aggregations/bucket/sampler/)、[diversified sampler](https://docs.opensearch.org/2.19/aggregations/bucket/diversified-sampler/)、[weighted average](https://docs.opensearch.org/2.19/aggregations/metric/weighted-avg/)、[composite aggregation](https://docs.opensearch.org/2.19/aggregations/bucket/composite/)、[asciifolding token filter](https://docs.opensearch.org/2.19/analyzers/token-filters/asciifolding/)、[ngram tokenizer](https://docs.opensearch.org/2.19/analyzers/tokenizers/ngram/) などに記載されています。

## 追加修正の状況

次の差は作業ツリーで実装し、対応するGo回帰テストを追加しました。個々のケースはOpenSearchでの基準動作をもとにしていますが、下記は新しく追加したosmem側の回帰確認結果です。

| 分野 | 対応した差 | 回帰テスト |
|---|---|---|
| 文書メタデータ | 初回 `_seq_no:0`、replica設定を含むwrite `_shards.total`、512 byte `_id` 上限 | `TestCompatibilityRegressionFixes` |
| mapping/source | root `enabled:false`、`dynamic:runtime`拒否、mapping-level `_source` include/exclude/disable、stored fields、`doc_values:false` sort/aggregation拒否、reindex時のsource filter | `TestRootMappingEnabledFalseStoresSourceWithoutDynamicMapping`, `TestDynamicRuntimeRejected`, `TestMappingSourceSettingsApplyToGetAndSearch`, `TestReindexHonorsMappingSourceFiltering`, `TestReindexRejectsIndexWithoutSource` |
| alias/routing | `require_alias`の具体index・未存在対象の拒否、`_routing.required`の欠落検査 | `TestRequireAliasAndRequiredRouting` |
| mapping limits | dynamic mappingでの`total_fields.limit`、1 source documentあたりの`nested_objects.limit`、`norms:false → true`拒否と`true → false`許可 | `TestMappingLimitsAndImmutableNorms` |
| query/PIT | should句だけの明示`minimum_should_match:0`、PIT期限切れと検索時の`keep_alive`延長 | `TestBoolMinimumShouldMatchZeroCompatibility`, `TestPITSearchExpiresAndExtendsKeepAlive` |
| document/bulk/reindex | noop updateでもOCCを検証、Bulkのaction `_require_alias`と終端改行、Reindexの`max_docs`とexternal source version | `TestUpdateNoopStillChecksOCC`, `TestBulkCompatibilityChecksAndReindexLimitsVersions` |
| aggregation | composite key衝突、histogram/date_histogram順序、複数値weightの500集計エラー、sampler既定上限とdiversified samplerの値上限 | `aggregation_compatibility_test.go` |
| analyzer | `café résumé`のASCII folding、ngram `token_chars`境界 | `TestAsciifoldingTokenFilterRemovesAccents`, `TestNgramTokenizerRespectsTokenChars` |

`_routing.required`ではrouting未指定を拒否しますが、routing値そのものは文書キーに含めていません。同じ`_id`を異なるroutingで書いても別文書にはならず、誤ったroutingでのGETも既存文書を返します。この仕様まで合わせるには、文書storeを`(routing, id)`で管理し、bulk・mget・update・delete・reindexを通じてroutingを保持する変更が必要です。

`sampler`と`diversified_sampler`は、osmemの単一検索ストリーム内での既定サンプルと値ごとの上限を実装しています。OpenSearchの独立したshardごとのサンプル、複数shard間のマージは再現しません。

## OpenSearch 2.19ソース監査で追加確認した差

2026-09-15にOpenSearch 2.19のmapper実装・mapper unit testsとAPI仕様を追加確認し、以下の差を見つけて修正しました。ここに追加したGoテストはosmem側の回帰確認です。各ケースをOpenSearch 2.19サーバーへ再送するHTTP比較はまだ行っていません。

| 差 | OpenSearch 2.19の動作 | 修正と回帰確認 |
|---|---|---|
| immutable mapping parameters | 既存fieldの`index`、`index_options`、`store`、`doc_values`、`null_value`、`similarity`、`normalizer`、`term_vector`の変更を拒否。既存object/rootの`enabled`とnestedの`include_in_parent` / `include_in_root`も変更不可。 | 明示された値が既存値と異なるmapping更新を拒否。`TestMappingRejectsImmutableParameters`。根拠: [FieldMapper.java](https://github.com/opensearch-project/OpenSearch/blob/2.19/server/src/main/java/org/opensearch/index/mapper/FieldMapper.java#L397-L438), [KeywordFieldMapperTests.java](https://github.com/opensearch-project/OpenSearch/blob/2.19/server/src/test/java/org/opensearch/index/mapper/KeywordFieldMapperTests.java#L1461-L1505), [ObjectMapper.java](https://github.com/opensearch-project/OpenSearch/blob/2.19/server/src/main/java/org/opensearch/index/mapper/ObjectMapper.java#L126-L139)。 |
| `enabled` | top-level mappingと既存object fieldでは変更不可。`enabled:false`のmappingに`enabled`を省略した更新を送ると、merge後の既定値trueとの差として拒否される。 | root/object両方の更新を`mapper_exception`で拒否。`TestMappingRejectsImmutableParameters`。根拠: [enabled docs](https://docs.opensearch.org/2.19/mappings/mapping-parameters/enabled/), [ObjectMapper.java](https://github.com/opensearch-project/OpenSearch/blob/2.19/server/src/main/java/org/opensearch/index/mapper/ObjectMapper.java#L738-L750)。 |
| `norms` | falseからtrueへの変更は拒否、trueからfalseへの変更は許可。 | 一方向の制約を実装。`TestMappingLimitsAndImmutableNorms`。根拠: [FieldMapper.java](https://github.com/opensearch-project/OpenSearch/blob/2.19/server/src/main/java/org/opensearch/index/mapper/FieldMapper.java#L414-L416)。 |
| DELETEのoptimistic concurrency control | `if_seq_no`と`if_primary_term`をペアで指定する。osmemはDELETEで片方だけ渡された条件を見落とし、文書を削除できていた。 | DELETEでもペア指定を要求し、片方欠落は400、古いseq_noは409。`TestDocumentAPIs`。根拠: [Delete Document API](https://docs.opensearch.org/2.19/api-reference/document-apis/delete-document/)。 |
| `_settings/{setting}` | setting名またはwildcardを指定すると該当する設定だけを返す。 | exact名とwildcard、flat/non-flatの絞り込みを実装。`TestIndexManagement`。根拠: [Get Settings API](https://docs.opensearch.org/2.19/api-reference/index-apis/get-settings/)。 |
| cluster health shard accounting | active primary/total/unassigned shardsとstatusはindex数ではなく設定shard数とreplica割当状態から算出する。 | 1 data nodeのモデルに合わせ、全primaryをactive、replicaをunassignedとして集計。`TestClusterHealthReflectsConfiguredShardCopies`。根拠: [Cluster Health API](https://docs.opensearch.org/2.19/api-reference/cluster-api/cluster-health/)。 |
| `_update`のnoop応答 | 内容が同じ更新は通常書き込みと同じ`_shards`数を返していました。OpenSearchのnoop応答では`total`、`successful`、`failed`が0です。 | stale OCC条件を検証した後にzero-shard応答を返すよう修正。`TestUpdateNoopStillChecksOCC`。根拠: [Update Document API](https://docs.opensearch.org/2.19/api-reference/document-apis/update-document/)。 |
| `_update`のnoopとOCC | 内容が同じ更新はnoop判定で先に返り、古い`if_seq_no` / `if_primary_term`が検証されませんでした。 | noopでもwrite conditionを検証するよう修正。`TestUpdateNoopStillChecksOCC`。根拠: [Update Document API](https://docs.opensearch.org/2.19/api-reference/document-apis/update-document/)。 |
| Bulkのaction metadataとNDJSON | action metadataの`_require_alias`を無視し、最終改行がないNDJSONも受理していました。 | action単位のalias必須検査と終端改行検査を実装。`TestBulkCompatibilityChecksAndReindexLimitsVersions`。根拠: [Bulk API](https://docs.opensearch.org/2.19/api-reference/document-apis/bulk/)。 |
| Reindexの上限・version | top-level `max_docs`と`dest.version_type`を反映していませんでした。 | `max_docs`で処理件数を制限し、`external` / `external_gte`ではsource versionをdestinationへ渡します。同じ回帰テストで確認。根拠: [Reindex API](https://docs.opensearch.org/2.19/api-reference/document-apis/reindex/)。 |

## 未修正項目と理由

| 差 | 残した理由 |
|---|---|
| `boosting.negative` / `negative_boost`、`dis_max.tie_breaker` | Bleveの標準DisjunctionではLuceneのスコア式にならず、ヒットごとにpositive/negative clauseのスコアを合成するクエリ実装が必要です。BM25スコア差の影響も切り分ける必要があります。 |
| `_name` と `matched_queries` | DSLを解析する時点で名前を保持し、各ヒットに対して名前付き節の一致を評価して応答へ合成する仕組みがありません。query builderとhit生成の双方へ横断的な追跡を加える必要があります。 |
| `highlight.require_field_match:false` | 現在のハイライトは検索句のBleve match locationsを使います。指定fieldでquery termを再評価し、別fieldのoffsetを安全に合成する必要があります。 |
| versioned deleteとtombstone | 削除後にもexternal versionとsequence metadataを保持するtombstone storeが必要で、再作成・bulk・update-by-queryのversion判定へ影響します。 |
| Painless scripts | `script_score`、scripted update、scripted aggregationを正確に実行するにはPainlessのparser/runtimeとOpenSearch互換のsandbox、context別APIが必要です。現在の依存にGo向け互換runtimeはなく、Java runtimeを呼び出すだけでも安全性と配布形態を含む別プロジェクト規模になるため、unsupported errorを維持します。 |
| mapping immutabilityの網羅性 | OpenSearch 2.19のsourceでimmutableと確認できた主要parameterを回帰テストにしました。他type-specific parameterと、明示値を省略したときのmerge挙動はREST fixtureや同バージョンサーバーとの比較でさらに検証する必要があります。 |
| `include_defaults=true` | setting defaultsはOpenSearchのversion・plugin・index setting registryに依存します。個別defaultを足すだけでは不完全なAPIになるため、version単位のdefault catalogなしでは保留しました。 |
| `/{index}/_stats/{metric}` | metric pathは無視され、現状は`docs`と`store`だけを返します。OpenSearchは`search`、`indexing`、`segments`などmetric groupを選べます。osmemは検索・indexing counterやshard-level statsを保持していません。根拠: [Index Stats API](https://docs.opensearch.org/2.19/api-reference/index-apis/stats/)。 |
| `/_field_caps` | Field Capabilities APIのrouteがありません。OpenSearchは複数indexを横断し、同じfieldのtype差、searchable / aggregatable可否を返します。mappingとindexごとのcapabilityを統合する処理が必要です。根拠: [Field Capabilities API](https://docs.opensearch.org/2.19/api-reference/search-apis/field-caps/)。 |
| `/{index}/_validate/query` | routeがありません。OpenSearchは検索前にquery DSLを検証し、`valid`と必要に応じて説明を返します。根拠: [Validate Query API](https://docs.opensearch.org/2.19/api-reference/search-apis/validate/)。 |
| `/{index}/_explain/{id}` と `explain:true` | Explain APIのrouteがなく、Searchの`explain:true`も受理後に無視されます。OpenSearchが返す文書ごとの一致判定とscore説明は得られません。根拠: [Explain API](https://docs.opensearch.org/2.19/api-reference/search-apis/explain/), [Search API](https://docs.opensearch.org/2.19/api-reference/search-apis/search/)。 |
| `/{index}/_termvectors` | routeがありません。term frequency・position・offsetなどの文書/term vectorを取得できません。mappingの`term_vector`設定だけではこのAPIの代わりになりません。根拠: [Term Vectors API](https://docs.opensearch.org/2.19/api-reference/document-apis/termvector/)。 |
| Search template APIs | `/_search/template`、`/_msearch/template`、`/_render/template`とstored search scriptのrouteがありません。Mustache templateの保存、描画、実行はできません。根拠: [Search Templates API](https://docs.opensearch.org/2.19/api-reference/search-apis/search-template/)。 |
| `/{index}/_rank_eval` | routeがありません。rated query/documentからNDCG・MRRなどを計算する検索評価APIは未対応です。根拠: [Ranking Evaluation API](https://docs.opensearch.org/2.19/api-reference/search-apis/rank-eval/)。 |
| `function_score` | 内側queryだけが評価され、`functions`、`weight`、`score_mode`、`boost_mode`、`max_boost`などのscore調整が無視されます。スコア関数を含む再スコア処理が必要です。根拠: [function_score query](https://docs.opensearch.org/2.19/query-dsl/compound/function-score/)。 |
| `geo_distance` parameters | `distance_type`、`validation_method`、`ignore_unmapped`を無視します。未mapping fieldではOpenSearchがエラーにする条件でもosmemは空結果になり、arc/planeや座標validationも異なります。根拠: [geo distance query](https://docs.opensearch.org/2.19/query-dsl/geo-and-xy/geodistance/)。 |
| `regexp.flags` | Lucene regexp flagsをBleve regexpへ適用せず、たとえばintersection演算子`&`を含む式の意味が異なります。根拠: [Regular expression syntax](https://docs.opensearch.org/2.19/query-dsl/regex-syntax/)。 |
| `fuzzy.max_expansions` / `transpositions` | どちらも無視し、BleveのLevenshtein距離を使います。OpenSearchはtranspositionsを既定で許し、expansion候補数も制御します。根拠: [fuzzy query](https://docs.opensearch.org/2.19/query-dsl/term/fuzzy/)。 |
| terms aggregationの`shard_size` | optionは受理されても候補term数に影響しません。単一in-memory streamの全件集計なので、OpenSearchのshardごとの候補選択・マージ時の件数精度と異なります。根拠: [terms aggregation](https://docs.opensearch.org/2.19/aggregations/bucket/terms/)。 |
| Reindexのinvalid parameter | `max_docs`は数値変換後に整数化され、fraction・string・booleanが受理される場合があります。未知の`dest.version_type`も`internal`相当として扱います。根拠: [Reindex API](https://docs.opensearch.org/2.19/api-reference/document-apis/reindex/)。 |
| Search options | `indices_boost`、`stats`、`slice`、`search_pipeline`は受理されますが効果がありません。index単位boost、統計group、slice分割、search pipelineはいずれも未実装です。 |
| cluster health detail | osmemは単一nodeです。primary/replicaの集計までは計算しますが、`level=indices` / `level=shards`、wait条件、allocation状態を持つ本物のcluster stateはありません。 |
| 全UnicodeのASCII folding一致 | 既存BleveにはLucene由来のfolding tableがあり、一般的なaccentのケースはそれをtoken filterとして使うよう修正しました。Unicode全域のLuceneバージョン別差分は、この2語の回帰ケースだけでは保証できません。追加packageは不要です。 |

`bool.should`の`minimum_should_match:0`は以前の直接比較では差を再現できませんでしたが、実装上Bleveがshould句を暗黙に必須にする条件が見つかったため修正しました。PIT expiry/延長も固定時計テストで実装を確認しています。一方、`keep_alive=1s`のPITを6秒後に検索した先行HTTPプローブは両サーバーが200を返しており、OpenSearchとの期限境界は再比較が必要です。

## テスト集の取り込み方

1. OpenSearch 2.19系のREST YAMLケースを固定し、search、mapping、文書API、aggregation、PIT/scrollから対象を選ぶ。
2. YAMLのsetup、操作、assertionを同じ順でOpenSearchとosmemへ送る差分ランナーを作る。
3. `took` などの変動値は無視し、BM25など既知の近似は別判定にする。
4. 今回追加したケースと、`nested_test.go` でOpenSearch応答を構造比較する仕組みを広げる。

全ケースを無条件で実行するのではなく、shard構成・plugin依存のケースとosmem非対応APIを分類してから取り込む必要があります。

## 検証

`GOCACHE=/private/tmp/osmem-gocache go test ./...`、追加ケースを絞ったfocused test、`git diff --check`が成功しました。sandbox内では既存のHTTP serverテストがlocalhostの一時portを開けなかったため、ローカルnetworkを許可した実行で全パッケージを確認しています。`website`ディレクトリの`npm run build`も成功しました。
