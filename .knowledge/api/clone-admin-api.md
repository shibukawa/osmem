---
id: api:clone-admin-api
type: api
title: Clone Admin API (HTTP)
---
Planned osmem-specific HTTP endpoints under `/_osmem/` that expose the clone lifecycle to non-Go clients.

```yaml
summary:
  status: implemented (admin.go); Go equivalents Cluster.Freeze/ManagedClone/CloseManagedClone
  endpoints:
    POST /_osmem/base/freeze: mark current state as the base; implicit at first clone (decision:seed-both-ways)
    POST /_osmem/base/unfreeze: allow base writes again
    GET /_osmem/base: frozen flag, clone count, indices
    POST /_osmem/clones: create clone, respond 201 {"id","url"}; url is a new loopback listener bound to that clone
    DELETE /_osmem/clones/{id}: close clone and listener
    GET /_osmem/clones: list; DELETE /_osmem/clones: close all
    GET /_osmem: version, frozen, clone count
  frozen_rule: PUT/POST/DELETE on a frozen base -> 403 osmem_base_frozen, except read-shaped endpoints (_search, _count, _msearch, _mget, _analyze, _refresh, scroll/PIT, _cat, _cluster)
  ownership: closing the base cluster closes its managed clones
  alternatives_considered:
    per_clone_port: chosen; clients only change base URL; no header hacks
    path_prefix: /_osmem/clones/{id}/<normal path>; breaks clients that build paths themselves
    header_routing: X-Osmem-Clone header; requires client configuration hooks in every language
  requirements:
    - creating a clone must be milliseconds (decision:copy-on-write-clone)
    - endpoints must not collide with OpenSearch paths (prefix `_osmem`)
    - Go API users get the same feature via Cluster methods
  references:
    - api:server-cli
    - requirement:multi-language-clients
```
