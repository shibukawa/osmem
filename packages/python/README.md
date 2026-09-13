# os-mem

In-memory OpenSearch-compatible server for tests (`pip install os-mem`).
The wheel bundles the `osmem-server` binary for your platform; no Docker,
no JVM. The import name is `os_mem`.

```python
# pytest.ini / pyproject.toml
[tool.pytest.ini_options]
osmem_seed = ["testdata/seed"]
```

```python
from opensearchpy import OpenSearch

def test_search(osmem_clone):
    client = OpenSearch(hosts=[osmem_clone.url])
    client.index(index="products", id="x", body={"name": "new"}, refresh=True)
    assert client.count(index="products")["count"] == 6
```

Each test gets its own clone of the seeded base cluster (milliseconds,
copy-on-write); the server itself starts once per session and exits with
the test process. Without pytest:

```python
from os_mem import OsmemServer

with OsmemServer.start(seed=["testdata/seed"]) as server, server.clone() as clone:
    ...
```

Set `OSMEM_SERVER_BIN` to use a locally built binary. Seed layout and the
management API are documented in the
[osmem repository](https://github.com/shibukawa/osmem).
