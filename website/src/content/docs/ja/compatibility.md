---
title: "互換性"
description: "OpenSearchと比べて、osmemが実装しているもの、近似しているもの、拒否するもの。"
---

フェイクで通ったテストが何かを証明するのは、テストが見ている箇所で、フェイクが本物と同じように振る舞う場合だけです。このページは、osmemについてそれを判断するための一覧です。実装しているもの、近似しているもの、意図的にエラーにするもの。

## 原則

明示的に未対応とした機能の多くは400 `unsupported_operation_exception`を返します。応答の形、ステータスコード、エラーの種類とメッセージはOpenSearchに合わせています(OpenSearch 3.8.0を基準に、2.19.1でも確認)。一方、近似した動作をする機能や、受理して無視する検索オプションもあります。そうした差は以下に記載します。回帰テストは個々の挙動を確認するもので、OpenSearchの全機能との同等性を保証するものではありません。

## 実装しているもの

**インデックス。** 設定・マッピング・エイリアス付きの作成、ワイルドカードと`_all`での削除、存在確認、取得、`_mapping`の取得と更新、OpenSearchの設定レジストリで検証する`_settings`の取得と更新(未知・不正・private・static・finalの設定はOpenSearchと同じメッセージで拒否し、`include_defaults`、`flat_settings`、`settings_filter`に対応)、全セクションと`level=shards`に対応した`_stats`、`_resolve/index`、何もしない`_refresh`・`_flush`・`_forcemerge`、実際にclosed状態を持つ`_open`・`_close`(closed indexへの検索・取得・書き込みは`index_closed_exception`になり、wildcardは`expand_wildcards`で指定しない限りclosed indexを含めず、`_cat/indices`とcluster healthでは`close`とredを表示し、analysis設定の変更は再open時に反映)、読み取り・書き込み・メタデータ変更に対するindexブロックとclusterブロック(`index.blocks.*`、`cluster.blocks.*`)、`level`・`wait_for_*`パラメータと408のタイムアウト応答に対応したcluster health、クラスタのsettings・state・stats、スニッフィングするクライアント向けに実際のアドレスを返す`_nodes`。composable・component・legacyのインデックステンプレートは合成したうえで、自動作成を含むインデックス作成時に適用し、simulationは重複するテンプレートも報告します。hidden indexとhidden aliasは、`-name`による除外を含め、OpenSearchの解決規則に従って`expand_wildcards`・`ignore_unavailable`・`allow_no_indices`を扱います。

**cat API。** `_cat/indices`、`aliases`、`health`、`count`、`nodes`、`master`/`cluster_manager`、`plugins`、`templates`、`shards`、`segments`、`recovery`、`allocation`、`thread_pool`、`pending_tasks`、`fielddata`、`nodeattrs`、`tasks`、`repositories`、`snapshots`、`segment_replication`。`format`、`h`(別名とワイルドカードを含む)、`v`、`s`、`help`、`bytes`、`time`、`pri`に対応し、列の配置もOpenSearchに合わせています。

**ドキュメント。** 自動id、検証済みの`op_type`、index・update・deleteでの`if_seq_no`/`if_primary_term`、`internal`・`external`・`external_gt`・`external_gte`のversionに対応した`_doc`。DELETEはexternal versionを検査し、削除した文書は`index.gc_deletes`(既定60秒)の間tombstoneを残すので、削除後もversionが連続します。`_create`、`doc`・`doc_as_upsert`・`upsert`(存在しないindexを作成)・`detect_noop`に対応した`_update`、`_source`、文書ごとの失敗を返す`_mget`、OpenSearchと同じく行単位で解析し、`require_alias`・空のid・必須routingをアイテムごとのエラーにする`_bulk`、`_delete_by_query`と`_update_by_query`(queryが必須、`conflicts`・`max_docs`・`slices`・`wait_for_completion=false`のtaskに対応)、source/destの検証、`max_docs`、external宛先versionに対応した`_reindex`。GETと検索結果のhitは`_routing`と`_ignored`を返します。`_count`のボディは`query`だけを受け付けます。write indexのない複数index向けaliasへの書き込みは、OpenSearchと同じく失敗します。

