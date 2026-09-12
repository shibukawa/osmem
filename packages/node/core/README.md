# @osmem/core

In-memory OpenSearch-compatible server for tests. This package launches
the `osmem-server` binary (shipped in the `@osmem/<platform>`
packages, pulled in as optional dependencies) and exposes the clone
lifecycle, so every test gets an isolated copy of a seeded cluster in
milliseconds without Docker.

```js
import { OsmemServer } from "@osmem/core";
import { Client } from "@opensearch-project/opensearch";

let server;
beforeAll(async () => {
  server = await OsmemServer.start({ seed: ["./testdata/seed"], freeze: true });
});
afterAll(() => server.close());

test("search", async () => {
  await server.withClone(async (clone) => {
    const client = new Client({ node: clone.url });
    await client.index({ index: "products", id: "x", body: { name: "new" } });
    const res = await client.search({ index: "products", body: { query: { match_all: {} } } });
    expect(res.body.hits.total.value).toBe(6);
  });
});
```

Read-only tests can use `server.url` directly. The server exits when the
test process exits (it watches its stdin and the parent pid).

Seed directory layout and the REST/management API are documented in the
[osmem repository](https://github.com/shibukawa/osmem). Set
`OSMEM_SERVER_BIN` to use a locally built binary.
