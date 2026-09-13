---
title: "Node.js guide"
description: "Use @osmem/core with Vitest, Jest or node:test to give every test its own OpenSearch clone."
---

`@osmem/core` starts `osmem-server` as a child process, waits for it to report its address, and gives each test a clone URL. The binary comes from a platform package (`@osmem/darwin-arm64`, `@osmem/linux-x64`, ...) that npm selects through optional dependencies, so there is nothing to download or configure. This page covers Vitest and Jest, plain `node:test`, and the options.

## Install

```bash
npm install --save-dev @osmem/core
```

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
