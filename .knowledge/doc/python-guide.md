---
id: doc:python-guide
type: doc
title: Python Guide Page
---
Using osmem-server (import osmem_server) with pytest fixtures or directly.

```yaml
summary:
  sections:
    - install osmem-server (platform wheel bundles the binary)
    - pytest: osmem_seed/osmem_freeze/osmem_japanese ini options; fixtures osmem_server, osmem_clone, osmem_url
    - overriding osmem_server in conftest for custom start options
    - without pytest: OsmemServer.start context manager, clone()
    - opensearch-py example, OSMEM_SERVER_BIN
  references:
    - api:server-cli
    - api:clone-admin-api
```
