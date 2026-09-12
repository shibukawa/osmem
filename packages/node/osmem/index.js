import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { existsSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";

const require = createRequire(import.meta.url);

/** Resolve the osmem-server binary: option, env, then the platform package. */
export function resolveBinary(binary) {
  if (binary) return binary;
  if (process.env.OSMEM_SERVER_BIN) return process.env.OSMEM_SERVER_BIN;
  const pkg = `osmem-server-${process.platform}-${process.arch}`;
  let pkgJson;
  try {
    pkgJson = require.resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `osmem: no server binary for ${process.platform}-${process.arch}: install ${pkg} or set OSMEM_SERVER_BIN`,
    );
  }
  const bin = join(pkgJson, "..", "bin", process.platform === "win32" ? "osmem-server.exe" : "osmem-server");
  if (!existsSync(bin)) throw new Error(`osmem: binary missing at ${bin}`);
  return bin;
}

async function requestJSON(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data;
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { raw: text };
  }
  if (!res.ok) {
    const err = data?.error;
    const detail = err && typeof err === "object" ? `${err.type}: ${err.reason}` : (err ?? text);
    throw new Error(`osmem: ${method} ${url}: ${res.status} ${detail}`);
  }
  return data;
}

/** A clone of the base cluster, served on its own port. */
export class OsmemClone {
  #server;
  constructor(server, id, url) {
    this.#server = server;
    this.id = id;
    this.url = url;
  }
  /** Close the clone and free its port. */
  async close() {
    await requestJSON("DELETE", `${this.#server.url}/_osmem/clones/${this.id}`).catch(() => {});
  }
}

/**
 * A running osmem-server process hosting a base cluster.
 *
 *   const server = await OsmemServer.start({ seed: ["./testdata/seed"] });
 *   const clone = await server.clone();
 *   ... use clone.url with any OpenSearch client ...
 *   await clone.close();
 *   await server.close();
 */
export class OsmemServer {
  #child;
  #exited;
  constructor(child, ready) {
    this.#child = child;
    this.url = ready.url;
    this.pid = ready.pid;
    this.version = ready.version;
    this.japanese = ready.japanese;
    this.indices = ready.indices;
    this.#exited = new Promise((resolve) => child.once("exit", resolve));
  }

  /**
   * Start a server.
   * @param {object} [options]
   * @param {string|string[]} [options.seed] seed directories or .ndjson files
   * @param {boolean} [options.freeze] freeze the base after seeding
   * @param {boolean} [options.japanese=true] enable kuromoji (kagome)
   * @param {string} [options.addr] listen address, default 127.0.0.1:0
   * @param {string} [options.binary] path to osmem-server
   * @param {number} [options.startupTimeoutMs=30000]
   * @param {boolean} [options.inheritStderr=true]
   */
  static async start(options = {}) {
    const bin = resolveBinary(options.binary);
    const args = ["--parent-pid", String(process.pid)];
    for (const s of [].concat(options.seed ?? [])) args.push("--seed", s);
    if (options.freeze) args.push("--freeze");
    if (options.japanese === false) args.push("--no-ja");
    if (options.addr) args.push("--addr", options.addr);
    const child = spawn(bin, args, {
      stdio: ["pipe", "pipe", options.inheritStderr === false ? "ignore" : "inherit"],
      windowsHide: true,
    });
    const ready = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        child.kill();
        reject(new Error(`osmem: server did not start within ${options.startupTimeoutMs ?? 30000} ms`));
      }, options.startupTimeoutMs ?? 30000);
      const rl = createInterface({ input: child.stdout });
      rl.on("line", (line) => {
        try {
          const msg = JSON.parse(line);
          if (msg.url) {
            clearTimeout(timer);
            rl.close();
            resolve(msg);
          }
        } catch {
          // not the ready line
        }
      });
      child.once("error", (err) => {
        clearTimeout(timer);
        reject(new Error(`osmem: failed to start ${bin}: ${err.message}`));
      });
      child.once("exit", (code) => {
        clearTimeout(timer);
        reject(new Error(`osmem: server exited during startup with code ${code}`));
      });
    });
    return new OsmemServer(child, ready);
  }

  /** Create a clone served on its own port; freezes the base. */
  async clone() {
    const res = await requestJSON("POST", `${this.url}/_osmem/clones`);
    return new OsmemClone(this, res.id, res.url);
  }

  /** Run fn with a fresh clone and close it afterwards. */
  async withClone(fn) {
    const clone = await this.clone();
    try {
      return await fn(clone);
    } finally {
      await clone.close();
    }
  }

  /** Freeze the base: writes to it fail with 403 until a clone is used. */
  async freeze() {
    await requestJSON("POST", `${this.url}/_osmem/base/freeze`);
  }

  /** Any request against the base, decoded as JSON. Throws on error status. */
  request(method, path, body) {
    return requestJSON(method, `${this.url}${path}`, body);
  }

  /** Stop the process (closes stdin, then kills it after a grace period). */
  async close() {
    if (this.#child.exitCode !== null) return;
    this.#child.stdin.end();
    const killer = setTimeout(() => this.#child.kill("SIGKILL"), 5000);
    await this.#exited;
    clearTimeout(killer);
  }

  [Symbol.asyncDispose]() {
    return this.close();
  }
}

export default OsmemServer;
