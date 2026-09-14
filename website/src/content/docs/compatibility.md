---
title: "Compatibility"
description: "What osmem implements, what it approximates, and what it rejects compared with OpenSearch 2.x."
---

A test that passes on a fake proves something only if the fake behaves like the real system where the test looks. This page is the list you need to judge that for osmem: what is implemented, what is approximated, and what fails on purpose.

## The rule

Unsupported features generally return an error, usually 400 `unsupported_operation_exception` with a reason ending in "not supported by osmem", rather than a plausible empty result. Response shapes, status codes and error types follow OpenSearch 2.x so that client libraries decode them normally. The cases listed here describe tested behavior, not complete parity with every OpenSearch feature.

## Implemented

**Indices.** Create with settings, mappings and aliases; delete with wildcards and `_all`; exists; get; `_mapping` get and put (type conflicts are rejected as on OpenSearch); `_settings`; `_stats`; `_refresh`, `_flush`, `_forcemerge`, `_open`, `_close` as no-ops; `_analyze`; `_cat/indices`, `_cat/aliases`, `_cat/health`, `_cat/count`; cluster health, settings, state; `_nodes` with the real address for sniffing clients. Composable and legacy index templates apply on creation, including auto-created indices.

**Documents.** `_doc` with auto ids, `op_type`, `if_seq_no`/`if_primary_term`, external versions; `_create`; `_update` with `doc`, `doc_as_upsert`, `upsert`, `detect_noop`; `_source`; `_mget`; `_bulk` with per-item status; `_delete_by_query`; `_update_by_query` without a script; `_reindex`.

**Mapping.** text (analyzer, search_analyzer, multi-fields), keyword (normalizer, ignore_above), every numeric type, boolean, date and date_nanos with named formats, Java patterns and epoch formats, geo_point, ip, object and nested, `null_value`, `copy_to`, `index: false`, `enabled: false`, `dynamic: true/false/strict`, dynamic templates for inferred fields, and `coerce:false` on numeric fields. Dynamic mapping follows OpenSearch: strings become text with a `.keyword` sub-field, ISO dates are detected, integers become long and decimals float; dotted keys expand into objects.

**Queries.** match (operator, minimum_should_match, fuzziness, zero_terms_query), match_phrase with slop, match_phrase_prefix, match_bool_prefix, multi_match (best_fields, most_fields, cross_fields, phrase, phrase_prefix, bool_prefix, field boosts, wildcard field names), term typed by the mapping (including exact `long` term matching beyond float64 precision), terms including terms lookup, range on numbers, dates with date math, `format` and `time_zone`, and strings, exists, prefix, wildcard, regexp, fuzzy, ids, bool with all four clauses and minimum_should_match, constant_score, dis_max, query_string and simple_query_string, geo_distance, geo_bounding_box, wrapper. `function_score` runs its inner query but ignores scoring functions.

**Search.** from/size up to the 10,000 window, sort by field with order, missing, mode, unmapped_type and format, `_score`, `_doc`, `_id`, search_after with OpenSearch's sentinel values for missing numbers and dates, scroll, point in time snapshots, `_source` filtering, `fields`, `docvalue_fields`, `version`, `seq_no_primary_term`, `track_total_hits`, `track_scores`, `min_score`, `post_filter`, `collapse` with `inner_hits`, highlight, `_count`, `_msearch`, `filter_path`, `pretty`, `rest_total_hits_as_int`, gzip request bodies. Search totals count matching documents before `search_after` paging or collapse. `terminate_after` applies a single global hit limit; shard-local early termination is not modeled.

**Aggregations.** terms (size, order by count, key or sub-aggregation, min_doc_count, missing, include/exclude), multi_terms, range, date_range, histogram, date_histogram (calendar and fixed intervals, time_zone, offset, format, extended_bounds, gap filling), filter, filters, missing, global, nested and reverse_nested over the nested objects, composite with `after`, avg, sum, min, max, value_count, stats, extended_stats, cardinality, percentiles, percentile_ranks, top_hits, weighted_avg, median_absolute_deviation; pipelines cumulative_sum, derivative, bucket_sort, avg/sum/min/max/stats_bucket. Sub-aggregations nest.

