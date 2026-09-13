---
id: doc:node-guide
type: doc
title: Node.js Guide Page
---
Using @osmem/core with Jest, Vitest or node:test.

```yaml
summary:
  sections:
    - install @osmem/core and official @opensearch-project/opensearch client (platform binary is optional dependency)
    - start server, register schema, seed data, and query through official client
    - fresh server per test or one server per file/worker
    - start once per file/suite in beforeAll, close in afterAll
    - withClone for tests that mutate documents, mappings, or index lifecycle; read-only tests use server.url
    - clone is a writable fork; closing it discards test side effects
    - options (seed, freeze, japanese, binary), OSMEM_SERVER_BIN, CommonJS entry
    - lifetime: stdin pipe + parent pid, no zombie servers
  references:
    - api:server-cli
    - api:clone-admin-api
    - flow:subprocess-test-flow
```
