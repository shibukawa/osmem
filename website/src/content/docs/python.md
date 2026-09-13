---
title: "Python guide"
description: "Use the osmem-server package and its pytest fixtures with opensearch-py."
---

`osmem-server` on PyPI bundles the server binary for your platform and registers pytest fixtures automatically. The import name is `osmem_server`. This page shows the pytest configuration, how to use the fixtures with opensearch-py, and how to drive the server without pytest.

## Install

```bash
pip install osmem-server opensearch-py pytest
```

`osmem-server` bundles the local server; `opensearch-py` is the official client your application already uses. pytest is only needed for the fixture integration.

## Start, register the schema, and query

Outside pytest, start the server as a context manager. Setup requests use the ordinary REST API; the official Python client sends the search:

```python
from opensearchpy import OpenSearch
from osmem_server import OsmemServer

with OsmemServer.start(japanese=False) as server:
    server.request("PUT", "/products", {
        "mappings": {"properties": {"name": {
            "type": "text", "fields": {"keyword": {"type": "keyword"}}
        }}}
    })
    server.request("PUT", "/products/_doc/1", {"name": "Red Apple"})

    client = OpenSearch(hosts=[server.url])
    result = client.search(index="products", body={"query": {"match": {"name": "apple"}}})
    print(result["hits"]["hits"])
```

For fixtures shared with Go, Node.js, and Java, pass the same seed directory to `OsmemServer.start`; see the [seed format](../seed-data/).

## pytest

Point the plugin at your seed data once, in `pytest.ini` or `pyproject.toml`:

```ini
[pytest]
osmem_seed = testdata/seed
```

```toml
[tool.pytest.ini_options]
osmem_seed = ["testdata/seed"]
osmem_freeze = true       # default
osmem_japanese = true     # default
```

Then ask for a clone in any test:

```python
from opensearchpy import OpenSearch

def test_adds_a_product(osmem_clone):
    client = OpenSearch(hosts=[osmem_clone.url])
    client.index(index="products", id="x", body={"name": "new"})
    assert client.get(index="products", id="x")["found"]
```

Three fixtures exist:

- `osmem_server` (session scope): the running server; `osmem_server.url` is the base, frozen once a clone exists.
- `osmem_clone` (function scope): a fresh clone per test, deleted afterwards.
- `osmem_url`: the clone's URL as a string, for tests that only need the address.

Use `osmem_clone` when a test can change index state—for example, by indexing or deleting documents, changing mappings, or creating and deleting indices. Tests that only query can use the base URL directly.

To start the server with other options, override `osmem_server` in `conftest.py`; the other fixtures keep working:

```python
import pytest
from osmem_server import OsmemServer

@pytest.fixture(scope="session")
def osmem_server():
    with OsmemServer.start(seed=["fixtures/catalog"], japanese=False) as server:
        yield server
```

## Without pytest

`OsmemServer` and clones are context managers:

```python
from osmem_server import OsmemServer

with OsmemServer.start(seed=["testdata/seed"]) as server, server.clone() as clone:
    print(clone.url)
```

`OsmemServer.start(seed=..., freeze=..., japanese=..., addr=..., binary=..., startup_timeout=...)` mirrors the command line. `server.request(method, path, body)` sends a JSON request to the base and raises `OsmemError` with OpenSearch's error type and reason on failure.

`OSMEM_SERVER_BIN` overrides the bundled binary, for example to test against a locally built server.

## Choose the test lifetime

- **Fresh server per test:** a function-scoped fixture can start and close `OsmemServer` for every test. This is easy to reason about but repeats startup and seeding.
- **One server for the session or class:** the built-in `osmem_server` fixture has session scope. For class scope, define a fixture with `scope="class"`; read-only tests can share its base URL.
- **A test with side effects:** keep `osmem_server` at session scope and request `osmem_clone` (function scope) when a test changes documents, mappings, or index state. Each clone is a fork of the frozen, seeded base; pytest closes it after the test, so those changes cannot leak into the next case.

Avoid using the base URL for writes after a clone has been created. The base is frozen to catch that mistake.

If one test needs a fully independent server, a function-scoped fixture can own it:

```python
import pytest
from osmem_server import OsmemServer

@pytest.fixture
def isolated_osmem():
    with OsmemServer.start(seed=["testdata/seed"]) as server:
        yield server
```

Use `isolated_osmem` for that test; keep the built-in session server plus `osmem_clone` for the usual faster pattern.
