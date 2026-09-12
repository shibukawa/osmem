---
id: flow:subprocess-test-flow
type: flow
title: Subprocess Test Flow (Java/Python/Node.js)
---
Lifecycle of a non-Go test session driving osmem-server.

```yaml
flow:
  - step: session fixture spawns osmem-server with --seed; waits for ready line
  - step: fixture stores base URL
  - step: per test, fixture POSTs /_osmem/clones and hands the clone URL to the OpenSearch client
  - step: test runs against clone URL
  - step: teardown DELETEs the clone
  - step: session teardown closes stdin / kills the process
references:
  - requirement:multi-language-clients
  - api:server-cli
  - api:clone-admin-api
```