**マッピング。** text(analyzer、search_analyzer、マルチフィールド)、match_only_text、keyword(normalizer、`ignore_above`、0を含む)、OpenSearchと同じ値の検証と型変換を行うすべての数値型、boolean、名前付きフォーマット・Javaパターン・epochフォーマットに対応したdateとdate_nanos(1677〜2262年の範囲外の日付とナノ秒精度を含む)、入力形式に対応したgeo_point、ip、objectとnested、フィールドエイリアス、`null_value`、`copy_to`、`index: false`(OpenSearchと同じくクエリはdoc valuesを使用)、`enabled: false`、`ignore_malformed`(そのフィールドだけを読み飛ばし`_ignored`に記録)、`coerce`、`dynamic: true/false/strict/strict_allow_templates/false_allow_templates`、dynamic template(`match`、`path_match`、`match_mapping_type`、`{name}`と`{dynamic_type}`)、range型(`integer_range`、`long_range`、`float_range`、`double_range`、`date_range`、`ip_range`)、`._2gram`…と`._index_prefix`サブフィールドを持つ`search_as_you_type`、`flat_object`、`join`、`rank_feature`、`rank_features`、`knn_vector`(値は検証しますが、kNN検索は未実装)と、`contexts`(`category`と`geo`)に対応した`completion`、nestedフィールドの`include_in_parent`と`include_in_root`、`similarity: boolean`。マッピングのパラメータはフィールド型ごとに検証します。未知・不正なパラメータはOpenSearchと同じ「Failed to parse mapping」のエラーになり、変更できないパラメータを変える更新は「Cannot update parameter [x] from [a] to [b]」で失敗します。`total_fields.limit`、`depth.limit`、`nested_fields.limit`、`field_name_length.limit`、文書単位の`nested_objects.limit`、index単位の`index.mapping.ignore_malformed`を適用します。動的マッピングはOpenSearchに従います。文字列は`.keyword`サブフィールド付きのtextになり、日付と数値は`date_detection`と`numeric_detection`に従って検出し、ドットを含むキーはオブジェクトに展開します。`derived`フィールドとstar-treeの`composite`マッピングは未対応として拒否します。

**クエリ。** クエリは実行前に全体を解析し、不正なクエリはOpenSearchと同じエラーの種類とメッセージで失敗します。match(operator、minimum_should_match、transpositionsを含むfuzziness、zero_terms_query)、実際のトークン位置を使うmatch_phraseとmatch_phrase_prefix(ストップワードの空き、`position_increment_gap`、`max_expansions`)、match_bool_prefix、multi_match(全type、フィールドパターン、ブースト、`lenient`、analyzerごとにフィールドをまとめるcross_fields)、combined_fields、common、マッピングの型に従うtermとterms(terms lookup、`case_insensitive`)、terms_set、数値と日付に対するrange(日付演算、`format`、`time_zone`)、exists、prefix、wildcard、regexp(Luceneの構文と`flags`)、fuzzy(`max_expansions`、`transpositions`)、ids、bool、constant_score、dis_max(`tie_breaker`)、boosting(`negative`と`negative_boost`)、function_score(weight、field_value_factor、random_score、decay関数、全score mode・boost mode、`max_boost`、`min_score`)、nested(score_mode、ignore_unmapped、inner_hits)、query_string(正規表現・エスケープ・構文エラーを含むLuceneのclassic構文)とsimple_query_string(flags)、geo_distance、geo_bounding_box、geo_polygon(geohashとWKTの`BBOX`入力、全距離単位)、range型フィールドへのrange(`relation`)、rank_feature(saturation、log、sigmoid、linear)、parent_id、has_childとhas_parent(`inner_hits`なし)、`flat_object`のサブフィールドへのterm、wrapper、intervals(`match`・`prefix`・`wildcard`・`fuzzy`・`regexp`・`all_of`・`any_of`と`filter`を、存在の近似ではなく実際の位置情報によるマッチングとして実装)、span_term・span_near・span_multi、more_like_this、distance_feature(`date`・`date_nanos`・`geo_point`のpivot)、`geo_point`フィールドに対するgeo_shape(envelope・polygon・circle形状の`intersects`関係)。term系クエリのスコアはOpenSearchと同じく定数(1×boost)で、名前付きクエリはhitに`matched_queries`を付け、`index.query.default_field`を適用します。

