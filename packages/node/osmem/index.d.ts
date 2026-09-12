export interface StartOptions {
  /** Seed directories or .ndjson files, loaded in order. */
  seed?: string | string[];
  /** Freeze the base after seeding; writes must go through clones. */
  freeze?: boolean;
  /** Enable Japanese analysis (kuromoji via kagome). Default true. */
  japanese?: boolean;
  /** Listen address, default 127.0.0.1:0. */
  addr?: string;
  /** Path to the osmem-server binary (default: platform package or OSMEM_SERVER_BIN). */
  binary?: string;
  startupTimeoutMs?: number;
  inheritStderr?: boolean;
}

export declare class OsmemClone {
  readonly id: string;
  /** Base URL to give to an OpenSearch client. */
  readonly url: string;
  close(): Promise<void>;
}

export declare class OsmemServer {
  readonly url: string;
  readonly pid: number;
  readonly version: string;
  readonly japanese: boolean;
  readonly indices: string[];
  static start(options?: StartOptions): Promise<OsmemServer>;
  clone(): Promise<OsmemClone>;
  withClone<T>(fn: (clone: OsmemClone) => Promise<T> | T): Promise<T>;
  freeze(): Promise<void>;
  request<T = any>(method: string, path: string, body?: unknown): Promise<T>;
  close(): Promise<void>;
  [Symbol.asyncDispose](): Promise<void>;
}

export declare function resolveBinary(binary?: string): string;
export default OsmemServer;
