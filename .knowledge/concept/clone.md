---
id: concept:clone
type: concept
title: Clone
---
A Clone is a Cluster derived from another Cluster that starts with identical observable state and diverges independently afterwards.

Use a clone when a test changes index state (documents, mappings, or index lifecycle); read-only tests can share the base.

```yaml
summary:
  guarantees:
    - reads see the parent's state at clone time
    - writes on either side are invisible to the other
    - closing one side does not affect the other
  not_copied: open scroll/PIT contexts
  references:
    - decision:copy-on-write-clone
```
