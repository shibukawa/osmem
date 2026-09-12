"use strict";
// CommonJS entry: loads the ESM implementation lazily.
let mod;
async function load() {
  if (!mod) mod = await import("./index.js");
  return mod;
}
class OsmemServer {
  static async start(options) {
    const { OsmemServer: Impl } = await load();
    return Impl.start(options);
  }
}
module.exports = {
  OsmemServer,
  resolveBinary: (binary) => load().then((m) => m.resolveBinary(binary)),
};
module.exports.default = OsmemServer;
