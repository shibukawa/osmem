import json
import urllib.request

import pytest

from osmem_server import OsmemError


def get_json(url):
    with urllib.request.urlopen(url) as r:
        return json.loads(r.read())


def test_server_ready(osmem_server):
    assert osmem_server.url.startswith("http://127.0.0.1:")
    assert osmem_server.indices == ["other", "products"]
    assert osmem_server.japanese


def test_base_is_frozen(osmem_server):
    with pytest.raises(OsmemError, match="osmem_base_frozen"):
        osmem_server.request("PUT", "/products/_doc/9", {"name": "x"})


def test_clone_is_isolated(osmem_clone, osmem_server):
    req = urllib.request.Request(
        f"{osmem_clone.url}/products/_doc/9", data=b'{"name": "clone"}', method="PUT", headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req) as r:
        assert r.status == 201
    assert get_json(f"{osmem_clone.url}/products/_count")["count"] == 3
    assert get_json(f"{osmem_server.url}/products/_count")["count"] == 2


def test_clone_again_starts_fresh(osmem_clone):
    assert get_json(f"{osmem_clone.url}/products/_count")["count"] == 2


def test_japanese(osmem_url):
    req = urllib.request.Request(
        f"{osmem_url}/products/_search",
        data=json.dumps({"query": {"match": {"name": "スカイツリー"}}}).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req) as r:
        assert json.loads(r.read())["hits"]["total"]["value"] == 1
