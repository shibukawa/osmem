---
id: doc:go-guide
type: doc
title: Go Guide Page
---
Using osmem in-process from Go tests with the osmemtest helpers.

```yaml
summary:
  sections:
    - install osmem and optional official opensearch-go client
    - start, register schema, seed data, and use official client or in-process Do
    - fresh cluster per test with osmemtest.New
    - per-test isolation with osmemtest.CloneAndServe; read-only tests share the base
    - first write forks the touched index through copy-on-write; close discards test changes
    - t.Parallel rules (every writer owns a clone)
    - talking to it: opensearch-go client, c.Do without network, helpers (Index, Search, Count)
    - Japanese analysis via import _ osmem/ja
    - options: WithClock (date math), WithWarnings, Freeze
    - Handler() for custom servers / httptest
  references:
    - api:go-cluster-api
    - decision:clone-helper-subpackage
    - flow:go-test-flow
```
