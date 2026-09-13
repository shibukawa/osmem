---
title: "Pythonガイド"
description: "osmem-serverパッケージとそのpytest fixtureを、opensearch-pyと組み合わせて使う。"
---

PyPIの`osmem-server`は、プラットフォームに合ったサーバーバイナリを同梱し、pytestのfixtureを自動で登録します。import名は`osmem_server`です。このページでは、pytestの設定、opensearch-pyでのfixtureの使い方、そしてpytestを使わずにサーバーを動かす方法を説明します。

## インストール

```bash
pip install osmem-server opensearch-py pytest
```

`osmem-server`はローカルサーバーを同梱し、`opensearch-py`はアプリでも使う公式クライアントです。pytest連携を使う場合に`pytest`も追加します。

## 起動し、schemaを登録して検索する

pytestの外ではcontext managerとしてサーバーを起動します。準備には通常のREST APIを使い、検索は公式Pythonクライアントから送ります。

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

Go、Node.js、Javaと共通のfixtureにする場合は、同じseed directoryを`OsmemServer.start`に渡します。[seed形式](../seed-data/)を参照してください。

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

## テストのライフタイムを選ぶ

- **テストごとに新しいserver:** function-scopeのfixtureで毎回`OsmemServer`を起動・終了します。理解しやすい反面、起動とseedを繰り返します。
- **sessionまたはclassでserverを共有:** 標準の`osmem_server` fixtureはsession scopeです。class scopeにしたい場合は`scope="class"`でfixtureを定義します。読み取り専用テストならbase URLを共有できます。
- **書き込むテスト:** `osmem_server`はsession scopeのまま、`osmem_clone`(function scope)を使います。各cloneは凍結済みのseed baseから作るforkです。pytestがテスト後に閉じるため、documentやmappingの変更は次のテストへ漏れません。

cloneを作った後にbase URLへ書き込まないでください。誤操作を検出するため、baseは凍結されます。

テストごとに独立したserverが必要なら、function-scopeのfixtureにライフタイムを任せます。

```python
import pytest
from osmem_server import OsmemServer

@pytest.fixture
def isolated_osmem():
    with OsmemServer.start(seed=["testdata/seed"]) as server:
        yield server
```

そのテストでは`isolated_osmem`を使います。通常は、session serverと`osmem_clone`を組み合わせるほうが起動を繰り返さずに済みます。
