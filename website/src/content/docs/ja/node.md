---
title: "Node.jsガイド"
description: "Vitest、Jest、node:testで@osmem/coreを使い、テストごとにOpenSearchのクローンを用意する。"
---

`@osmem/core`は`osmem-server`を子プロセスとして起動し、アドレスが報告されるとテストにクローンのURLを渡します。npmがoptional dependenciesから対応するplatform package(`@osmem/darwin-arm64`、`@osmem/linux-x64`など)もinstallするため、binaryを別途設定する必要はありません。このページでは、VitestとJest、素の`node:test`、そしてオプションを扱います。

## インストール

```bash
npm install --save-dev @osmem/core @opensearch-project/opensearch
```

`@osmem/core`はローカルサーバーとテスト用helperを提供し、`@opensearch-project/opensearch`はアプリ側と同じ公式クライアントです。npmが現在のOSとCPUに合うoptional binary packageを選びます。

## 起動し、schemaを登録して検索する

launcherは子プロセスを1つ起動します。`request`でindexを作ってdocumentを登録し、同じserver URLを公式クライアントに渡します。

```js
import { OsmemServer } from "@osmem/core";
import { Client } from "@opensearch-project/opensearch";

const server = await OsmemServer.start({ japanese: false });
try {
  await server.request("PUT", "/products", {
    mappings: { properties: { name: { type: "text", fields: { keyword: { type: "keyword" } } } } },
  });
  await server.request("PUT", "/products/_doc/1", { name: "Red Apple" });

  const client = new Client({ node: server.url });
  const result = await client.search({ index: "products", body: { query: { match: { name: "apple" } } } });
  console.log(result.body.hits.hits);
} finally {
  await server.close();
}
```

fixtureが大きくなったら`start`にseed directoryを渡します。[共通seed形式](../seed-data/)ならmappingやdocumentをレビューしやすく、言語間でも共有できます。

## ファイルごとに1サーバー、テストごとに1クローン

`beforeAll`でサーバーを起動し、`afterAll`で閉じます。`withClone`はクローンを作り、それを渡して関数を実行し、終わったらクローンを削除します。

```js
import { OsmemServer } from "@osmem/core";
import { Client } from "@opensearch-project/opensearch";

let server;
beforeAll(async () => {
  server = await OsmemServer.start({ seed: ["./testdata/seed"], freeze: true });
});
afterAll(() => server.close());

test("adds a product", async () => {
  await server.withClone(async (clone) => {
    const client = new Client({ node: clone.url });
    await client.index({ index: "products", id: "x", body: { name: "new" } });
    const res = await client.get({ index: "products", id: "x" });
    expect(res.body.found).toBe(true);
  });
});
```

同じコードがJestでもVitestでも動きます。`node:test`では、`beforeAll`/`afterAll`の代わりに`node:test`の`before`/`after`を使います。

自分で閉じたい場合は、`server.clone()`がクローンのオブジェクトを返します。`clone.url`が、どのOpenSearchクライアントにも渡せるアドレスです。読み取りだけのテストは`server.url`を直接使えます。

## オプション

`OsmemServer.start(options)`が受け付けるもの:

| オプション | 意味 |
|---|---|
| `seed` | シードディレクトリか`.ndjson`ファイル、またはその配列。指定順に読み込む([形式](../seed-data/)) |
| `freeze` | ベースへの書き込みを即座に拒否する(指定しなければ最初のクローンで凍結される) |
| `japanese` | `false`でkuromojiを無効にする。デフォルトは`true` |
| `addr` | listenするアドレス。デフォルトは`127.0.0.1:0` |
| `binary` | `osmem-server`のパス。プラットフォームパッケージより優先される |
| `startupTimeoutMs` | デフォルトは30000 |
| `inheritStderr` | `false`でサーバーの標準エラーを表示しない |

`server.request(method, path, body)`は任意のJSONリクエストをベースに送り、エラーステータスなら例外を投げます。`/_osmem`や`_count`に対するアサーションに便利です。

環境変数`OSMEM_SERVER_BIN`は、プラットフォームパッケージより優先されます。ローカルでビルドしたサーバーで試すときに使います。

## CommonJS

`require("@osmem/core")`は、同じ`start`メソッドを持つ`{ OsmemServer }`を返します。実装はESモジュールとして遅延ロードされるので、`start`はいつもどおり`await`してください。

## プロセスのライフタイム

子プロセスには`--parent-pid`と、パイプでつないだ標準入力が渡されます。終了するのは、テストプロセスが終わって標準入力が閉じたとき、親のpidが消えたとき、あるいは`server.close()`が呼ばれたときです。`close()`は標準入力を閉じ、5秒経っても終わらなければプロセスをkillします。テストランナーがクラッシュしても、サーバーは残りません。

## テストのライフタイムを選ぶ

- **テストごとに新しいserver:** 各テスト内で`OsmemServer.start({ seed })`を呼び、`finally`で閉じます。境界は単純ですが、起動とseedを繰り返します。
- **fileまたはworkerごとに1つのserver:** 上の例のように`beforeAll`で起動し、`afterAll`で閉じます。読み取り専用テストは`server.url`を直接使えます。
- **書き込みをするテスト:** `server.withClone(...)`か`server.clone()`でseed済みbaseをforkします。cloneは隔離され、閉じればそのテストのwriteは破棄されます。`withClone`ならassertionが失敗しても後始末されます。

書き込み可能なcloneをテスト間で共有しないでください。並列テストでは、それぞれ専用cloneを作ります。
