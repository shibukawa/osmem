---
id: doc:node-guide
type: doc
title: Node.js Guide Page
---
Using @osmem/core with Jest, Vitest or node:test.

```yaml
summary:
  sections:
    - install @osmem/core (platform packages arrive as optional dependencies)
    - start once per file/suite in beforeAll, close in afterAll
    - withClone per test; passing clone.url to @opensearch-project/opensearch
    - options (seed, freeze, japanese, binary), OSMEM_SERVER_BIN, CommonJS entry
    - lifetime: stdin pipe + parent pid, no zombie servers
  references:
    - api:server-cli
    - api:clone-admin-api
    - flow:subprocess-test-flow
```