**Analysis.** standard, simple, whitespace, keyword, stop, pattern and bleve's language analyzers; custom analyzers from tokenizers (standard, whitespace, keyword, letter, pattern, ngram, edge_ngram, char_group, uax_url_email), token filters (lowercase, asciifolding, stop, ngram, edge_ngram, shingle, stemmer, snowball, porter_stem, truncate, length, unique, reverse, cjk_bigram, cjk_width) and char filters (html_strip, pattern_replace); normalizers. With Japanese support: kuromoji analyzer, kuromoji_tokenizer with modes, user dictionaries, kuromoji_baseform, kuromoji_part_of_speech, cjk_width, ja_stop, kuromoji_stemmer, kuromoji_readingform.

## Approximated

- **Scores.** bleve's BM25 is not Lucene's. Rankings agree for ordinary queries; `_score` values and ties do not. Filters are excluded from scoring as on OpenSearch.
- **Numeric range and sort precision.** Integer term queries retain exact integral values, but large `long` range queries and sorting still use float64-backed numeric values. Numeric subtype bounds and `scaled_float` rounding are not fully modeled.
- **Malformed mapped values.** `ignore_malformed:true` is accepted in mappings but does not let a document with an invalid field value be stored while skipping that field; indexing can still reject the whole document. OpenSearch supports this behavior through [`ignore_malformed`](https://docs.opensearch.org/latest/mappings/mapping-parameters/ignore-malformed/).
- **Text fielddata.** Sorting a `text` field is enabled when `fielddata:true`; osmem sorts the field's analyzed terms. Text aggregations with fielddata are still rejected, and sorting without fielddata remains an error.
- **Ignored search options.** `timeout`, `profile`, `rescore`, `script_fields`, `runtime_mappings` and some other accepted options are currently ignored. `script_fields` does not execute its script or return computed values.
- **`function_score`.** The inner query runs, but scoring functions are ignored with a warning. `script_score` returns 400 because Painless execution is not supported.
- **Analyzers.** Language analyzers use bleve's stemmers and stop lists. Japanese segmentation comes from kagome with the IPA dictionary rather than kuromoji and can differ on unknown words; Korean and Chinese are CJK bigrams. Unsupported filters in a custom analyzer are skipped with a warning.
- **Exactness.** cardinality and percentiles are exact, while OpenSearch approximates them; a test asserting an approximate value would differ.
- **Pre-aggregated document counts.** Bucket aggregations ignore `_doc_count`; OpenSearch can use [`_doc_count`](https://docs.opensearch.org/latest/aggregations/bucket/terms/) to count pre-aggregated documents.
- **Refresh.** Writes are visible immediately. A test cannot observe the window between a write and a refresh.

## Not supported

Painless scripts (`script`, `script_score`, scripted updates), `bucket_script`, `bucket_selector`, suggesters, kNN and neural search, percolator, join fields, span queries, geo shapes, significant_terms and other statistical aggregations, ingest pipelines, security, snapshots, node and shard management beyond stubs, changing `max_result_window`.

## Client notes

- opensearch-go v4 is tested in CI. go-elasticsearch v8 receives the `X-Elastic-Product` header it checks.
- olivere/elastic sniffs `/_nodes`; osmem reports the real listen address there.
- For clients that insist on an Elasticsearch 7 version string, set the cluster setting `compatibility.override_main_response_version: true`; `GET /` then reports 7.10.2.
- Errors follow the `{"error": {"type", "reason", "root_cause"}, "status"}` shape; unknown URLs return 400 with a string `error`, wrong methods 405, as on OpenSearch.

## Regression testing

The OpenSearch project maintains a broad [REST API YAML test suite](https://github.com/opensearch-project/OpenSearch/tree/main/rest-api-spec/src/main/resources/rest-api-spec/test). Its runner is part of OpenSearch's Java test framework, so those YAML files do not run directly as osmem Go tests. osmem has focused Go regression tests for the behaviors described above, but does not yet have a general differential runner for the full YAML corpus. A passing osmem test therefore establishes only the behavior asserted by that test.
