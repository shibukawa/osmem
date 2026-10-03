---
id: system:opensearch-serverless
type: system
title: Amazon OpenSearch Serverless
---
AWS serverless OpenSearch offering. Accepts a narrower input space than OpenSearch core or Amazon OpenSearch Service managed domains; code tested only against core-compatible behaviour can fail here. Facts below are evidence for requirement:portable-names and requirement:serverless-portability.

```yaml
facts:
  index_name:
    observed: dot-prefixed index rejected on Serverless; accepted on managed domain (user report 2026-10)
    official_rule: none published for index names
    nearest_evidence:
      lifecycle_policy_resource_regex: 'index/[a-z][a-z0-9-]{3,63}/([a-z;0-9&$%][+.~=\-_a-z;0-9&$%]*)'
      src: https://docs.aws.amazon.com/opensearch-service/latest/ServerlessAPIReference/API_LifecyclePolicyResourceIdentifier.html
      implies: first char in [a-z0-9;&$%]; later chars in [a-z0-9+.~=_;&$%-]; no uppercase, no non-ASCII
    create_index_api: lowercase; must not begin with _ or - (no dot rule stated)
    create_index_src: https://docs.aws.amazon.com/opensearch-service/latest/ServerlessAPIReference/API_CreateIndex.html
  data_access_policy:
    resource: index/<collection|pattern>/<index|pattern>
    wildcard: trailing * only
    dot_prefixed_index: not expressible (user report)
    unmatched_resource: 403 security_exception, not 404
    src: https://docs.aws.amazon.com/opensearch-service/latest/developerguide/serverless-data-access.html
  collection_types:
    search: custom _id allowed
    timeseries: no custom _id on index/create; no upsert
    vectorsearch: custom _id rejected on Classic per client docs; NextGen docs conflict
    error_seen: 400 illegal_argument_exception "Document ID is not supported in create/index operation request"
    src:
      - https://docs.aws.amazon.com/opensearch-service/latest/developerguide/serverless-overview.html
      - https://github.com/opensearch-project/opensearch-py/issues/792
  unsupported_apis:
    list: [_cluster/*, _nodes, _tasks, _stats, _refresh, _flush, _forcemerge, _open, _close, _shrink, _split, _clone, _reindex, _update_by_query, _delete_by_query, _search/scroll, _snapshot, _rollover, _termvectors, _mtermvectors, _search/template, _render/template, _scripts, GET /]
    observed_status: 404
    cat_allowed: [indices, aliases, templates]
    cat_indices_omits: [health, status]
    src: https://docs.aws.amazon.com/opensearch-service/latest/developerguide/serverless-genref.html
  refresh_param:
    true_or_wait_for: 400 status_exception "true refresh policy is not supported"
    src: https://github.com/opensearch-project/opensearch-ruby/issues/131
  settings:
    fixed: [number_of_shards, number_of_replicas, refresh_interval]
    reject_or_ignore: unconfirmed (community says ignored)
  quotas:
    indices_per_collection: 1000
    index_templates_per_collection: 500
    access_policy_bytes: 10240
    src: https://docs.aws.amazon.com/general/latest/gr/opensearch-service.html
  plugins: fixed analysis set (icu, kuromoji, nori, phonetic, smartcn, stempel, ukrainian); no custom plugins; no stored scripts
doc_conflicts:
  - collection name length 3-32 vs 3-64; underscore allowed in console doc only
  - ingest pipelines supported (Classic) vs unsupported (NextGen)
  - vectorsearch custom _id Classic vs NextGen
```
