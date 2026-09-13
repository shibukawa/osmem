---
id: doc:seed-data
type: doc
title: Seed Data Page
---
How to describe fixtures: seed directory layout, bulk NDJSON, REST seeding then freeze.

```yaml
summary:
  sections:
    - directory layout and load order (template, index, ndjson, aliases)
    - writing index bodies (mappings, analyzers) and NDJSON actions
    - single .ndjson file mode
    - seeding through the API + POST /_osmem/base/freeze
    - Go: LoadSeed; CLI: --seed (repeatable)
    - tips: keep fixtures small, deterministic ids, kuromoji only with ja
  references:
    - decision:seed-both-ways
    - api:server-cli
```
