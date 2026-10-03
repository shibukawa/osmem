---
id: requirement:serverless-portability
type: requirement
title: Serverless Mode
---
Opt-in mode emulating one system:opensearch-serverless collection type: osmem.WithServerless(type) / osmem-server --serverless type, type in search|timeseries|vectorsearch. Off by default because the restrictions break ordinary OpenSearch test suites (refresh=true is common). Default-mode checks live in requirement:portable-names.

```yaml
implemented:
  - id: api-allowlist
    rule: only routes in AWS "Supported OpenSearch API operations" table (http_serverless.go serverlessRoutes); read APIs accept GET and POST
    else: 404 empty body (observed Serverless behaviour)
    blocked_examples: [GET /, _refresh, scroll, _reindex, _update_by_query, _delete_by_query, _cluster/*, _nodes, _stats, _open, _close, _template, _termvectors, _cat except indices/aliases/templates]
  - id: refresh-param
    rule: non-GET request with refresh "" | true | wait_for -> 400 status_exception "<true|wait_for> refresh policy is not supported."
    evidence: true wording observed; wait_for wording inferred
  - id: custom-doc-id
    collections: [timeseries, vectorsearch]
    rule: PUT/POST _doc/{id}, _create/{id} -> 400 illegal_argument_exception "Document ID is not supported in create/index operation request"; _update/{id} and bulk update -> "... in update operation request" (inferred wording); bulk index/create with _id -> per-item 400; delete by id allowed
    note: vectorsearch Classic vs NextGen docs conflict; Classic behaviour chosen
  - id: charset
    rule: rule:portable-index-name serverless-charset rejects instead of warning
  - id: fixed-settings
    rule: number_of_shards, number_of_replicas, refresh_interval in create body or _settings -> warning only (reject vs ignore unconfirmed)
  - id: cat-indices
    rule: _cat/indices drops health and status columns
inheritance: clones (Clone, management API clones) keep the mode and WithDotNames
backlog:
  - core-deprecations: date_histogram interval, indices_boost object form, field boost, unmapped_type string, multiple legacy template match, translog retention, simplefs, template param in put template, GET /_cat/master; check osmem behaviour, then reject per decision:stricter-than-opensearch
  - Warning response headers for core deprecations osmem accepts
  - quotas (1000 indices, 500 templates per collection): ignored
already_matching_core:
  - _id > 512 bytes rejected
  - _type in bulk metadata rejected
  - typed mappings rejected
packages: python OsmemServer.start(allow_dot_names, serverless) + pytest ini; node StartOptions.allowDotNames/serverless; java Builder.allowDotNames/serverless
references:
  - api:go-cluster-api
  - api:server-cli
  - api:rest-compat
```
