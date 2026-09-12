---
id: api:rest-compat
type: api
title: OpenSearch REST Compatibility Surface
---
The HTTP handler implements the OpenSearch 2.x REST subset used by clients: index and document CRUD, bulk, search DSL, aggregations, scroll/PIT, aliases, templates, cat and cluster info.

```yaml
summary:
  tested_clients: [opensearch-go v4]
  intended_clients: [go-elasticsearch v8 (product header sent), olivere/elastic (real publish_address), opensearch-java, opensearch-py, @opensearch-project/opensearch]
  unsupported_returns: 400 unsupported_operation_exception, never silent empty results
  compat_mode: cluster setting compatibility.override_main_response_version reports ES 7.10.2
  references:
    - policy:fidelity-first
    - requirement:multi-language-clients
```
