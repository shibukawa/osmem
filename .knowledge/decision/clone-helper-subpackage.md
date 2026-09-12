---
id: decision:clone-helper-subpackage
type: decision
title: Test Helpers in osmemtest Subpackage
---
The testing.TB-aware helpers live in a separate package `osmemtest`, keeping package osmem free of the `testing` import.

```yaml
summary:
  decided: 2026-09-12
  shape:
    osmemtest.Clone(t testing.TB, base *osmem.Cluster) *osmem.Cluster: clone + t.Cleanup(Close)
    osmemtest.Serve(t testing.TB, c *osmem.Cluster) *osmem.Server: serve + t.Cleanup(Close)
    osmemtest.CloneAndServe(t, base) (*osmem.Cluster, *osmem.Server): the common one-liner
  rationale:
    - production packages should not depend on testing
    - mirrors net/http/httptest layering
  references:
    - requirement:go-test-lifecycle
    - api:go-cluster-api
```
