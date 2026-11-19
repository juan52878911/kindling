// API de alto nivel: Sandbox, Files.

import { Client } from "./client.js";
import { NotFound } from "./errors.js";

const asyncDispose = Symbol.asyncDispose ?? Symbol.for("Symbol.asyncDispose");
const INSPECT = Symbol.for("nodejs.util.inspect.custom");

const b64 = (s) => (s ? new Uint8Array(Buffer.from(s, "base64")) : new Uint8Array(0));
const utf8 = (u8) => new TextDecoder().decode(u8);

function resultado(j) {
  j = j ?? {};
  const stdoutBytes = b64(j.stdout), stderrBytes = b64(j.stderr);
  return {
    exit_code: j.exit_code ?? 0, stdout: utf8(stdoutBytes), stderr: utf8(stderrBytes),
    stdoutBytes, stderrBytes, duration_ms: j.duration_ms ?? 0,
    truncated: j.truncated ?? false, timed_out: j.timed_out ?? false,
  };
}

function evento(j) {
  const data = b64(j.data);
  return {
    stream: j.stream ?? null, data, text: utf8(data), exit: j.exit ?? null,
    duration_ms: j.duration_ms ?? 0, truncated: j.truncated ?? false,
    timed_out: j.timed_out ?? false, error: j.error ?? null,
  };
}

function peticionExec(cmd, { cwd, env, timeout, stdin, user, maxOutputBytes } = {}) {
  const p = { cmd: typeof cmd === "string" ? ["sh", "-c", cmd] : [...cmd] }; // un string es una línea de shell
  if (cwd) p.dir = cwd;
  if (env) p.env = Array.isArray(env) ? [...env] : Object.entries(env).map(([k, v]) => `${k}=${v}`);
  if (stdin != null) p.stdin = Buffer.from(typeof stdin === "string" ? stdin : stdin).toString("base64");
  if (timeout) p.timeout_seconds = Math.trunc(timeout);
  if (maxOutputBytes) p.max_output_bytes = Math.trunc(maxOutputBytes);
  if (user) p.user = user;
  return p;
}

/** Ficheros de un sandbox: `await sb.files.read("/etc/hostname")`. */
export class Files {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  read(path) { return this.#c.readFile(this.#id, path); }
  async readText(path) { return utf8(await this.read(path)); }
  /** `mode` es un entero (0o644); `mkdir: true` crea los directorios que falten. */
  write(path, data, { mode, mkdir = false } = {}) {
    return this.#c.writeFile(this.#id, path, typeof data === "string" ? new TextEncoder().encode(data) : data, { mode, mkdir });
  }
  delete(path) { return this.#c.deleteFile(this.#id, path); }
  stat(path) { return this.#c.statFile(this.#id, path); }
}

export class Sandbox {
  #client;
  constructor(info, client) {
    this.info = info;
    this.#client = client;
    this.files = new Files(client, info.id);
  }
  get id() { return this.info.id; }
  toString() { return `Sandbox(id=${JSON.stringify(this.info.id)}, state=${JSON.stringify(this.info.state)})`; }
  [INSPECT]() { return this.toString(); }
  toJSON() { return this.info; }

  static #cliente({ client, url, token, timeout } = {}) {
    return client ?? new Client({ url, token, ...(timeout !== undefined && { timeout }) });
  }

  /** `ttl` en segundos. */
  /** `volumes`: [{ name, mount, read_only, clone_from }] de volúmenes del inquilino (Client.createVolume). */
  static async create({ template, ttl, image, on_ttl, egress, allow_domains, mem_mib, vcpus, labels, volumes, ...conn } = {}) {
    const c = Sandbox.#cliente(conn);
    const info = await c.createSandbox({
      template, image, ttl_seconds: ttl, on_ttl, egress,
      allow_domains: allow_domains?.length ? [...allow_domains] : undefined, mem_mib, vcpus, labels,
      volumes: volumes?.length ? volumes.map((v) => ({ ...v })) : undefined,
    });
    return new Sandbox(info, c);
  }
  /**
   * Crea `count` sandboxes iguales (evaluaciones, RL). Array de Sandbox con `.errors`
   * (los que no salieron; solo sin `atomic`). Con `atomic: true` es todo o nada: si uno
   * falla se borran los demás y se lanza la excepción. Un lote solo monta volúmenes read_only.
   */
  static async createMany(count, { atomic = false, template, ttl, image, on_ttl, egress, allow_domains, mem_mib, vcpus, labels, volumes, ...conn } = {}) {
    const c = Sandbox.#cliente(conn);
    const { sandboxes, errors } = await c.createSandboxes(count, {
      atomic, template, image, ttl_seconds: ttl, on_ttl, egress,
      allow_domains: allow_domains?.length ? [...allow_domains] : undefined, mem_mib, vcpus, labels,
      volumes: volumes?.length ? volumes.map((v) => ({ ...v })) : undefined,
    });
    return Object.assign(sandboxes.map((i) => new Sandbox(i, c)), { errors });
  }
  static async connect(id, conn = {}) {
    const c = Sandbox.#cliente(conn);
    return new Sandbox(await c.getSandbox(id), c);
  }
  /** Array de Sandbox; `.partial` trae X-Kindling-Partial (hosts caídos) o null. */
  static async list(conn = {}) {
    const c = Sandbox.#cliente(conn);
    const { sandboxes, partial } = await c.listSandboxes();
    return Object.assign(sandboxes.map((i) => new Sandbox(i, c)), { partial });
  }

  async refresh() { this.info = await this.#client.getSandbox(this.id); return this; }
  /** Renueva el TTL (segundos; sin valor, el del sandbox). */
  async renew(ttl) { this.info = await this.#client.renewSandbox(this.id, ttl); return this.info; }
  delete() { return this.#client.deleteSandbox(this.id); }
  kill() { return this.delete(); }

  async [asyncDispose]() {
    try { await this.delete(); } catch (e) { if (!(e instanceof NotFound)) throw e; }
  }

  /** Espera al final. `cmd` string → `sh -c`; array, tal cual. `timeout` en segundos. */
  async exec(cmd, opts) {
    return resultado(await this.#client.execWait(this.id, peticionExec(cmd, opts)));
  }
  /** Iterador asíncrono de eventos; el último trae `exit` o `error`. Los errores previos al flujo saltan al hacer await. */
  async *execStream(cmd, opts) {
    const it = await this.#client.execStream(this.id, peticionExec(cmd, opts));
    for await (const j of it) yield evento(j);
  }
  exec_stream(cmd, opts) { return this.execStream(cmd, opts); }
}