**検索。** `index.max_result_window`(既定は10,000件、index単位で変更可)までのfrom/size、フィールドによるソート(order、missing、mode、unmapped_type。`format`キーはOpenSearchと同じく拒否)、`_score`・`_doc`・`_id`でのソート、欠損した数値や日付にOpenSearchと同じ番兵値を使うsearch_after、scroll、作成時点のindexを保持するpoint in time、`_source`のフィルタリング、`fields`、`docvalue_fields`、`version`、`seq_no_primary_term`、`track_total_hits`、`track_scores`、`min_score`、`post_filter`、`inner_hits`付きの`collapse`、rescore(`window_size`、重み、すべてのscore mode)、`indices_boost`、scrollとpoint in timeのslice、`_count`、`_msearch`、`typed_keys`、`preference=_shards:`、`phase_took`、`_geo_distance`によるソート、`_field_caps`(メタデータフィールド、`index_filter`、`meta`、検索・集計できないindexの一覧を含む)、`explain`・`rewrite`・`all_shards`に対応した`/{index}/_validate/query`、`/{index}/_explain/{id}`とhitの`explain`(いずれもfilter付きaliasのfilterを反映)、設定したshard・replica数をもとにした`_search_shards`(`routing`、`preference`、リクエストボディの`slice`に対応)、既にindexされた文書に対する`/{index}/_termvectors/{id}`と`_mtermvectors`(`field_statistics`、`term_statistics`、`positions`、`offsets`、`realtime`)、除外指定と`**`を含む`filter_path`、`pretty`、`rest_total_hits_as_int`、URLパラメータ`source`、gzipで圧縮されたリクエストボディ。ハイライトはOpenSearchのunified・plain・fvhの3種類を実装しています。文と単語の境界スキャナ、`fragment_size`、`number_of_fragments`、`order: score`、`no_match_size`、`require_field_match`、`highlight_query`、`matched_fields`、`tags_schema`、html encoder、`max_analyzer_offset`に対応し、nestedとcollapseのinner hitsもハイライトします。totalはsearch_afterによるページングやcollapseの前に一致した文書数を返します。`terminate_after`はindex順にshardごとに件数を数え、集計付きのsize 0検索ではOpenSearch 3.8と同じく`terminated_early`を返します。検索ボディのキーは文書中の順にOpenSearchと同じメッセージで検証し、スコア関数はスコアが必要な場合だけ実行します。

**集計。** 集計リクエストは実行前に解析・検証し、OpenSearchと同じエラーを返します。バケット集計: terms(size、並べ替え、min_doc_count、missing、Luceneの正規表現によるinclude/exclude、`show_term_doc_count_error`)、multi_terms、rare_terms、significant_termsとsignificant_text(`jlh`・`chi_square`・`gnd`・`mutual_information`・`percentage`の各heuristic)、range、date_range、ip_range、geo_distance、histogram、date_histogram(calendarとfixedの間隔、夏時間を正しく丸める`time_zone`、offset、format、extended_bounds、空バケットの補完)、auto_date_histogram、variable_width_histogram、filter、filters、adjacency_matrix、missing、global、nestedとreverse_nested、sampler、`after`付きのcomposite(`reverse_nested`の子としても使用可)、geohash_grid、geotile_grid。数値系メトリクスとrange・compositeのbucket keyは、2^53を超える`unsigned_long`もdouble同様に扱います。メトリクス集計: avg、sum、min、max、value_count、stats、extended_stats、cardinality、percentilesとpercentile_ranks(OpenSearchと同じt-digest。`hdr` methodは未実装)、median_absolute_deviation、top_hits、weighted_avg、geo_bounds、geo_centroid。パイプライン集計: cumulative_sum、derivative、bucket_sort、avg/sum/min/max/stats/extended_stats/percentiles_bucket、serial_diff、moving_avg(simple、linear、ewmaモデル)。`search.max_buckets`は固定値ではなくcluster設定(transient、次いでpersistent)から読み取ります。応答には`meta`、`typed_keys`、`format`に従う`*_as_string`を出力します。サブ集計は入れ子にできます。

