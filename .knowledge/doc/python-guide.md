---
id: doc:python-guide
type: doc
title: Python Guide Page
---
Using osmem-server (import osmem_server) with pytest fixtures or directly.

```yaml
summary:
  sections:
    - install osmem-server, official opensearch-py client, and pytest as needed (platform wheel bundles binary)
    - start server, register schema, seed data, and query through official client
    - pytest: osmem_seed/osmem_freeze/osmem_japanese ini options; fixtures osmem_server, osmem_clone, osmem_url
    - overriding osmem_server in conftest for custom start options
    - without pytest: OsmemServer.start context manager, clone()
    - choose function/class/session server lifetime; use function-scoped osmem_clone as a writable fork
    - opensearch-py example, OSMEM_SERVER_BIN
  references:
    - api:server-cli
    - api:clone-admin-api
```
