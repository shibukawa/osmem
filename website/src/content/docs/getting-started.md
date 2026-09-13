---
title: "Getting started"
description: "What osmem is, the base-and-clone model, and how to install it for Go, Node.js, Python and Java."
---

Tests that touch OpenSearch usually pay twice: once to start a container, and again on every test that has to wait for a refresh before its documents become searchable. osmem removes both costs. It is an OpenSearch-compatible server written in Go that keeps every index in memory, starts in milliseconds, and makes writes visible to the next search immediately. After this page you will know which of its two forms fits your test suite and how to install it.

## What it is, and what it is not

osmem speaks the OpenSearch 2.x REST API: index and document CRUD, bulk, the query DSL, aggregations, scroll, aliases, templates. Existing clients work unchanged. Under the hood it is not OpenSearch. bleve, a pure Go search library, provides the inverted index and BM25 scoring; everything else (mapping semantics, sorting, aggregations, highlighting) is reimplemented in Go against the stored documents.

That design sets the limits. Rankings agree with OpenSearch for ordinary queries, but the numeric `_score` values differ, so a test must not assert on them. Painless scripts are not supported and return 400. Nested documents are flattened. The [compatibility page](../compatibility/) lists every known difference; the rule throughout is that an unsupported feature fails loudly rather than returning a plausible but wrong answer.

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

Both forms expose the same REST API and the same seed format. The Go form is faster and needs no binary; the subprocess form works everywhere an HTTP client works.

## Install

```bash
# Go
go get github.com/shibukawa/osmem

# Node.js (the binary for your platform arrives as an optional dependency)
npm install --save-dev @osmem/core

# Python (the wheel bundles the binary)
pip install osmem-server

# Java (Maven; add the binaries jar for the platform your tests run on)
#   io.github.shibukawa.osmem:osmem:0.1.0
#   io.github.shibukawa.osmem:osmem-server-binaries:0.1.0:linux-amd64
```

## Where next

- [Go guide](../go/), [Node.js guide](../node/), [Python guide](../python/), [Java guide](../java/)
- [Seed data](../seed-data/): how to describe the base cluster as files
- [Management API](../management-api/): the `/_osmem` endpoints behind the language packages
- [Compatibility](../compatibility/): what works, what is approximated, what returns an error