**サジェスター。** `_search`ボディの`suggest`は、term(索引済みtermへの編集距離による補正)、phrase(stupid-backoffのn-gramモデルでスコア付けする候補生成。Luceneの平滑化モデルの近似、「近似」節を参照)、completionの3種類のsuggesterに対応します。completionフィールドは`contexts`(`category`と`geo`。索引時の必須context検証を含む)、`skip_duplicates`、weightによる順序付け、fuzzy prefixマッチに対応します。

**解析。** `_analyze`は、osmemが実装する組み込みのアナライザ・トークナイザ・トークンフィルタ・文字フィルタについて、OpenSearchと同じトークンtype、UTF-16のoffset、`explain`の出力を返します。実装していないコンポーネントは「not supported by osmem」で終わるエラーになります。analysis設定はインデックス作成時に検証します。インデックス作成と検索のトークン化は引き続きbleveベースのアナライザで行います。standard、simple、whitespace、keyword、stop、patternと、bleveの言語別アナライザ、トークナイザ(standard、whitespace、keyword、letter、pattern、ngram、edge_ngram、char_group、uax_url_email)、トークンフィルタ(lowercase、asciifolding、stop、ngram、edge_ngram、shingle、stemmer、snowball、porter_stem、truncate、length、unique、reverse、cjk_bigram、cjk_width)、文字フィルタ(html_strip、pattern_replace)から組み立てるカスタムアナライザ、normalizer。日本語サポートを有効にすれば、kuromojiアナライザ、モードとユーザー辞書に対応したkuromoji_tokenizer、kuromoji_baseform、kuromoji_part_of_speech、cjk_width、ja_stop、kuromoji_stemmer、kuromoji_readingform。

## 近似しているもの

