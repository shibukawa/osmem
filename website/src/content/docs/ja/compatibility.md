---
title: "互換性"
description: "OpenSearch 2.xと比べて、osmemが実装しているもの、近似しているもの、拒否するもの。"
---

フェイクで通ったテストが何かを証明するのは、テストが見ている箇所で、フェイクが本物と同じように振る舞う場合だけです。このページは、osmemについてそれを判断するための一覧です。実装しているもの、近似しているもの、意図的にエラーにするもの。

## 原則

未対応の機能はエラーを返します。多くは400 `unsupported_operation_exception`で、理由は「not supported by osmem」で終わります。それらしい空の結果を返すことはありません。レスポンスの形、ステータスコード、エラー種別はOpenSearch 2.xに合わせてあるので、クライアントライブラリは普段どおりにデコードできます。これまでに見つかった差異には、すべて回帰テストがあります。

## 実装しているもの

**インデックス。** 設定・マッピング・エイリアス付きの作成、ワイルドカードと`_all`での削除、存在確認、取得、`_mapping`の取得と更新(型の衝突はOpenSearchと同じく拒否)、`_settings`、`_stats`、何もしない`_refresh`・`_flush`・`_forcemerge`・`_open`・`_close`、`_analyze`、`_cat/indices`・`_cat/aliases`・`_cat/health`・`_cat/count`、クラスタのhealth・settings・state、スニッフィングするクライアント向けに実際のアドレスを返す`_nodes`。composableとlegacyのインデックステンプレートは、自動作成を含むインデックス作成時に適用されます。

**ドキュメント。** 自動id、`op_type`、`if_seq_no`/`if_primary_term`、external versionに対応した`_doc`、`_create`、`doc`・`doc_as_upsert`・`upsert`・`detect_noop`に対応した`_update`、`_source`、`_mget`、アイテムごとのステータスを返す`_bulk`、`_delete_by_query`、スクリプトなしの`_update_by_query`、`_reindex`。

**マッピング。** text(analyzer、search_analyzer、マルチフィールド)、keyword(normalizer、ignore_above)、すべての数値型、boolean、名前付きフォーマット・Javaパターン・epochフォーマットに対応したdateとdate_nanos、geo_point、ip、objectとnested、`null_value`、`copy_to`、`index: false`、`enabled: false`、`dynamic: true/false/strict`。動的マッピングはOpenSearchに従います。文字列は`.keyword`サブフィールド付きのtextになり、ISO形式の日付は検出され、整数はlong、小数はfloatになります。ドットを含むキーはオブジェクトに展開されます。

**クエリ。** match(operator、minimum_should_match、fuzziness、zero_terms_query)、slop対応のmatch_phrase、match_phrase_prefix、match_bool_prefix、multi_match(best_fields、most_fields、cross_fields、phrase、phrase_prefix、bool_prefix、フィールドごとのブースト、ワイルドカードのフィールド名)、マッピングの型に従うterm、terms lookupを含むterms、数値・日付(日付演算、`format`、`time_zone`)・文字列に対するrange、exists、prefix、wildcard、regexp、fuzzy、ids、4種類の句とminimum_should_matchに対応したbool、constant_score、dis_max、query_stringとsimple_query_string、geo_distance、geo_bounding_box、wrapper。

**検索。** 10,000件のウィンドウまでのfrom/size、フィールドによるソート(order、missing、mode、unmapped_type、format)、`_score`・`_doc`・`_id`でのソート、欠損した数値や日付にOpenSearchと同じ番兵値を使うsearch_after、scroll、point in time、`_source`のフィルタリング、`fields`、`docvalue_fields`、`version`、`seq_no_primary_term`、`track_total_hits`、`track_scores`、`min_score`、`post_filter`、`collapse`、ハイライト、`_count`、`_msearch`、`filter_path`、`pretty`、`rest_total_hits_as_int`、gzipで圧縮されたリクエストボディ。

**集計。** terms(size、件数・キー・サブ集計による並べ替え、min_doc_count、missing、include/exclude)、multi_terms、range、date_range、histogram、date_histogram(calendarとfixedの間隔、time_zone、offset、format、extended_bounds、空バケットの補完)、filter、filters、missing、global、そのまま通すだけのnestedとreverse_nested、`after`付きのcomposite、avg、sum、min、max、value_count、stats、extended_stats、cardinality、percentiles、percentile_ranks、top_hits、weighted_avg、median_absolute_deviation。パイプライン集計はcumulative_sum、derivative、bucket_sort、avg/sum/min/max/stats_bucket。サブ集計は入れ子にできます。

**解析。** standard、simple、whitespace、keyword、stop、patternと、bleveの言語別アナライザ。トークナイザ(standard、whitespace、keyword、letter、pattern、ngram、edge_ngram、char_group、uax_url_email)、トークンフィルタ(lowercase、asciifolding、stop、ngram、edge_ngram、shingle、stemmer、snowball、porter_stem、truncate、length、unique、reverse、cjk_bigram、cjk_width)、文字フィルタ(html_strip、pattern_replace)から組み立てるカスタムアナライザ。normalizer。日本語サポートを有効にすれば、kuromojiアナライザ、モードとユーザー辞書に対応したkuromoji_tokenizer、kuromoji_baseform、kuromoji_part_of_speech、cjk_width、ja_stop、kuromoji_stemmer、kuromoji_readingform。

## 近似しているもの

- **スコア。** bleveのBM25は、Luceneのものとは違います。普通のクエリなら順位は一致しますが、`_score`の値と同点の扱いは一致しません。フィルタは、OpenSearchと同じくスコア計算から除外されます。
- **nestedドキュメント**はフラットに扱われます。`nested`クエリは、配列内のどこかでフィールドが一致すれば、`object`フィールドと同じようにヒットします。
- **アナライザ。** 言語別アナライザは、bleveのステマーとストップワードを使います。日本語の分かち書きはkuromojiではなく、IPA辞書を使うkagomeによるもので、未知語では結果が異なることがあります。韓国語と中国語はCJKのbigramです。カスタムアナライザ内の未対応フィルタは、警告を出して読み飛ばします。
- **厳密さ。** OpenSearchはcardinalityとpercentilesを近似値で返しますが、osmemは厳密な値を返します。近似値を前提にしたアサーションは、結果が変わります。
- **リフレッシュ。** 書き込みは即座に見えます。書き込みからリフレッシュまでの間の状態を、テストで観測することはできません。

## 未対応

あらゆる形のPainless(`script`、`script_score`、`function_score`の関数、スクリプトによる更新、`bucket_script`、`bucket_selector`)、サジェスター、kNNとニューラル検索、パーコレーター、joinフィールド、spanクエリ、geo shape、significant_termsなどの統計的な集計、ingestパイプライン、セキュリティ、スナップショット、スタブを超えるノードとシャードの管理、`max_result_window`の変更。

## クライアントごとの注意

- opensearch-go v4はCIでテストしています。go-elasticsearch v8には、確認対象の`X-Elastic-Product`ヘッダを返します。
- olivere/elasticは`/_nodes`をスニッフィングします。osmemはそこに実際のlistenアドレスを返します。
- Elasticsearch 7のバージョン文字列を要求するクライアントには、クラスタ設定`compatibility.override_main_response_version: true`を設定してください。`GET /`が7.10.2を返すようになります。
- エラーは`{"error": {"type", "reason", "root_cause"}, "status"}`の形です。未知のURLには文字列の`error`を持つ400を、誤ったメソッドには405を、OpenSearchと同じく返します。
