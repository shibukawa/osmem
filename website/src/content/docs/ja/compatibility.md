---
title: "互換性"
description: "OpenSearch 2.xと比べて、osmemが実装しているもの、近似しているもの、拒否するもの。"
---

フェイクで通ったテストが何かを証明するのは、テストが見ている箇所で、フェイクが本物と同じように振る舞う場合だけです。このページは、osmemについてそれを判断するための一覧です。実装しているもの、近似しているもの、意図的にエラーにするもの。

## 原則

明示的に未対応とした機能の多くは400 `unsupported_operation_exception`を返します。一方、近似した動作をする機能や、受理して無視する検索オプションもあります。そうした差は以下に記載します。回帰テストは個々の挙動を確認するもので、OpenSearchの全機能との同等性を保証するものではありません。

## 実装しているもの

**インデックス。** 設定・マッピング・エイリアス付きの作成、ワイルドカードと`_all`での削除、存在確認、取得、`_mapping`の取得と更新(型の衝突はOpenSearchと同じく拒否)、`_settings`、`_stats`、何もしない`_refresh`・`_flush`・`_forcemerge`・`_open`・`_close`、`_analyze`、`_cat/indices`・`_cat/aliases`・`_cat/health`・`_cat/count`、クラスタのhealth・settings・state、スニッフィングするクライアント向けに実際のアドレスを返す`_nodes`。composableとlegacyのインデックステンプレートは、自動作成を含むインデックス作成時に適用されます。

**ドキュメント。** 自動id、`op_type`、`if_seq_no`/`if_primary_term`、external versionに対応した`_doc`、`_create`、`doc`・`doc_as_upsert`・`upsert`・`detect_noop`に対応した`_update`、`_source`、`_mget`、アイテムごとのステータスを返す`_bulk`、`_delete_by_query`、スクリプトなしの`_update_by_query`、`_reindex`。

**マッピング。** text(analyzer、search_analyzer、マルチフィールド)、keyword(normalizer、ignore_above)、すべての数値型、boolean、名前付きフォーマット・Javaパターン・epochフォーマットに対応したdateとdate_nanos、geo_point、ip、objectとnested、`null_value`、`copy_to`、`index: false`、`enabled: false`、`dynamic: true/false/strict`、動的フィールド向けのdynamic template、数値フィールドの`coerce:false`。動的マッピングはOpenSearchに従います。文字列は`.keyword`サブフィールド付きのtextになり、ISO形式の日付は検出され、整数はlong、小数はfloatになります。ドットを含むキーはオブジェクトに展開されます。

**クエリ。** match(operator、minimum_should_match、fuzziness、zero_terms_query)、slop対応のmatch_phrase、match_phrase_prefix、match_bool_prefix、multi_match(best_fields、most_fields、cross_fields、phrase、phrase_prefix、bool_prefix、フィールドごとのブースト、ワイルドカードのフィールド名)、マッピングの型に従うterm(2^53を超えるlongの完全な整数値を含む)、terms lookupを含むterms、数値・日付(日付演算、`format`、`time_zone`)・文字列に対するrange、exists、prefix、wildcard、regexp、fuzzy、ids、4種類の句とminimum_should_matchに対応したbool、constant_score、dis_max、query_stringとsimple_query_string、geo_distance、geo_bounding_box、wrapper。`function_score`は内側のqueryだけを実行し、スコア関数は無視します。

**検索。** 10,000件のウィンドウまでのfrom/size、フィールドによるソート(order、missing、mode、unmapped_type、format)、`_score`・`_doc`・`_id`でのソート、欠損した数値や日付にOpenSearchと同じ番兵値を使うsearch_after、scroll、作成時点のindexを保持するpoint in time、`_source`のフィルタリング、`fields`、`docvalue_fields`、`version`、`seq_no_primary_term`、`track_total_hits`、`track_scores`、`min_score`、`post_filter`、`inner_hits`付きの`collapse`、ハイライト、`_count`、`_msearch`、`filter_path`、`pretty`、`rest_total_hits_as_int`、gzipで圧縮されたリクエストボディ。totalはsearch_afterによるページングやcollapseの前に一致した文書数を返します。`terminate_after`は単一の全体上限として適用します。

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

## 未対応

Painless script(`script`、`script_score`、スクリプトによる更新)、`bucket_script`、`bucket_selector`、サジェスター、kNNとニューラル検索、パーコレーター、joinフィールド、spanクエリ、geo shape、significant_termsなどの統計的な集計、ingestパイプライン、セキュリティ、スナップショット、スタブを超えるノードとシャードの管理、`max_result_window`の変更。

## クライアントごとの注意

- opensearch-go v4はCIでテストしています。go-elasticsearch v8には、確認対象の`X-Elastic-Product`ヘッダを返します。
- olivere/elasticは`/_nodes`をスニッフィングします。osmemはそこに実際のlistenアドレスを返します。
- Elasticsearch 7のバージョン文字列を要求するクライアントには、クラスタ設定`compatibility.override_main_response_version: true`を設定してください。`GET /`が7.10.2を返すようになります。
- エラーは`{"error": {"type", "reason", "root_cause"}, "status"}`の形です。未知のURLには文字列の`error`を持つ400を、誤ったメソッドには405を、OpenSearchと同じく返します。

## 回帰テスト

OpenSearch本体には広範な[REST API YAMLテスト集](https://github.com/opensearch-project/OpenSearch/tree/main/rest-api-spec/src/main/resources/rest-api-spec/test)があります。実行にはOpenSearchのJavaテストフレームワークが必要なので、osmemのGoテストとしてそのまま動かすことはできません。osmemには上記の挙動を確認するGo回帰テストがありますが、YAML全体を実行する差分ランナーはまだありません。テストが証明するのは、そのテストがassertする挙動に限られます。