- **スコア。** bleveのBM25は、Luceneのものとは違います。普通のクエリなら順位は一致しますが、全文検索クエリの`_score`の値と同点の扱いは一致しません。フィルタやterm系クエリなど定数スコアの部分はOpenSearchと同じです。
- **数値のrangeとsortの精度。** 整数フィールドのterm検索は完全な値を使いますが、非常に大きな`long`のrange検索とsortでは2^53を超える精度が失われることがあります。
- **textのfielddata。** `fielddata:true`なら、解析後のtermによるtext fieldのsortができます。fielddataを使ったtext aggregationは未対応で、fielddataを有効にしていないtext fieldのsortはエラーになります。
- **効果のない検索オプション。** `timeout`と`stats`は検証しますが効果はありません。`profile`はタイミングが0のOpenSearchと同じ構造を返し、名前付きの`search_pipeline`は存在している必要があります。`runtime_mappings`とscript付きの`script_fields`は拒否します。
- **アナライザ。** インデックス作成はbleveのアナライザを使うため、同じアナライザでも`_analyze`が返すトークンと実際に索引されるtermが異なることがあります。言語別アナライザは、bleveのステマーとストップワードを使います。日本語の分かち書きはkuromojiではなく、IPA辞書を使うkagomeによるもので、未知語では結果が異なることがあります。韓国語と中国語はCJKのbigramです。インデックス作成が実装していないカスタムアナライザのコンポーネントは、警告を出して読み飛ばします。
- **厳密さ。** OpenSearchはcardinalityを近似値で返しますが、osmemは厳密な値を返します。percentilesはOpenSearchと同じt-digestアルゴリズムですが、centroidが統合されるデータでは値がわずかに異なることがあります。
- **事前集計した文書数。** osmemのバケット集計は`_doc_count`を無視します。OpenSearchでは[`_doc_count`](https://docs.opensearch.org/latest/aggregations/bucket/terms/)で事前集計済み文書の件数を反映できます。
- **shardとrouting。** 応答は設定したshard数を報告し、`terminate_after`とsliceもOpenSearchのshard単位の規則に従います。get・exists・delete・update・mgetと単一文書の読み書きは、複数shardのindexではOpenSearchと同じrouting hashを計算し、誤った、または未指定のroutingを本物の誤ったshardと同じく「見つからない」扱いにします。文書storeを`(routing, id)`で物理的に再構成してはいないため、同じ`_id`を別のroutingで書き込むと、別shardで共存するのではなく既存の1文書を更新します。
- **リフレッシュ。** 書き込みは即座に見えます。書き込みからリフレッシュまでの間の状態を、テストで観測することはできません。

### 既知の動作差

以下は、このページの末尾にある差分監査の後も残っている差です。

| 領域 | 差 |
| --- | --- |
| フィールド型 | Luceneの決定化の上限を超えるregexpを拒否しません。textとshingleフィールドへの`exists`クエリのヒット総数は、削除後に異なることがあります。BM25の`k1`/`b`パラメータは無視します。2^53を超える`long`値のsort値は精度が失われ、composite集計の`unsigned_long`のbucket keyも同じ境界で1ずれることがあります。マッピングされた`date_range`フィールドへの`date_histogram`は`hard_bounds`を無視します。 |
| エラーの詳細 | リクエストボディに複数の問題がある場合、osmemはキーを文書中の順ではなくソート順に検査するため、OpenSearchと異なる問題を報告することがあります。まれに使われる原因の連鎖が異なる場合があります。`_mtermvectors`は非推奨のパラメータ別名(`_version_type`、camelCase名)をOpenSearchのように拒否しません。 |
| explanation | `explain=true`、`_explain`、[Validate Query](https://docs.opensearch.org/latest/api-reference/search-apis/validate/)のexplanationは、定数スコアのクエリとそのboolean組み合わせについて出力します。BM25と`function_score`のexplanationは再現しません。`preference=_only_nodes:`はノードを選ばずに受理し、`_geo_distance`のソート値は末尾の桁が異なることがあります。 |
| 統計とノード | `_stats`、`_cat`、クラスタ統計のカウンタは0で、ストアサイズは推定値です。 |
| 集計 | JSONのキー順を保持しないため、複数キーのorderオブジェクトや、1リクエスト内での`aggs`と`aggregations`の優先が異なることがあります。マッピングが食い違う複数indexにまたがる集計は、OpenSearchがshard単位の結果を返す場合でも失敗することがあります。osmemは単一の検索ストリームなので、termsの`shard_size`は効果がありません。集計の`profile`は常にサブ集計0件を報告します。 |
| ハイライト | `tags_schema`と`pre_tags`の両方を指定すると、キーの順番に関係なく`tags_schema`が優先されます。top_hitsのハイライトはクエリのtermを使いません。`index.highlight.max_analyzed_offset`と`boundary_scanner_locale`は未実装です。 |
| サジェスター | completion suggestionの応答は一致した`contexts`を含みません。typed_keysはcompletion・phrase suggestionの名前にprefixを付けません。completionフィールドの`contexts`マッピングは、OpenSearchの正規化形式(解決済みgeohash精度、大文字の`type`)ではなく入力時の値(距離文字列そのまま、小文字の`type`)をそのまま返します。 |
| その他のAPI | search templateと`_rank_eval`にはrouteがありません。`_mtermvectors`と`_termvectors`は、既にindexされた文書のみに対応し、その場で解析する`doc`/`per_field_analyzer`モードは未対応です。 |

PIT検索では期限を確認し、検索リクエストに`keep_alive`があれば延長します。

## 未対応

Painless script(`script`、`script_score`、スクリプトによる更新とby-query操作、集計とソートのscript)、`bucket_script`、`bucket_selector`、`moving_fn`、kNNとニューラル検索、パーコレーター、`children`/`parent`集計とjoinクエリの`inner_hits`、ほとんどのspanクエリ(`span_or`、`span_first`、`span_not`、`span_containing`、`span_within`、`field_masking_span`)、実際の`geo_shape`型フィールドに対するgeo_shape(bounding boxによる近似のみ。`geo_point`フィールドへのgeo_shapeクエリは実装済み、「クエリ」を参照)、percentilesの`hdr` method、matrix_stats、geohex_grid、derivedフィールド、search template、rank evaluation、`_list/indices`、`_list/shards`、ingestパイプライン(パイプラインを参照する書き込みは「pipeline with id [x] does not exist」で失敗)、data stream、rollover、shrink/split/clone、`_tasks`、セキュリティ、スナップショット、`_nodes/stats`、`_nodes/usage`、hot threads、YAML・CBOR・SMILEのリクエストボディと応答(`format=yaml`)、`error_trace`のstack trace(`_mtermvectors`向けの簡易な自作実装を除く)。

## クライアントごとの注意

- opensearch-go v4はCIでテストしています。go-elasticsearch v8には、確認対象の`X-Elastic-Product`ヘッダを返します。
- olivere/elasticは`/_nodes`をスニッフィングします。osmemはそこに実際のlistenアドレスを返します。
- Elasticsearch 7のバージョン文字列を要求するクライアントには、クラスタ設定`compatibility.override_main_response_version: true`を設定してください。`GET /`が7.10.2を返すようになります。
- エラーはOpenSearchと同じ`{"error": {"root_cause", "type", "reason", "caused_by", ...}, "status"}`の形です。`search_phase_execution_exception`内のshard failure、入れ子の例外からroot causeを選ぶ規則、パースエラーの位置情報(reasonの`[行:列]`接頭辞と`line`/`col`フィールド。delete-by-query、reindexのsource、aliasのfilterのようにOpenSearchが再シリアライズするボディを含む)もOpenSearchに合わせています。未知のURLには文字列の`error`を持つ400を返します。誤ったメソッドには`Allow`ヘッダ付きのフラットな405を返し、`OPTIONS`には許可メソッドを返します。
- リクエストはOpenSearchのRESTレイヤーと同じように検査します。すべてのrouteで未知のURLパラメータを400で拒否し(候補の提示を含む)、パラメータ値を検証します。ボディにはJSONの`Content-Type`が必要で、ない場合は406です。JSONの重複キーは拒否し、`filter_path`はエラー応答を絞り込みません。
- 応答の数値はJavaと同じ表記(`1.0`、`1.0E-4`)です。`float`・`half_float`・`scaled_float`のdoc valueは、sort値、集計、`fields`、`docvalue_fields`で保存時の精度を反映します。

## 回帰テスト

OpenSearch本体には広範な[2.19系REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/2.19/rest-api-spec/src/main/resources/rest-api-spec/test)があります。実行にはOpenSearchのJavaテストフレームワークが必要なので、osmemのGoテストとしてそのまま動かすことはできません。osmemには上記の挙動を確認するGo回帰テストがありますが、YAML全体を実行する差分ランナーはまだありません。テストが証明するのは、そのテストがassertする挙動に限られます。

別の[`OpenSearch API Specification` project](https://github.com/opensearch-project/opensearch-api-specification/blob/main/TESTING_GUIDE.md)にもYAML storyと`npm run test:spec`のrunnerがあり、`OPENSEARCH_URL`で接続先を指定できます。ガイドのcoverage例ではverb/path組み合わせの約39%を評価しています。request/responseの形を確認する補助には使えます。本体2.19 RESTテスト集の方が複数操作を通じた動作ケースを多く含みますが、osmemで使うにはJava側のtest harnessを適応させる必要があります。

## OpenSearch 3.8.0との実サーバー比較

2026-09-15、公式OpenSearch 3.8.0 Dockerサーバーとosmem handlerで32ケースを実行しました。エンジン修正後は、同じHTTP status・応答フィールドのassertが両方で通過しています。`v0.1.3`で見つけた12件の差は、対象ケースについて修正済みです。

| 領域 | 検証した挙動 |
| --- | --- |
| 書き込みvalidation | external versionとcreate、自動ID、sequence number条件の併用を拒否します。正常なexternal versionとcompare-and-setの書き込みは成功します。Bulkの不正なcreate条件も書き込み前に拒否します。 |
| Bulk | `2^53`を超えるversionとsigned 64-bit最大値を保持し、overflowを拒否します。JSONの小数versionはゼロ方向へ切り捨てます。未知のaction metadataとupdateのversion指定はリクエスト全体を拒否し、後続actionが不正なら先行文書も書き込みません。 |
| Resolve index | indexとaliasを別の配列で返し、解決したindexの全aliasとhidden属性を返します。wildcard指定を順番に適用し、`none,open`はopen indexを返し、`open,none`は空配列になります。`hidden`だけではopen indexを展開しません。closed indexは`closed`属性を返し、存在しない名前はリクエストを失敗させずに結果から除きます。 |
| Index template | composableが一致した場合はlegacyを適用しません。同priorityの重複検査は3.8の規則に従い、非重複pattern、異なるpriority、同名templateの更新は許可します。 |

3.8の[template重複検査](https://github.com/opensearch-project/OpenSearch/blob/3.8.0/server/src/main/java/org/opensearch/cluster/metadata/MetadataIndexTemplateService.java#L870)は、patternから`*`を除いた文字列が相手のpatternに一致するかを調べます。同じprefixの`logs*`と`*2026`のように、理論的には交差しても登録できる組み合わせがあり、osmemもこの規則に合わせています。

リポジトリの`testdata/compatibility/README.md`にfixture、実サーバーrunner、修正前後の応答記録の説明があります。`go test -run '^TestCompatibilityProbes$' -count=1 -v .`でosmem側の回帰検査を実行でき、通常のテストにも含まれます。3.8の全機能との互換を保証するものではなく、assertしていないエラーメッセージや応答フィールドには差が残り得ます。このページのその他の制限も引き続き適用されます。osmemは`GET /`でOpenSearch 3.8.0を報告します。

## OpenSearch 3.8.0との差分監査

2026-09-16に、同じリクエスト列をOpenSearch 3.8.0(基準)、OpenSearch 2.19.1、osmemへ送り、`took`、UUID、ノードID、自動生成された文書IDなどの変動する値を除いたうえで、HTTP statusと応答全体を比較する差分ハーネスを実行しました。477シナリオ(6,139リクエスト)で、検索、文書API、マッピングとフィールド型、集計、クエリDSL、ハイライト、クラスタとインデックスの管理、cat API、HTTP層の挙動(URLパラメータ、Content-Type、メソッド、`filter_path`)を扱っています。2つのOpenSearchの挙動が異なる場合は3.8.0に合わせました。

| 時点 | OpenSearch 3.8.0と異なるリクエスト | 差のあるシナリオ |
| --- | --- | --- |
| 修正前 | 3,263 | 447 |
| 修正後 | 308 | 101 |

各修正には、OpenSearch 3.8.0の応答を期待値にしたGoの回帰テストを追加しています。残っている差の多くは[既知の動作差](#既知の動作差)と[未対応](#未対応)に記載したもので、値を検証していないフィールド型、意図的に拒否している機能、osmemが集計しない統計値、BM25のスコア値です。差分ハーネスとシナリオファイルはリポジトリに含めていません。

続く2回目の監査では、OpenSearch本体の[REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/3.8.0/rest-api-spec/src/main/resources/rest-api-spec/test)(検索・集計・文書API・マッピング・クラスタ/index管理にまたがる314ファイル)を、YAML自体のアサーションではなく応答の直接比較でOpenSearch 3.8.0とosmemへ同様に再生しました。1,200シナリオ中495件に実際の差があり、`intervals`、suggest API、`rare_terms`/`significant_terms`/`significant_text`/`auto_date_histogram`/`variable_width_histogram`、`more_like_this`、`distance_feature`、`geo_point`フィールドへの`geo_shape`、spanクエリ、`_search_shards`、term vectors、routingによるshard不一致の検出を実装し、`unsigned_long`集計、`date_range`のbucket境界、動的な`search.max_buckets`設定、その他いくつかの小さな不具合を修正した結果、244件まで減らしました(`_score`/`max_score`の値のみの差と、集計の`profile`タイミングのみの差は、いずれも上記で既に近似として記載済みのため除いています)。残った差は上記の各節に統合しています。このハーネスも(初回のものとは別に作成した2つ目ですが)リポジトリには含めていません。
