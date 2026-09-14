---
title: "互換性"
description: "OpenSearch 2.xと比べて、osmemが実装しているもの、近似しているもの、拒否するもの。"
---

フェイクで通ったテストが何かを証明するのは、テストが見ている箇所で、フェイクが本物と同じように振る舞う場合だけです。このページは、osmemについてそれを判断するための一覧です。実装しているもの、近似しているもの、意図的にエラーにするもの。

## 原則

明示的に未対応とした機能の多くは400 `unsupported_operation_exception`を返します。一方、近似した動作をする機能や、受理して無視する検索オプションもあります。そうした差は以下に記載します。回帰テストは個々の挙動を確認するもので、OpenSearchの全機能との同等性を保証するものではありません。

## 実装しているもの

**インデックス。** 設定・マッピング・エイリアス付きの作成、ワイルドカードと`_all`での削除、存在確認、取得、`_mapping`の取得と更新、setting名の完全一致・ワイルドカードfilterに対応した`_settings`、`docs`と`store` metricに対応した`_stats`、モデル化したindexとaliasの`_resolve/index`、index templateのsimulation、何もしない`_refresh`・`_flush`・`_forcemerge`・`_open`・`_close`、`_analyze`、`_cat/indices`・`_cat/aliases`・`_cat/health`・`_cat/count`、クラスタのhealth・settings・state、スニッフィングするクライアント向けに実際のアドレスを返す`_nodes`。composableとlegacyのインデックステンプレートは、自動作成を含むインデックス作成時に適用されますが、`composed_of`で参照するcomponent templateは作成時にもsimulation時にもマージしません。simulationでは重複templateもすべては報告しません。data streamとhidden/closed indexの完全な挙動もモデル化していません。`include_defaults=true`は未対応です。

**ドキュメント。** 自動id、検証済みの`op_type`、ペアで指定する`if_seq_no`/`if_primary_term`、明示した0以上のversionを必要とする`external`・`external_gt`・`external_gte` versionに対応した`_doc`、`_create`、`doc`・`doc_as_upsert`・`upsert`・`detect_noop`に対応した`_update`、`_source`、`_mget`、アイテムごとのステータスを返す`_bulk`、`_delete_by_query`、スクリプトなしの`_update_by_query`、`max_docs`とexternal宛先versionに対応した`_reindex`。

**マッピング。** text(analyzer、search_analyzer、マルチフィールド)、keyword(normalizer、`ignore_above`、0を含む)、すべての数値型、boolean、名前付きフォーマット・Javaパターン・epochフォーマットに対応したdateとdate_nanos、geo_point、ip、objectとnested、`null_value`、`copy_to`、`index: false`、objectフィールドの`enabled: false`、`dynamic: true/false/strict`、動的フィールド向けのdynamic template、数値フィールドの`coerce:false`。`total_fields.limit`、`depth.limit`、`nested_fields.limit`、文書単位の`nested_objects.limit`を適用します。確認済みimmutable parameter(`index`、`index_options`、`store`、`doc_values`、`null_value`、`similarity`、`normalizer`、`term_vector`、`enabled`、nested include flags)の変更は拒否します。`norms:false → true`は拒否し、`true → false`は許可します。一般的なケースの動的マッピングはOpenSearchに従います。文字列は`.keyword`サブフィールド付きのtextになり、ISO形式の日付は検出され、整数はlong、小数はfloatになります。ドットを含むキーはオブジェクトに展開されます。OpenSearch 2.19で有効な`strict_allow_templates`と`false_allow_templates`は未対応です。

**クエリ。** match(operator、minimum_should_match、fuzziness、zero_terms_query)、slop対応のmatch_phrase、match_phrase_prefix、match_bool_prefix、multi_match(best_fields、most_fields、cross_fields、phrase、phrase_prefix、bool_prefix、フィールドごとのブースト、ワイルドカードのフィールド名)、マッピングの型に従うterm(2^53を超えるlongの完全な整数値を含む)、terms lookupを含むterms、数値・日付(日付演算、`format`、`time_zone`)・文字列に対するrange、exists、prefix、wildcard、regexp、fuzzy、ids、4種類の句とminimum_should_matchに対応したbool、constant_score、dis_max、query_stringとsimple_query_string、geo_distance、geo_bounding_box、wrapper。`function_score`は内側のqueryだけを実行し、スコア関数は無視します。

