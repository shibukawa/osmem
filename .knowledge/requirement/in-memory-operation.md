---
id: requirement:in-memory-operation
type: requirement
title: In-Memory Operation
---
All cluster state (indices, mappings, documents, aliases, templates) lives in process memory; nothing is written to disk and no external daemon is required.

```yaml
summary:
  rationale: test isolation, speed, portability (macOS/Linux/Windows, no cgo)
  implementation: concept:cluster holds Go maps; decision:bleve-engine uses in-memory scorch
  acceptance:
    - go test runs with no Docker and no temp files
    - startup of an empty cluster < 10 ms
    - process exit discards everything
  references:
    - vision:osmem
```
