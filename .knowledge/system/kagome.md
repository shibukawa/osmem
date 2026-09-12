---
id: system:kagome
type: system
title: kagome
---
Pure Go Japanese morphological analyzer (github.com/ikawaha/kagome/v2) with IPA dictionary, backing osmem's kuromoji analyzer, tokenizer and filters.

```yaml
summary:
  version: v2.11.0, kagome-dict/ipa v1.2.6
  cost: +8 MB binary, dictionary loads once per process
  opt_in: import _ github.com/shibukawa/osmem/ja
  references:
    - api:go-cluster-api
    - api:server-cli
```
