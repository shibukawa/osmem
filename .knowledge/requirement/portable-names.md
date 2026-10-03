---
id: requirement:portable-names
type: requirement
title: Portable Resource Names
---
Every name-creating path rejects names that are not portable per rule:portable-index-name, under decision:stricter-than-opensearch.

```yaml
scope:
  index_create:
    - PUT /{index} (CreateIndexWithParams)
    - auto-create via index/create/update/bulk/_reindex dest (ensureIndex)
    - date-math names are checked after resolution
  alias_create:
    - POST /_aliases add, PUT /{index}/_alias/{name} (applyAliasActions)
    - aliases in create-index body and template aliases after {index} substitution (validateCreateAlias)
  templates: index_patterns and template alias names are not checked at put time; the created index/alias is
  data_streams: core already rejects leading '.'; osmem has no data streams
not_in_scope:
  - read/search/delete on a name: non-existent dot names keep core behaviour (404)
acceptance:  # portability_test.go
  - PUT /.foo -> 400 invalid_index_name_exception, also with index.hidden=true
  - POST /.foo/_doc -> 400, index not auto-created
  - bulk item to .foo -> per-item 400, other items succeed
  - alias .a or MyAlias -> 400 invalid_alias_name_exception
  - template alias .{index}-alias -> 400 at index creation
  - non-ASCII or '@' names -> warning (default), 400 (serverless mode)
  - WithDotNames -> .kibana accepted, inherited by clones; lowercase alias rule still applies
  - existing core rejections unchanged (_, -, +, :, #, uppercase, 255 bytes)
status: implemented 2026-10-03
references:
  - system:opensearch-serverless
  - policy:fidelity-first
```
