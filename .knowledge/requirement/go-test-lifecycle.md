---
id: requirement:go-test-lifecycle
type: requirement
title: Go Test Lifecycle
---
Go tests build one base cluster in TestMain (schema + seed data) and each test case obtains an isolated copy that is released automatically when the test ends.

```yaml
summary:
  base_setup:
    where: TestMain
    steps: create indices, put mappings, load seed documents (bulk NDJSON), aliases
  per_test:
    call: osmemtest.Clone(t, base) (decision:clone-helper-subpackage)
    release: registered with t.Cleanup; no manual Close in test bodies
    isolation: writes in one test never reach the base or other tests
  reuse:
    allowed: tests that only read may share one clone or the base itself
    reason: search fixtures rarely change after initialization
  cost_targets:
    clone_read_only: microseconds
    clone_first_write: proportional to touched index size
  status:
    implemented: New, Clone() (no testing.TB), Close, Serve, Do, Bulk
    osmemtest: implemented (Clone, Serve, CloneAndServe, New)
  references:
    - api:go-cluster-api
    - decision:copy-on-write-clone
    - flow:go-test-flow
    - requirement:seed-once-reuse
```
