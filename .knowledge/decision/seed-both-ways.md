---
id: decision:seed-both-ways
type: decision
title: Seed by Directory and by REST
---
osmem-server accepts seed data both as a directory passed on the command line and as REST writes followed by an explicit freeze.

```yaml
summary:
  decided: 2026-09-12
  directory_mode: --seed DIR; declarative, reviewable in git, no test code needed
  rest_mode: client seeds through normal OpenSearch API, then POST /_osmem/base/freeze; keeps seeding logic in the test language
  rule: after freeze (explicit or implicit at first clone) the base is immutable; further writes to the base URL return 403 osmem_base_frozen
  references:
    - api:server-cli
    - api:clone-admin-api
    - requirement:seed-once-reuse
```
