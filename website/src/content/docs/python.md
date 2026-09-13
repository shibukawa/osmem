---
title: "Python guide"
description: "Use the osmem-server package and its pytest fixtures with opensearch-py."
---

`osmem-server` on PyPI bundles the server binary for your platform and registers pytest fixtures automatically. The import name is `osmem_server`. This page shows the pytest configuration, how to use the fixtures with opensearch-py, and how to drive the server without pytest.

## Install

```bash
pip install osmem-server
```

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