**検索。** 10,000件のウィンドウまでのfrom/size、フィールドによるソート(order、missing、mode、unmapped_type、format)、`_score`・`_doc`・`_id`でのソート、欠損した数値や日付にOpenSearchと同じ番兵値を使うsearch_after、scroll、作成時点のindexを保持するpoint in time、`_source`のフィルタリング、`fields`、`docvalue_fields`、`version`、`seq_no_primary_term`、`track_total_hits`、`track_scores`、`min_score`、`post_filter`、`inner_hits`付きの`collapse`、ハイライト、`_count`、`_msearch`、mapping fieldを返す`_field_caps`、DSLを検証する`/{index}/_validate/query`、モデル化したindexとalias向けの`/_resolve/index/{name}`、`filter_path`、`pretty`、`rest_total_hits_as_int`、gzipで圧縮されたリクエストボディ。Validateのexplain/rewrite出力は不完全です。field_capsのmetadataと`index_filter`は未対応です。totalはsearch_afterによるページングやcollapseの前に一致した文書数を返します。`terminate_after`は単一の全体上限として適用します。

**集計。** terms(size、件数・キー・サブ集計による並べ替え、min_doc_count、missing、include/exclude)、multi_terms、range、date_range、histogram、date_histogram(calendarとfixedの間隔、time_zone、offset、format、extended_bounds、空バケットの補完)、filter、filters、missing、global、nestedオブジェクト単位で動くnestedとreverse_nested、`after`付きのcomposite、avg、sum、min、max、value_count、stats、extended_stats、cardinality、percentiles、percentile_ranks、top_hits、weighted_avg、median_absolute_deviation。パイプライン集計はcumulative_sum、derivative、bucket_sort、avg/sum/min/max/stats_bucket。サブ集計は入れ子にできます。

**解析。** standard、simple、whitespace、keyword、stop、patternと、bleveの言語別アナライザ。トークナイザ(standard、whitespace、keyword、letter、pattern、ngram、edge_ngram、char_group、uax_url_email)、トークンフィルタ(lowercase、asciifolding、stop、ngram、edge_ngram、shingle、stemmer、snowball、porter_stem、truncate、length、unique、reverse、cjk_bigram、cjk_width)、文字フィルタ(html_strip、pattern_replace)から組み立てるカスタムアナライザ。normalizer。日本語サポートを有効にすれば、kuromojiアナライザ、モードとユーザー辞書に対応したkuromoji_tokenizer、kuromoji_baseform、kuromoji_part_of_speech、cjk_width、ja_stop、kuromoji_stemmer、kuromoji_readingform。

## 近似しているもの

