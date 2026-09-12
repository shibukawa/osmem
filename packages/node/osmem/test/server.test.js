import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { OsmemServer } from "../index.js";

const here = dirname(fileURLToPath(import.meta.url));
const seed = join(here, "..", "..", "..", "..", "internal", "serve", "testdata", "seed");

let server;
before(async () => {
  server = await OsmemServer.start({ seed, freeze: true });
});
after(async () => {
  await server.close();
});

test("ready information", () => {
  assert.match(server.url, /^http:\/\/127\.0\.0\.1:\d+$/);
  assert.deepEqual(server.indices, ["other", "products"]);
});

test("base is frozen, clones are writable and isolated", async () => {
  await assert.rejects(server.request("PUT", "/products/_doc/9", { name: "x" }), /osmem_base_frozen/);
  const a = await server.clone();
  const b = await server.clone();
  try {
    const res = await fetch(`${a.url}/products/_doc/9`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "only in a" }),
    });
    assert.equal(res.status, 201);
    const countA = await (await fetch(`${a.url}/products/_count`)).json();
    const countB = await (await fetch(`${b.url}/products/_count`)).json();
    assert.equal(countA.count, 3);
    assert.equal(countB.count, 2);
  } finally {
    await a.close();
    await b.close();
  }
  const list = await server.request("GET", "/_osmem/clones");
  assert.equal(list.clones.length, 0);
});

test("withClone closes the clone", async () => {
  let url;
  await server.withClone(async (clone) => {
    url = clone.url;
    const res = await server.request("GET", "/_osmem/clones");
    assert.equal(res.clones.length, 1);
  });
  await assert.rejects(fetch(`${url}/`));
});

test("japanese analysis is available", async () => {
  const res = await server.request("POST", "/products/_search", { query: { match: { name: "スカイツリー" } } });
  assert.equal(res.hits.total.value, 1);
});
