---
title: "Getting started"
description: "What osmem is, the base-and-clone model, and how to install it for Go, Node.js, Python and Java."
---

Tests that touch OpenSearch usually pay twice: once to start a container, and again on every test that has to wait for a refresh before its documents become searchable. osmem keeps the same REST-facing workflow but replaces the heavyweight service with an in-memory Go server. In a local measurement, its seeded child process became ready in 9–13 ms after the first launch; the OpenSearch Docker comparison took 9.4 seconds to accept setup requests. The measurements are workload-specific, so the [benchmark page](../performance/) shows the conditions before drawing a conclusion.

## What it is, and what it is not

osmem speaks the OpenSearch 2.x REST API: index and document CRUD, bulk, the query DSL, aggregations, scroll, aliases, templates. Existing clients work unchanged. Under the hood it is not OpenSearch. bleve, a pure Go search library, provides the inverted index and BM25 scoring; everything else (mapping semantics, sorting, aggregations, highlighting) is reimplemented in Go against the stored documents.

That design sets the limits. Rankings agree with OpenSearch for ordinary queries, but the numeric `_score` values differ, so a test must not assert on them. Painless scripts are not supported and return 400. Check the [compatibility page](../compatibility/) before relying on a feature: it lists known differences, including options that osmem accepts but handles differently.

## The model: one base, many clones

Every test suite follows the same shape, whatever the language:

1. Build a **base cluster** once: create indices, put mappings, load seed documents.
2. For each test, take a **clone** of the base. Cloning copies nothing; an index is duplicated only when one side writes to it, so a clone costs microseconds and a test that writes pays once for the indices it touches.
3. Run the test against the clone and throw it away.

Once a clone has been taken, the base is **frozen**: HTTP writes to it return 403 `osmem_base_frozen`. That protects the fixture from a test that forgot to clone. Read-only tests can query the base directly.

## Two ways to run it

| Your tests are in | Use | How the server runs |
|---|---|---|
| Go | package `osmem` with the `osmemtest` helpers | inside the test process, no network needed |
| Node.js, Python, Java, anything else | `osmem-server` through a language package | a child process started by the test session, speaking HTTP on a loopback port |

Both forms expose the same REST API and the same seed format. Go can call the cluster in-process or expose an HTTP handler; the other language packages start a small child process and let the official OpenSearch client use its loopback URL.

## Package versioning

Future osmem releases are planned to use `1.<OpenSearch-major>.<osmem-release>`. The leading `1` is fixed, the second segment follows the supported OpenSearch major (currently `9`), and the last segment is osmem's own release number; the planned series is therefore `1.9.y`. This is the osmem package version, not the OpenSearch server or client version. Use a release version available from the package registry; the Java examples currently show the earlier `0.1.0` coordinates.

## Install

```bash
# Go
go get github.com/shibukawa/osmem github.com/opensearch-project/opensearch-go/v4

# Node.js (the binary for your platform arrives as an optional dependency)
npm install --save-dev @osmem/core @opensearch-project/opensearch

# Python (the wheel bundles the binary)
pip install osmem-server opensearch-py pytest

# Java (Maven; also add opensearch-java and its HTTP transport)
#   io.github.shibukawa.osmem:osmem:0.1.0
#   io.github.shibukawa.osmem:osmem-server-binaries:0.1.0:linux-amd64
#   org.opensearch.client:opensearch-java
```

## Where next

- [Go guide](../go/), [Node.js guide](../node/), [Python guide](../python/), [Java guide](../java/)
- [Seed data](../seed-data/): how to describe the base cluster as files
- [Management API](../management-api/): the `/_osmem` endpoints behind the language packages
- [Compatibility](../compatibility/): what works, what is approximated, what returns an error
- [Performance](../performance/): startup, query, and artifact-size measurements with their limits
