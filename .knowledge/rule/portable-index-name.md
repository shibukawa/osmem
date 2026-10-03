---
id: rule:portable-index-name
type: rule
title: Portable Index Name Rule
---
Name check osmem applies on top of OpenSearch core validation so that any accepted name is valid on managed domains and on system:opensearch-serverless. Implemented in internal/engine/portability.go.

```yaml
core_rules_kept:  # MetadataCreateIndexService.validateIndexOrAliasName / validateIndexName, 2.x
  forbidden_chars: '\ / * ? " < > | space , # :'
  forbidden_first: '_ - +'
  forbidden_whole: ['', '.', '..']
  lowercase: index names only (core does not lowercase-check aliases)
  max_len: 255 UTF-8 bytes
  order: core checks run first; portability checks after
  src: https://github.com/opensearch-project/OpenSearch/blob/2.x/server/src/main/java/org/opensearch/cluster/metadata/MetadataCreateIndexService.java
added_rules:
  - id: no-leading-dot
    applies_to: [index, alias]
    reject: name starts with '.'
    core_behaviour: accepted; deprecation warning index_name_starts_with_dot unless system index or index.hidden=true
    applies_even_if: index.hidden=true
    opt_out: osmem.WithDotNames() / --allow-dot-names
  - id: alias-lowercase
    applies_to: [alias]
    reject: upper case letters
    opt_out: none
  - id: serverless-charset
    applies_to: [index, alias]
    allow: '^[a-z0-9;&$%][a-z0-9+.~=_;&$%-]*$'  # leading '.' judged by no-leading-dot
    default: warning via Cluster.Warn
    serverless_mode: reject
    source: lifecycle policy ResourceName regex (indirect evidence)
error_shape:
  index: 400 invalid_index_name_exception "Invalid index name [<name>], must not start with '.'"
  index_charset: 400 invalid_index_name_exception "Invalid index name [<name>], must contain only [a-z0-9+.~=_;&$%-] and must start with [a-z0-9;&$%]"
  alias: 400 invalid_alias_name_exception "Invalid alias name [<name>]: must not start with '.' | must be lowercase | <charset>"
```
