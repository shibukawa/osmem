---
id: api:server-cli
type: api
title: osmem-server Command
---
Planned standalone binary that hosts one base cluster and serves the REST API for non-Go test suites.

```yaml
summary:
  status: implemented (cmd/osmem-server, internal/serve)
  invocation: osmem-server [--addr 127.0.0.1:0] [--seed DIR|FILE ...] [--freeze] [--no-ja] [--parent-pid N] [--no-stdin-watch]
  seed_input:
    - directory: <name>.template.json, <index>.index.json, <index>.ndjson, aliases.json (applied in that order); Go equivalent Cluster.LoadSeed
    - or a single bulk NDJSON file (indices auto-created with dynamic mapping)
    - or none: client seeds via REST then freezes the base (api:clone-admin-api)
  startup_contract:
    - print `{"url","pid","version","japanese","indices"}` line on stdout when ready (~1.3 s with kagome dictionary, ~10 ms without)
    - exit on stdin EOF (default), SIGTERM/SIGINT, or when --parent-pid disappears
  binary_size_mb: 39 (kagome included)
  references:
    - decision:subprocess-for-other-languages
    - decision:seed-both-ways
    - decision:kuromoji-as-plugin
    - api:clone-admin-api
```
