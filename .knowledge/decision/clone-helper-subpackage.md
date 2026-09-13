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
  no_testmain_wrapper:
    decided: 2026-09-13
    rule: no API takes *testing.M or owns the test binary lifecycle; TestMain builds the base, `defer base.Close()`, and returns after m.Run() (Go >= 1.15 exits with m.Run's code)
    reason: suites combine osmem with other in-memory fakes (pgmem for PostgreSQL, valkey fakes); a framework-style Main wrapper per library cannot compose
  references:
    - requirement:go-test-lifecycle
    - api:go-cluster-api
```