- **数値のrangeとsortの精度。** 整数term検索は完全な整数値を扱いますが、大きな`long`のrange検索とsortは引き続きfloat64を使います。数値型の範囲検証と`scaled_float`の丸めも完全には再現していません。
- **不正なマッピング値。** `ignore_malformed:true`はmappingで受理されますが、不正なフィールド値だけを読み飛ばして文書を保存する動作にはなりません。インデックス時に文書全体が拒否されることがあります。OpenSearchの[`ignore_malformed`](https://docs.opensearch.org/latest/mappings/mapping-parameters/ignore-malformed/)とは異なります。
- **textのfielddata。** `fielddata:true`なら、解析後のtermによるtext fieldのsortができます。fielddataを使ったtext aggregationは未対応で、fielddataを有効にしていないtext fieldのsortはエラーになります。
- **検索オプション。** `terminate_after`は単一の全体上限です。複数shardでのOpenSearchのshard単位の早期終了は再現しません。`timeout`、`profile`、`rescore`、`script_fields`、`runtime_mappings`など、受理しても無視するオプションがあります。`script_fields`のscriptは実行されず、計算値も返りません。
- **スコア関数。** `function_score`は内側のqueryだけを実行し、関数を警告付きで無視します。`script_score`はPainless未対応のため400を返します。
- **スコア。** bleveのBM25は、Luceneのものとは違います。普通のクエリなら順位は一致しますが、`_score`の値と同点の扱いは一致しません。フィルタは、OpenSearchと同じくスコア計算から除外されます。
- **アナライザ。** 言語別アナライザは、bleveのステマーとストップワードを使います。日本語の分かち書きはkuromojiではなく、IPA辞書を使うkagomeによるもので、未知語では結果が異なることがあります。韓国語と中国語はCJKのbigramです。カスタムアナライザ内の未対応フィルタは、警告を出して読み飛ばします。
- **厳密さ。** OpenSearchはcardinalityとpercentilesを近似値で返しますが、osmemは厳密な値を返します。近似値を前提にしたアサーションは、結果が変わります。
- **事前集計した文書数。** osmemのバケット集計は`_doc_count`を無視します。OpenSearchでは[`_doc_count`](https://docs.opensearch.org/latest/aggregations/bucket/terms/)で事前集計済み文書の件数を反映できます。
- **リフレッシュ。** 書き込みは即座に見えます。書き込みからリフレッシュまでの間の状態を、テストで観測することはできません。

### 既知の動作差

以下はOpenSearch 2.19.1とosmem 2.19.0へのHTTP比較、またはOpenSearch 2.19のソース監査で見つけ、現在の互換性修正後も残っている差です。今回追加したソース監査由来の回帰テストはosmem側の動作を確認していますが、すべてを実サーバーへ再送したわけではありません。オプションを受理しても、効果が同じとは限りません。

| 領域 | 差 |
| --- | --- |
| マッピングと_source | 確認済みimmutable parameterは値を変える更新を拒否します。`norms:false → true`は拒否し、`true → false`は許可します。その他type-specificなmerge規則や、省略時の既定値の扱いは追加比較が必要です。`total_fields.limit`、`depth.limit`、`nested_fields.limit`、`nested_objects.limit`を適用します。`ignore_above:0`は空でないkeyword値をindexしません。mappingの`_source`設定、ルート`enabled:false`、stored field、`doc_values:false`、`dynamic:runtime`の拒否には回帰テストがあります。`strict_allow_templates`と`false_allow_templates`は未対応です。reindexはsource mappingのfilterを適用し、`_source`無効のindexを拒否します。 |
| routingとalias | `_routing.required:true`でroutingがない書き込みを拒否し、GET/HEADとmgetでも必須指定を検証します。`require_alias=true`は実indexや存在しない対象を拒否します。ただしrouting値は文書識別子の一部として保存されません。同じ`_id`を異なるroutingで書くと上書きされ、誤ったroutingを指定しても文書が隠れません。 |
| ドキュメントのメタデータ | DELETEはexternal versionを検査せず、削除tombstoneも保持しません。初回`_seq_no`、レプリカ数を含む書き込み応答の`_shards.total`、512 byteの`_id`上限、DELETEでの`if_seq_no` / `if_primary_term`ペア必須検査、noop updateでのzero shard countには回帰テストがあります。 |
| reindexのvalidation | `max_docs`は0以上の整数でなければなりません。`dest.version_type`は`internal`、`external`、`external_gt`、`external_gte`を受け付けます。externalの3種類はsource versionを引き継ぎ、大小比較を行います。未知のversion typeと無効な`op_type`は拒否します。 |
| settingsとhealth | `_settings/{setting}`は完全一致と`*` / `?`のfilterに対応しますが、`include_defaults=true`はOpenSearchのversionごとの全既定値を返しません。cluster healthは単一data nodeでのprimary/replica数を計算しますが、`level=indices` / `level=shards`の詳細、allocationとwait状態はモデル化していません。 |
| index statsとfield capabilities | [`_stats/{metric}`](https://docs.opensearch.org/2.19/api-reference/index-apis/stats/)は実装済みの`docs`と`store` groupでfilterしますが、store sizeはplaceholderで、search、indexing、cache、segment、shard単位の統計は未実装です。[`/_field_caps`](https://docs.opensearch.org/2.19/api-reference/search-apis/field-caps/)はmapping済みfieldのtypeとsearchable/aggregatable可否、`include_unmapped`を返します。`index_filter`と統合したmapping metadataは未対応で、一部の珍しいfield typeでは型固有のcapability差が残る可能性があります。 |
| その他の検索API | [Validate Query](https://docs.opensearch.org/2.19/api-reference/search-apis/validate/)は対応するquery DSLを検証しますが、正常な`explain=true`の説明と`rewrite=true`の結果は欠けます。文書ID単位の[Explain](https://docs.opensearch.org/2.19/api-reference/search-apis/explain/)、[term vectors](https://docs.opensearch.org/2.19/api-reference/document-apis/termvector/)、[search template](https://docs.opensearch.org/2.19/api-reference/search-apis/search-template/)(`_search/template`、`_msearch/template`、`_render/template`)、[rank evaluation](https://docs.opensearch.org/2.19/api-reference/search-apis/rank-eval/)(`_rank_eval`)、`_search_shards`にはrouteがありません。 |
| クエリと検索結果 | `function_score`は内側queryだけを実行し、score関数や`boost_mode`を無視します。`boosting.negative` / `negative_boost`と`dis_max.tie_breaker`はスコアに反映されません。名前付きqueryを使ってもhitに`matched_queries`が付きません。`geo_distance`は座標を検証し、`ignore_unmapped`と`validation_method:COERCE`を扱いますが、`distance_type:plane`と`IGNORE_MALFORMED`の完全な挙動は再現しません。regexpの`flags`、fuzzyの`max_expansions` / `transpositions`、`indices_boost`、`stats`、`slice`、`search_pipeline`を無視します。ハイライトでは`require_field_match:false`が無視されます。`bool`のshould句だけで明示した`minimum_should_match:0`には回帰テストがあります。 |
| 集計 | composite tuple key、histogramのbucket順序、複数値weightを持つ`weighted_avg`のエラー、既定のsampler上限は回帰テストがあります。terms集計では`shard_size`を無視します。集計とsamplerはosmemの単一の検索ストリームを使うため、OpenSearchのshardごとの候補選択や独立したsamplingは再現しません。 |
| 解析 | `café résumé`のfoldingとngramの`token_chars`境界には回帰テストがあります。LuceneのASCII foldingが扱う全Unicode文字との一致は未検証です。 |

PIT検索では期限を確認し、検索リクエストに`keep_alive`があれば延長します。固定時計を使った回帰テストで確認しています。以前の短いkeep-aliveを用いたHTTP比較では、待機後も両サーバーが200を返し、差は確定できませんでした。

## 未対応

Painless script(`script`、`script_score`、スクリプトによる更新)、`bucket_script`、`bucket_selector`、サジェスター、kNNとニューラル検索、パーコレーター、joinフィールド、spanクエリ、geo shape、significant_termsなどの統計的な集計、Explain、term vectors、search template、rank evaluation、`_search_shards`、`_list/indices`、`_list/shards`、ingestパイプライン、セキュリティ、スナップショット、スタブを超えるノードとシャードの管理、`max_result_window`の変更。

## クライアントごとの注意

- opensearch-go v4はCIでテストしています。go-elasticsearch v8には、確認対象の`X-Elastic-Product`ヘッダを返します。
- olivere/elasticは`/_nodes`をスニッフィングします。osmemはそこに実際のlistenアドレスを返します。
- Elasticsearch 7のバージョン文字列を要求するクライアントには、クラスタ設定`compatibility.override_main_response_version: true`を設定してください。`GET /`が7.10.2を返すようになります。
- エラーは`{"error": {"type", "reason", "root_cause"}, "status"}`の形です。未知のURLには文字列の`error`を持つ400を、誤ったメソッドには405を、OpenSearchと同じく返します。

## 回帰テスト

OpenSearch本体には広範な[2.19系REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/2.19/rest-api-spec/src/main/resources/rest-api-spec/test)があります。実行にはOpenSearchのJavaテストフレームワークが必要なので、osmemのGoテストとしてそのまま動かすことはできません。osmemには上記の挙動を確認するGo回帰テストがありますが、YAML全体を実行する差分ランナーはまだありません。テストが証明するのは、そのテストがassertする挙動に限られます。

別の[`OpenSearch API Specification` project](https://github.com/opensearch-project/opensearch-api-specification/blob/main/TESTING_GUIDE.md)にもYAML storyと`npm run test:spec`のrunnerがあり、`OPENSEARCH_URL`で接続先を指定できます。ガイドのcoverage例ではverb/path組み合わせの約39%を評価しています。request/responseの形を確認する補助には使えます。本体2.19 RESTテスト集の方が複数操作を通じた動作ケースを多く含みますが、osmemで使うにはJava側のtest harnessを適応させる必要があります。
