---
title: "Pythonガイド"
description: "osmem-serverパッケージとそのpytest fixtureを、opensearch-pyと組み合わせて使う。"
---

PyPIの`osmem-server`は、プラットフォームに合ったサーバーバイナリを同梱し、pytestのfixtureを自動で登録します。import名は`osmem_server`です。このページでは、pytestの設定、opensearch-pyでのfixtureの使い方、そしてpytestを使わずにサーバーを動かす方法を説明します。

## インストール

```bash
pip install osmem-server
```

## pytest

シードデータの場所を、`pytest.ini`か`pyproject.toml`で一度だけ指定します。

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

あとは、任意のテストでクローンを受け取ります。

```python
from opensearchpy import OpenSearch

def test_adds_a_product(osmem_clone):
    client = OpenSearch(hosts=[osmem_clone.url])
    client.index(index="products", id="x", body={"name": "new"})
    assert client.get(index="products", id="x")["found"]
```

fixtureは3つあります。

- `osmem_server`(sessionスコープ): 起動済みのサーバー。`osmem_server.url`がベースで、クローンが作られた時点で凍結されます。
- `osmem_clone`(functionスコープ): テストごとの新しいクローン。テストの後に削除されます。
- `osmem_url`: クローンのURLの文字列。アドレスだけが必要なテスト向けです。

別のオプションでサーバーを起動したいときは、`conftest.py`で`osmem_server`を上書きします。他のfixtureはそのまま動きます。

```python
import pytest
from osmem_server import OsmemServer

@pytest.fixture(scope="session")
def osmem_server():
    with OsmemServer.start(seed=["fixtures/catalog"], japanese=False) as server:
        yield server
```

## pytestを使わない場合

`OsmemServer`とクローンは、コンテキストマネージャです。

```python
from osmem_server import OsmemServer

with OsmemServer.start(seed=["testdata/seed"]) as server, server.clone() as clone:
    print(clone.url)
```

`OsmemServer.start(seed=..., freeze=..., japanese=..., addr=..., binary=..., startup_timeout=...)`は、コマンドラインの引数に対応しています。`server.request(method, path, body)`はベースにJSONリクエストを送り、失敗するとOpenSearchのエラー種別と理由を含む`OsmemError`を送出します。

`OSMEM_SERVER_BIN`は同梱バイナリより優先されます。たとえば、ローカルでビルドしたサーバーに対してテストするときに使います。
