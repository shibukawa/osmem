---
title: "Node.js guide"
description: "Use @osmem/core with Vitest, Jest or node:test to give every test its own OpenSearch clone."
---

`@osmem/core` starts `osmem-server` as a child process, waits for its address, and gives tests a clone URL. npm installs the matching platform package (`@osmem/darwin-arm64`, `@osmem/linux-x64`, ... ) through optional dependencies, so no separate binary setup is needed. This page covers Vitest and Jest, plain `node:test`, and the options.

## Install

```bash
npm install --save-dev @osmem/core @opensearch-project/opensearch
```

`@osmem/core` supplies the local server and test helpers; `@opensearch-project/opensearch` is the official client used by the application. npm selects the matching optional binary package for the current OS and CPU architecture.

## Start, register the schema, and query

The launcher starts one child process. Use its `request` method to set up the index and seed a document, then point the official client at the same server:

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

For a larger fixture, pass a seed directory to `start`; the [shared seed format](../seed-data/) keeps mappings and documents reviewable and reusable across languages.

## One server per file, one clone per test

Start the server in `beforeAll` and close it in `afterAll`. `withClone` creates a clone, runs your function with it, and deletes the clone afterwards.

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

The same code works in Jest and Vitest. With `node:test`, use `before`/`after` from `node:test` instead of `beforeAll`/`afterAll`.

`server.clone()` returns the clone object if you prefer to close it yourself; `clone.url` is the address for any OpenSearch client. Tests that only read can use `server.url` directly.

## Options

`OsmemServer.start(options)` accepts:

| option | meaning |
|---|---|
| `seed` | seed directory or `.ndjson` file, or an array of them, loaded in order ([format](../seed-data/)) |
| `freeze` | reject writes to the base immediately (otherwise the first clone freezes it) |
| `japanese` | `false` disables kuromoji; default `true` |
| `addr` | listen address, default `127.0.0.1:0` |
| `binary` | path to `osmem-server`; overrides the platform package |
| `startupTimeoutMs` | default 30000 |
| `inheritStderr` | `false` silences the server's stderr |

`server.request(method, path, body)` sends any JSON request to the base and throws on an error status, which is handy for assertions against `/_osmem` or `_count`.

The environment variable `OSMEM_SERVER_BIN` takes precedence over the platform package, for running against a locally built server.

## CommonJS

`require("@osmem/core")` returns `{ OsmemServer }` with the same `start` method; the implementation loads lazily as an ES module, so `start` must be awaited as usual.

## Process lifetime

The child process receives `--parent-pid` and a piped stdin. It exits when the test process exits (stdin closes), when the parent pid disappears, or when `server.close()` runs, which closes stdin and kills the process after five seconds if it has not left. A crashed test runner therefore does not leave servers behind.

## Choose the test lifetime

- **Fresh server per test:** call `OsmemServer.start({ seed })` inside each test and close it in `finally`. This is the simplest boundary, but repeats process startup and seeding.
- **One server per file or worker:** start it in `beforeAll` and close it in `afterAll`, as above. Read-only tests may use `server.url` directly.
- **A test that writes:** fork the seeded base with `server.withClone(...)` or `server.clone()`. The clone is isolated, and closing it discards that test's writes. The `withClone` form guarantees cleanup even when an assertion throws.

Do not share one writable clone between tests. Parallel tests should each create their own clone.
