---
id: decision:kuromoji-as-plugin
type: decision
title: Japanese Analysis Follows OpenSearch's Plugin Model
---
Because OSS OpenSearch ships kuromoji as an installable plugin (analysis-kuromoji) rather than in the default distribution, osmem treats Japanese analysis as opt-in in Go and bundles it in the server binary.

```yaml
summary:
  decided: 2026-09-12
  fact: opensearch-plugin install analysis-kuromoji is required on self-managed clusters; managed services (Amazon OpenSearch Service) preinstall it
  go: import _ github.com/shibukawa/osmem/ja (static plugin), unchanged
  server:
    proposed: always compile system:kagome into osmem-server; a prebuilt binary cannot be extended by import and a second binary doubles packaging
    escape_hatch: --no-ja flag to fall back to CJK bigrams when a suite must mimic a cluster without the plugin
  references:
    - decision:bundled-binaries
    - api:server-cli
```
