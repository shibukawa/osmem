# osmem と OpenSearch の互換性監査

- 監査日: 2026-09-15
- 最新の実サーバー比較: OpenSearch **3.8.0**、32ケース一致（末尾の追加検証を参照）
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
| mapping limits | dynamic `total_fields.limit`、mapping `depth.limit`、異なるnested field数の`nested_fields.limit`、文書ごとの`nested_objects.limit`、`norms`の変更方向 | `TestMappingLimitsAndImmutableNorms`, `mapping_limits_compatibility_test.go` |
| query/PIT | should句だけの明示`minimum_should_match:0`、PIT期限切れと検索時の`keep_alive`延長 | `TestBoolMinimumShouldMatchZeroCompatibility`, `TestPITSearchExpiresAndExtendsKeepAlive` |
| document/bulk/reindex | noop update OCC, Bulk `_require_alias` and final newline, strict `max_docs`, write enum validation, and `external_gt` / external source-version handling | `TestUpdateNoopStillChecksOCC`, `TestBulkCompatibilityChecksAndReindexLimitsVersions`, `TestWriteEnumValidationAndExternalGT` |
| REST query APIs | `_field_caps` reports mapped types/capabilities and supports `include_unmapped`; Validate Query checks supported DSL; `_resolve/index` reports modeled indices and aliases, applying open/hidden filters to wildcard expansion; template simulation routes are read-only. Their remaining partial behavior is listed below. | `TestFieldCapsAcrossIndices`, `TestValidateQueryAPI`, `TestResolveIndexForIndicesAndAliases`, `TestResolveIndexWildcardExpansion`, `TestSimulateIndexTemplateDoesNotWriteTemplate` |
| Settings and validation | `preserve_existing=true` applies only new settings; invalid write `op_type` / `version_type`, external version writes without a version, and invalid `dynamic` values are rejected; `ignore_above:0` remains present in mapping JSON and skips nonempty values. | `TestPreserveExistingSettingsAndDynamicEnumValidation`, `TestExternalVersionRequiresVersion`, `TestIgnoreAbove` |
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
| Reindexの上限・version | top-level `max_docs`とdestination versionを反映していませんでした。 | `max_docs`を整数として検証し、`external` / `external_gt` / `external_gte`でsource versionをdestinationへ渡します。`external_gt`は厳密なgreater-than比較です。`TestWriteEnumValidationAndExternalGT`。根拠: [Reindex API](https://docs.opensearch.org/2.19/api-reference/document-apis/reindex/)。 |

## 未実装・部分対応の項目と理由

| 差 | 残した理由 |
|---|---|
| `boosting.negative` / `negative_boost`、`dis_max.tie_breaker` | Bleveの標準DisjunctionではLuceneのスコア式にならず、ヒットごとにpositive/negative clauseのスコアを合成するクエリ実装が必要です。BM25スコア差の影響も切り分ける必要があります。 |
| `_name` と `matched_queries` | DSLを解析する時点で名前を保持し、各ヒットに対して名前付き節の一致を評価して応答へ合成する仕組みがありません。query builderとhit生成の双方へ横断的な追跡を加える必要があります。 |
| `highlight.require_field_match:false` | 現在のハイライトは検索句のBleve match locationsを使います。指定fieldでquery termを再評価し、別fieldのoffsetを安全に合成する必要があります。 |
| versioned deleteとtombstone | 削除後にもexternal versionとsequence metadataを保持するtombstone storeが必要で、再作成・bulk・update-by-queryのversion判定へ影響します。 |
| Painless scripts | `script_score`、scripted update、scripted aggregationを正確に実行するにはPainlessのparser/runtimeとOpenSearch互換のsandbox、context別APIが必要です。現在の依存にGo向け互換runtimeはなく、Java runtimeを呼び出すだけでも安全性と配布形態を含む別プロジェクト規模になるため、unsupported errorを維持します。 |
| mapping immutabilityの網羅性 | OpenSearch 2.19のsourceでimmutableと確認できた主要parameterを回帰テストにしました。他type-specific parameterと、明示値を省略したときのmerge挙動はREST fixtureや同バージョンサーバーとの比較でさらに検証する必要があります。 |
| `include_defaults=true` | setting defaultsはOpenSearchのversion・plugin・index setting registryに依存します。個別defaultを足すだけでは不完全なAPIになるため、version単位のdefault catalogなしでは保留しました。 |
| `/{index}/_stats/{metric}` | 実装済みの`docs` / `store` groupでfilterしますが、store sizeはplaceholderです。`search`、`indexing`、`segments`、cache、shard単位の統計は保持していません。 |
| `/_field_caps` partial support | Mapped field types, per-type searchable/aggregatable flags, and `include_unmapped` are implemented. `index_filter` returns an unsupported-operation error, mapping metadata merge semantics are not modeled, and rare field types with special capability rules need comparison against OpenSearch. |
| `/{index}/_validate/query` partial support | Validates supported query DSL and reports invalid-query errors. Successful `explain=true` explanations and `rewrite=true` output are missing; `all_shards` is only an approximation because index execution is not sharded. Root and index routes exist. |
| `/{index}/_explain/{id}` と `explain:true` | Explain APIのrouteがなく、Searchの`explain:true`も受理後に無視されます。OpenSearchが返す文書ごとの一致判定とscore説明は得られません。根拠: [Explain API](https://docs.opensearch.org/2.19/api-reference/search-apis/explain/), [Search API](https://docs.opensearch.org/2.19/api-reference/search-apis/search/)。 |
| `/{index}/_termvectors` | routeがありません。term frequency・position・offsetなどの文書/term vectorを取得できません。mappingの`term_vector`設定だけではこのAPIの代わりになりません。根拠: [Term Vectors API](https://docs.opensearch.org/2.19/api-reference/document-apis/termvector/)。 |
| Search template APIs | `/_search/template`、`/_msearch/template`、`/_render/template`とstored search scriptのrouteがありません。Mustache templateの保存、描画、実行はできません。根拠: [Search Templates API](https://docs.opensearch.org/2.19/api-reference/search-apis/search-template/)。 |
| `/_resolve/index/{name}` partial support | Concrete indices and aliases are returned. Data streams, closed indices, and full hidden-index expansion semantics are not modeled. Root endpoint is documented at [Resolve Index API](https://docs.opensearch.org/2.19/api-reference/index-apis/resolve-index/). |
| Component index templates | Component template APIs are not implemented. A composable template's `composed_of` components are not merged during index creation or simulation. |
| Index-template simulation partial support | The simulation endpoints are read-only, but inline/named simulations always return an empty overlap list; component templates are also not resolved or merged. |
| `/_list/indices`, `/_list/shards` | Missing. These OpenSearch 2.18+ endpoints expose paginated index/shard state; cursor pagination, formatted tabular output, and replica allocation state are not modeled. Root references: [List APIs](https://docs.opensearch.org/2.19/api-reference/list/). |
| `/_search_shards` | Missing. OpenSearch returns shard routing and node allocation details; osmem has no actual per-shard node routing to report. Root reference: [Search Shards API](https://docs.opensearch.org/2.19/api-reference/search-apis/search-shards/). |
| `/{index}/_rank_eval` | routeがありません。rated query/documentからNDCG・MRRなどを計算する検索評価APIは未対応です。根拠: [Ranking Evaluation API](https://docs.opensearch.org/2.19/api-reference/search-apis/rank-eval/)。 |
| `function_score` | 内側queryだけが評価され、`functions`、`weight`、`score_mode`、`boost_mode`、`max_boost`などのscore調整が無視されます。スコア関数を含む再スコア処理が必要です。根拠: [function_score query](https://docs.opensearch.org/2.19/query-dsl/compound/function-score/)。 |
| `geo_distance` parameters | `ignore_unmapped`と座標検証 / `COERCE`は実装済みです。`distance_type:plane`は無視し（Bleveのarc距離を使う）、`IGNORE_MALFORMED`の全挙動は再現しません。 |
| `regexp.flags` | Lucene regexp flagsをBleve regexpへ適用せず、たとえばintersection演算子`&`を含む式の意味が異なります。根拠: [Regular expression syntax](https://docs.opensearch.org/2.19/query-dsl/regex-syntax/)。 |
| `fuzzy.max_expansions` / `transpositions` | どちらも無視し、BleveのLevenshtein距離を使います。OpenSearchはtranspositionsを既定で許し、expansion候補数も制御します。根拠: [fuzzy query](https://docs.opensearch.org/2.19/query-dsl/term/fuzzy/)。 |
| terms aggregationの`shard_size` | optionは受理されても候補term数に影響しません。単一in-memory streamの全件集計なので、OpenSearchのshardごとの候補選択・マージ時の件数精度と異なります。根拠: [terms aggregation](https://docs.opensearch.org/2.19/aggregations/bucket/terms/)。 |
| `dynamic` のtemplate限定モード | OpenSearch 2.19は`strict_allow_templates`と`false_allow_templates`を受け付けます。osmemはこの2種類のtemplate適用規則を実装せず、400で拒否します。根拠: [dynamic parameter](https://docs.opensearch.org/2.19/mappings/mapping-parameters/dynamic/)。 |
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

## 追加調査で見つけた差と対応

| ケース | OpenSearch 2.19 | osmemの現状 | 回帰テスト / 根拠 |
|---|---|---|---|
| `PUT /{index}/_settings?preserve_existing=true` | 既存settingを保持し、新しいsettingだけ反映する。 | 新しい値のみをdeep mergeする。 | `TestPreserveExistingSettingsAndDynamicEnumValidation`; [Update Settings](https://docs.opensearch.org/2.19/api-reference/index-apis/update-settings/) |
| `ignore_above:0` | 空でないkeyword値をindexしない。 | 明示された0をmappingに保持し、空でない値をindexしない。 | `TestIgnoreAbove`; [ignore_above](https://docs.opensearch.org/2.19/mappings/mapping-parameters/ignore-above/) |
| field capabilities across indices | `aggregatable` / `searchable` は同じtype group内で該当能力を持たないindexがあればfalse。 | capability flagsを全mapped indexで評価するよう修正。 | `TestFieldCapsAcrossIndices`; [Field Capabilities](https://docs.opensearch.org/2.19/api-reference/search-apis/field-caps/) |
| Index document version types | 2.19 accepts `external_gt` alongside `external` and `external_gte`. | `external_gt`を受け付け、versionが既存値より厳密に大きい場合だけ書き込み。 | `TestWriteEnumValidationAndExternalGT`; [Index Document](https://docs.opensearch.org/2.19/api-reference/document-apis/index-document/) |
| `dynamic` enum | The API supports `true`, `false`, `strict`, `strict_allow_templates`, and `false_allow_templates`; `runtime` is invalid in this setting. | Unknown values and `runtime` are rejected. The two `*_allow_templates` modes are valid in OpenSearch 2.19 but not implemented by osmem. | `TestPreserveExistingSettingsAndDynamicEnumValidation`; [dynamic parameter](https://docs.opensearch.org/2.19/mappings/mapping-parameters/dynamic/) |

OpenSearch本体のREST YAMLケースに加え、[`opensearch-api-specification`](https://github.com/opensearch-project/opensearch-api-specification/blob/main/TESTING_GUIDE.md)には`npm run test:spec`で実行するYAMLテストもあります。ガイドのcoverage例ではverb/path組み合わせの約38.9%を評価し、`OPENSEARCH_URL`で任意の接続先を指定できます。request/response schemaを素早く確認する補助には使えますが、挙動比較のケース数では本体2.19 RESTテスト集がより広く、OpenSearch側runnerをそのままosmemへ接続することはできません。

## 検証

`GOCACHE=/private/tmp/osmem-gocache go test -count=1 ./...`、互換性のfocused test、`git diff --check`が成功しました。全パッケージをローカルnetworkを許可した環境で確認しています。`website`ディレクトリの`npm run build`も成功しました（既存の `docs → 404` 警告のみ）。

## v0.1.3基点の追加監査: 条件の組み合わせとメタデータ（修正前の記録）

**この節は修正前の記録です。以下の12件は後述の3.8.0実比較で確認し、修正しました。**

2026-09-15、`2b96669966be7155de5baec6a7dfbdee5153b8ea` (`v0.1.3`)を基点に、13ケースを追加しました。**12ケースで期待値との差、1ケースで一致を確認しました。** 今回はDocker daemonが停止していたため、OpenSearch 2.19.1の固定ソースから期待値を導き、osmemのHTTP handlerにリクエストを送っています。前節の実サーバー直接比較とは検証方法が異なり、この段階では12件の実サーバー再比較が必要で、エンジンの修正はまだ含めていませんでした。

再現用の[リクエスト集](testdata/compatibility/probes.json)、[実行方法](testdata/compatibility/README.md)、[観測した応答全文](testdata/compatibility/observations-v0.1.3.json)を保存しました。値を`float64`へ変換せずに比較するため、64bit整数の差も検出できます。以下のケース名はfixtureの`id`と一致します。

| ケース | OpenSearch 2.19.1の期待値 | osmem v0.1.3の観測結果 | 影響 |
| --- | --- | --- | --- |
| `create-external-version` | `_create`に`version=5&version_type=external`は400 [A] | 201で作成 | 本番では拒否される書き込みがテストで成功する |
| `auto-id-external-version` | IDなしのPOSTにexternal version指定は400 [A] | 201でIDを生成 | 不正なクライアントの指定を見逃す |
| `version-with-occ` | external versionと`if_seq_no` / `if_primary_term`の併用は400 [B] | 条件が一致すると200で更新 | 排他制御のAPI使用誤りを見逃す |
| `bulk-version-precision` | Bulkのversion `9007199254740993`を64bit整数で保持 [C] | `_version:9007199254740992`へ丸める | versionの値が変わり、競合判定にも影響し得る |
| `bulk-unknown-metadata` | action metadataの未知の`unexpected`はリクエスト全体を400で拒否 [C] | 200で文書を書き込む | パラメータ名の誤りを見逃す |
| `bulk-update-version` | Bulk updateの`version`指定はリクエスト全体を400で拒否 [C] | 200で更新 | 禁止されたversion指定が成功する |
| `resolve-alias-only` | alias名だけを解決するとtop-level `indices`は空 [D] | backing indexも`indices`に含める | indexとaliasの列挙結果が変わる |
| `resolve-index-aliases` | concrete indexを解決すると、そのindexの全aliasを返す [D] | alias名が検索式に一致しないと`aliases`を省略 | 関連aliasを取得できない |
| `resolve-hidden-attribute` | hidden indexのattributesは`["hidden","open"]` [D] | `["open"]` | hiddenの識別ができない |
| `resolve-wildcard-order` | `expand_wildcards=none,open`はnoneでリセット後openを有効にして200 [E] | noneを含むため400 | 有効な列挙指定を拒否する |
| `template-equal-priority` | 同じpattern・priorityの別composable templateのPUTは400 [F] | 200で両方登録 | 本番で登録できない構成を許す |
| `template-v2-suppresses-v1` | composableが一致すればlegacy templateを適用しない [G] | legacyのfieldもマージする | 本番にはないfield mappingが作られる |

### 期待値の根拠とテスト集の活用

- [A: IndexRequest.validate](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/action/index/IndexRequest.java#L202)はcreateのversion指定と自動IDのversion指定を検証します。
- [B: DocWriteRequest.validateSeqNoBasedCASParams](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/action/DocWriteRequest.java#L309)はversionとcompare-and-set条件の併用を拒否します。
- [C: BulkRequestParser](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/action/bulk/BulkRequestParser.java#L230)はversionを`longValue()`で読み、未知のmetadataとupdateのversion指定も拒否します。osmemは`internal/engine/docs.go`のBulk処理でmetadataの数値を`toFloat`経由で変換しています。既報のlong fieldのrange/sort精度とは別の経路です。
- [D: ResolveIndexAction.enrichIndexAbstraction](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/action/admin/indices/resolve/ResolveIndexAction.java#L635)はindexとaliasを別々の配列へ追加し、indexの全aliasとhidden属性を設定します。
- [E: IndicesOptionsのwildcard状態parser](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/action/support/IndicesOptions.java#L80)は指定順に状態を変更し、noneでそれまでの状態を消します。
- [F: MetadataIndexTemplateService](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/cluster/metadata/MetadataIndexTemplateService.java#L626)は同priorityの重複patternを拒否します。これは既報のsimulationでの重複表示不足とは別に、登録時点のvalidationの差です。
- [G: MetadataCreateIndexService](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/server/src/main/java/org/opensearch/cluster/metadata/MetadataCreateIndexService.java#L458)はV2が一致したらV1を適用する分岐に入りません。

本家の[`indices.resolve_index/10_basic_resolve_index.yml`](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/rest-api-spec/src/main/resources/rest-api-spec/test/indices.resolve_index/10_basic_resolve_index.yml)からopen indexとaliasに関するassertを切り出した`resolve-open-control`は通過しました。closed indexを含む原本全体の移植ではありません。他の12ケースは上記の本家ソースを根拠に作った境界ケースです。基本ケースだけでは、alias名・concrete index名を個別に指定する際の不具合を検出できませんでした。

修正するなら、まず値が変わるBulkのversion精度、続いて書き込みvalidationとtemplate適用規則を優先します。いずれもアプリケーションのテスト結果を誤らせます。resolveの応答構造・属性・wildcard指定はその次に扱えます。修正時は該当ケースを通常の回帰テストへ移し、期待値をosmemの現在値へ書き換えないようにします。

今回の検証: 通常の`go test ./...`は全パッケージ成功、websiteの`npm run build`も成功しました（既存の`docs → 404`警告あり）。opt-inの`TestCompatibilityProbes`は上表の12ケースで期待どおり差を検出して失敗し、controlの1ケースは成功しています。これは修正済みを示すテスト結果ではありません。

## OpenSearch 3.8.0の実サーバー比較と修正

2026-09-15、`opensearchproject/opensearch:3.8.0`をsingle-node、heap 512 MiB、security plugin無効、localhost限定portで起動して比較しました。`GET /`で`version.number=3.8.0`、build hash `e5a3c5691be87af6c12dbe3e158c59c04ee72973`を確認しています。使用imageのdigestは`sha256:bcc1797519726ceb6d651d4a3e60b7c30da91793914a8dfe75fd441d4f641509`です。

前節の13ケースは実サーバーですべて期待値に一致し、osmemの12件の差を確認できました。その後、正常系・境界値・書き込み副作用を加えて**32ケース**へ拡張し、実サーバーと修正後のosmemが同じassertを通過しました。[OpenSearchの実応答](testdata/compatibility/observations-opensearch-3.8.0.json)と[修正後のosmem応答](testdata/compatibility/observations-osmem-fixed.json)を保存しています。setup/transport/cleanupの失敗はありません。

修正内容:

- 書き込みのversionとcreate/OCC、自動IDの組み合わせを事前検証します。Bulkのcreate条件も、個別itemの実行前に検査します。
- Bulkのversion・sequence number・primary termは`float64`を通さずsigned 64-bit整数へ変換します。未知のmetadataとupdateのversion指定はリクエスト全体を拒否します。後続actionが不正なら、先行文書も書き込まれないことを確認しました。
- `_resolve/index`でindexとaliasを分離し、indexに付いた全aliasとhidden属性を返します。wildcard状態は指定順に適用します。以前のGoテストにあった「alias解決でbacking indexもtop-levelに返す」「noneなら400」という期待値も実測結果に合わせて訂正しました。
- composable templateが一致したらlegacy templateを適用しません。同priorityの重複検査は3.8の実装に合わせ、同名templateの更新と異なるpriorityは許可します。

追加比較によって訂正した期待値もあります。BulkのJSON小数version `1.5`は400ではなく200で`_version:1`となります。`expand_wildcards=open,none`と`none`は400ではなく200で空配列となり、`hidden`だけではopen indexを展開しません。また、3.8では同じprefixの`logs*`と`*2026`を同priorityで登録できます。これは[3.8のtemplate重複検査](https://github.com/opensearch-project/OpenSearch/blob/3.8.0/server/src/main/java/org/opensearch/cluster/metadata/MetadataIndexTemplateService.java#L870)が完全なpattern交差ではなく、`*`を除いた最小文字列による検査へ変わっているためです。これらは元の12件に含めず、追加ケースとして記録しています。

[再実行手順](testdata/compatibility/README.md)にはDocker起動とPython runnerのコマンドがあります。runnerはケースごとに一意のresource名を使い、作成した名前だけを削除します。Go側の32ケースはopt-in監査から通常の回帰テストへ昇格しました。HTTP statusと指定した応答フィールドを比較しており、エラーメッセージ全文や未assertのフィールド、3.8全機能との一致を保証するものではありません。以前からのrouting、scoring、component template、closed indexなどの制限は残ります。osmemが応答で報告するversionは、この時点では変更していません(後述の差分監査で3.8.0に変更しました)。

最終検証: `go test -count=1 ./...`は全パッケージ成功、3.8.0実サーバーrunnerと修正後osmemの32ケースもすべて成功しました。`git diff --check`とwebsiteの`npm run build`は成功しています（既存の`docs → 404`警告あり）。実応答を保存した後、比較用コンテナは削除しました。

## 2026-09-16: OpenSearch 3.8.0との差分監査と修正

### 方法

- 62本のシナリオファイル(477シナリオ、6,139リクエスト)を用意し、同じリクエスト列をOpenSearch 3.8.0(基準)、OpenSearch 2.19.1、osmemへ送りました。HTTP statusと応答全体を比較し、`took`、UUID、ノードID、自動生成IDなど変動する値は比較から除外しています。
- 対象は検索、文書API、マッピングとフィールド型、集計、クエリDSL、ハイライト、管理API(cat、cluster、alias、template、settings、analyze)、HTTP層(URLパラメータ、Content-Type、メソッド、`filter_path`)です。
- OpenSearch 3.8.0と2.19.1はDockerのsingle-node構成(security無効、localhost限定)で起動しました。両者の挙動が異なる場合は3.8.0に合わせています。osmemが`GET /`で報告するバージョンも3.8.0(lucene 10.5.0、最小互換バージョン2.19.0/2.0.0)に変更しました。
- 修正は領域ごとに行い、3.8.0の実応答を期待値とする回帰テストを追加しました。

### 結果

| 時点 | OpenSearch 3.8.0と異なるリクエスト | 差のあるシナリオ |
|---|---|---|
| 修正前 | 3,263 | 447 |
| 修正後 | 308 | 101 |

### 主な修正

- **応答の表現。** 数値をJavaと同じ表記で出力し(`1.0`、`1.0E-4`)、`float`・`half_float`・`scaled_float`のdoc valueの精度をsort値、集計、`fields`、`docvalue_fields`に反映しました。`root_cause`/`caused_by`の連鎖と`search_phase_execution_exception`のshard failureをOpenSearchの規則で組み立てます。
- **HTTP層。** OpenSearchのPathTrieに合わせたルーティング(405と`Allow`、`OPTIONS`)、APIごとのURLパラメータ検証、Content-Type(406)、重複キーの拒否、`filter_path`の除外指定、`source`パラメータ。
- **index解決と検索機能。** `expand_wildcards`、hidden index、closed indexと`_open`/`_close`、shard単位の`terminate_after`、rescore、`indices_boost`、slice、index単位の`max_result_window`、scroll・PIT・msearchの検証。
- **文書API。** 削除tombstone、external versionのDELETE、bulkの行単位解析、by-query系の検証("query is missing")、`_routing`と`_ignored`の返却。
- **マッピング。** 1677〜2262年の範囲外の日付とナノ秒精度、数値・IP・geo_pointの検証、型ごとのパラメータ検証と更新時の衝突、dynamic template、`ignore_malformed`。
- **クエリDSL。** 実行前の完全な解析とOpenSearchのエラー、Luceneと同じbool・dis_max・boosting・function_scoreの意味、トークン位置を使うフレーズ、query_stringとsimple_query_stringの文法、regexp・fuzzy・wildcard、`matched_queries`。
- **ハイライト。** unified・plain・fvhの3種類をOpenSearchから移植しました(JavaのBreakIteratorの移植を含む)。
- **集計。** 事前の解析と検証、ip_range・geo_distance・adjacency_matrix・geohash_grid・geotile_grid・geo_bounds・geo_centroid・serial_diff・moving_avgなどの追加、t-digestによるpercentiles、`typed_keys`と`meta`。
- **管理API。** 設定レジストリによる検証、aliasとtemplateの書き直し(component templateの合成を含む)、`_stats`、cluster health、cat API群、`_analyze`、indexブロックとclusterブロック。
- **エラーの位置情報。** パースエラーに`[行:列]`の接頭辞と`line`/`col`をOpenSearchと同じ規則で付けます。delete-by-query、reindexのsource、aliasのfilterのようにOpenSearchが再シリアライズするボディも対象です。
- **検索リクエストの検証。** 検索ボディのキーを文書中の順に検証し、`_validate/query`の`explain`/`rewrite`、`_explain/{id}`、field capabilitiesの詳細、`preference=_shards:`、`_geo_distance`ソート、スコアが不要なときにスコア関数を実行しない挙動を追加しました。
- **フィールド型の拡充。** range型(値の検証と`relation`付きのrangeクエリ)、`search_as_you_type`のサブフィールド、`flat_object`、`join`(parent_id、has_child、has_parent)、`rank_feature`(クエリを含む)、`knn_vector`と`completion`の値の検証、nestedの`include_in_parent`/`include_in_root`、`copy_to`の検証、`similarity: boolean`、2^53を超える整数の正確な出力。

### 残っている差

修正後の308リクエストの主な内訳です。

- 意図的に未対応としている機能(Painless script、ingestパイプライン、data stream、rollover・shrink・split・clone、`_tasks`、`_nodes/stats`など)への400/404: 97件。
- 応答ボディの細部(統計カウンタ、`_seq_no`、cat出力、taskの情報、ノードIDなど): 78件。
- エラーメッセージや種類: 60件。
- 検索結果の件数や内容: 25件。
- OpenSearchが受理する入力をosmemが拒否するもの: 19件。集計値: 13件。OpenSearchが拒否する入力をosmemが受理するもの: 11件。その他のステータスの差: 5件。

### 検証

`go vet ./...`と`go test -count=1 ./...`は全パッケージで成功しました。差分ハーネスはリポジトリに含めていません。
