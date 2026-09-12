---
id: api:go-cluster-api
type: api
title: Go Cluster API
---
Public Go surface of package osmem for building, cloning, serving and seeding a cluster.

```yaml
summary:
  constructors:
    New(opts...): empty cluster; options WithClock, WithWarnings, WithClusterName
  lifecycle:
    Clone(): copy-on-write clone (implemented)
    testing.TB helpers: package osmemtest (Clone, Serve, CloneAndServe, New), implemented
    Close(): release indices
  serving:
    Handler(): http.Handler
    Serve()/MustServe(): loopback listener; Server.URL, Server.Close
  in_process_requests:
    Do(method, path, body): no network; body string/[]byte/io.Reader/any
    helpers: CreateIndex, DeleteIndex, Index, Get, Bulk, BulkString, Search, Count, Indices, Warnings, LoadSeed
  management: Freeze/Unfreeze/Frozen, ManagedClone, CloseManagedClone, ManagedClones (backing api:clone-admin-api)
  japanese: import _ osmem/ja registers kuromoji via system:kagome
  references:
    - concept:cluster
    - requirement:go-test-lifecycle
    - api:rest-compat
```
