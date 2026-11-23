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

function peticionExec(cmd, { cwd, env, timeout, stdin, user, maxOutputBytes, session } = {}) {
  const p = { cmd: typeof cmd === "string" ? ["sh", "-c", cmd] : [...cmd] }; // un string es una línea de shell
  if (cwd) p.dir = cwd;
  if (env) p.env = Array.isArray(env) ? [...env] : Object.entries(env).map(([k, v]) => `${k}=${v}`);
  if (stdin != null) p.stdin = Buffer.from(typeof stdin === "string" ? stdin : stdin).toString("base64");
  if (timeout) p.timeout_seconds = Math.trunc(timeout);
  if (maxOutputBytes) p.max_output_bytes = Math.trunc(maxOutputBytes);
  if (user) p.user = user;
  if (session) p.session = session;
  return p;
}

function evProceso(j) {
  const data = b64(j.data);
  return {
    stream: j.stream ?? null, data, text: utf8(data), offset: j.offset ?? 0, dropped: j.dropped ?? 0,
    exit: j.exit ?? null, signal: j.signal ?? "", error: j.error ?? null,
  };
}

const lista = (env) => (Array.isArray(env) ? [...env] : Object.entries(env).map(([k, v]) => `${k}=${v}`));

/** Procesos de fondo de un sandbox: sobreviven a la conexión del cliente. */
export class Processes {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  /** Lanza y vuelve enseguida con el pid. `cmd` string → `sh -c`. */
  start(cmd, opts) { return this.#c.startProcess(this.#id, peticionExec(cmd, { ...opts, stdin: undefined })); }
  list() { return this.#c.listProcesses(this.#id); }
  /** Estado; con `wait` (segundos) espera a que termine, y si no sigue «running». */
  get(pid, { wait } = {}) { return this.#c.getProcess(this.#id, pid, wait); }
  /** Iterador asíncrono de eventos desde `offset`; con `follow` hasta que termine. El último lleva el offset de reanudación. */
  async *output(pid, { offset = 0, follow = false } = {}) {
    const it = await this.#c.processOutput(this.#id, pid, offset, follow);
    for await (const j of it) yield evProceso(j);
  }
  /** Señal al grupo del proceso (por defecto TERM). */
  signal(pid, signal) { return this.#c.signalProcess(this.#id, pid, signal); }
  kill(pid) { return this.signal(pid, "KILL"); }
  delete(pid) { return this.#c.deleteProcess(this.#id, pid); }
}

/** Sesiones de exec: cwd y entorno que persisten entre comandos. */
export class Sessions {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  create({ id, cwd, env, ttl } = {}) {
    const p = {};
    if (id) p.id = id;
    if (cwd) p.dir = cwd;
    if (env) p.env = lista(env);
    if (ttl) p.ttl_seconds = Math.trunc(ttl);
    return this.#c.createSession(this.#id, p);
  }
  list() { return this.#c.listSessions(this.#id); }
  get(sid) { return this.#c.getSession(this.#id, sid); }
  /** Como Sandbox.exec, dentro de la sesión `sid`. */
  async exec(sid, cmd, opts) {
    return resultado(await this.#c.execWait(this.#id, peticionExec(cmd, { ...opts, session: sid })));
  }
  delete(sid) { return this.#c.deleteSession(this.#id, sid); }
}

/** Ficheros de un sandbox: `await sb.files.read("/etc/hostname")`. */
export class Files {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  /** `range`: `[inicio, fin]` (inclusivos; `fin` null = hasta el final) o `"bytes=-n"`: solo ese trozo. */
  read(path, { range } = {}) { return this.#c.readFile(this.#id, path, range); }
  async readText(path, opts) { return utf8(await this.read(path, opts)); }
  /** Mueve o renombra. Un destino que existe da Conflict salvo `overwrite: true`. */
  move(from, to, { overwrite = false } = {}) { return this.#c.moveFile(this.#id, from, to, overwrite); }
  /** Crea el directorio y los que falten (`mkdir -p`). */
  mkdir(path) { return this.#c.mkdirFile(this.#id, path); }
  /** Cambia permisos. `mode` es un entero (0o755) o una cadena octal ("755"). */
  chmod(path, mode, { recursive = false } = {}) {
    return this.#c.chmodFile(this.#id, path, typeof mode === "number" ? mode.toString(8) : String(mode), recursive);
  }
  /** Sustituye `old` por `new` en el fichero. Sin `all`, `old` tiene que aparecer una vez (si no, Conflict). */
  replace(path, old, nuevo, { all = false } = {}) { return this.#c.replaceInFile(this.#id, path, old, nuevo, all); }
  /** Busca por NOMBRE (`*.log`) bajo `path`, recursivo; `truncated` si se cortó. */
  search(path, glob) { return this.#c.searchFiles(this.#id, path, glob); }
  /** Extrae un tar dentro del directorio `path` del sandbox. */
  uploadTar(path, data, { mkdir = false } = {}) { return this.#c.uploadTar(this.#id, path, data, { mkdir }); }
  /** Baja el directorio `path` como tar. */
  downloadTar(path) { return this.#c.downloadTar(this.#id, path); }
  /** `mode` es un entero (0o644); `mkdir: true` crea los directorios que falten. */
  write(path, data, { mode, mkdir = false } = {}) {
    return this.#c.writeFile(this.#id, path, typeof data === "string" ? new TextEncoder().encode(data) : data, { mode, mkdir });
  }
  delete(path) { return this.#c.deleteFile(this.#id, path); }
  stat(path) { return this.#c.statFile(this.#id, path); }
  list(path) { return this.#c.listFiles(this.#id, path); }
}

/** Puertos publicados de un sandbox: `await sb.endpoints.create(8000)`. */
export class Endpoints {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  /** `ttl` en segundos (hasta 7 días); `public: true` no pide token. */
  create(port, { ttl, public: publico = false } = {}) {
    return this.#c.createEndpoint(this.#id, { port, ttl_seconds: ttl, public: publico });
  }
  list() { return this.#c.listEndpoints(this.#id); }
  revoke(port) { return this.#c.revokeEndpoint(this.#id, port); }
}

/** Contextos de código: guardan el estado entre ejecuciones. */
export class CodeContexts {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; }
  create({ language, cwd } = {}) { return this.#c.createCodeContext(this.#id, { language, cwd }); }
  list() { return this.#c.listCodeContexts(this.#id); }
  get(cid) { return this.#c.getCodeContext(this.#id, cid); }
  delete(cid) { return this.#c.deleteCodeContext(this.#id, cid); }
}

/** Ejecución de código: `for await (const ev of sb.code.run("1+1"))`. */
export class Code {
  #c; #id;
  constructor(client, id) { this.#c = client; this.#id = id; this.contexts = new CodeContexts(client, id); }
  /** Iterador asíncrono de eventos; el último es siempre `done`. Un error del código llega como evento `error`. */
  async *run(code, { language, context, timeout } = {}) {
    const it = await this.#c.runCode(this.#id, { code, language, context_id: context, timeout_seconds: timeout });
    for await (const ev of it) yield ev;
  }
  /** Como `run`, agregado: `{ ok, stdout, stderr, result, error, results, events }`. */
  async runWait(code, opts) {
    const r = { ok: false, stdout: "", stderr: "", result: null, error: null, results: {}, events: [] };
    for await (const ev of this.run(code, opts)) {
      r.events.push(ev);
      if (ev.type === "stdout") r.stdout += ev.text ?? "";
      else if (ev.type === "stderr") r.stderr += ev.text ?? "";
      else if (ev.type === "result") { r.result = ev; r.results = ev.results ?? {}; }
      else if (ev.type === "error") r.error = ev;
      else if (ev.type === "done") r.ok = Boolean(ev.ok);
    }
    return r;
  }
  /** Interrumpe la ejecución en curso de un contexto (el contexto sigue). */
  interrupt(context) { return this.#c.interruptCode(this.#id, context); }
}

export class Sandbox {
  #client;
  constructor(info, client) {
    this.info = info;
    this.#client = client;
    this.files = new Files(client, info.id);
    this.processes = new Processes(client, info.id);
    this.sessions = new Sessions(client, info.id);
    this.endpoints = new Endpoints(client, info.id);
    this.code = new Code(client, info.id);
  }
  get id() { return this.info.id; }
  toString() { return `Sandbox(id=${JSON.stringify(this.info.id)}, state=${JSON.stringify(this.info.state)})`; }
  [INSPECT]() { return this.toString(); }
  toJSON() { return this.info; }

  static #cliente({ client, url, token, timeout } = {}) {
    return client ?? new Client({ url, token, ...(timeout !== undefined && { timeout }) });
  }

  /** `ttl` en segundos. */
  /**
   * `volumes`: [{ name, mount, read_only, clone_from }] de volúmenes del inquilino (Client.createVolume).
   * `env`: objeto con las variables que ve todo exec con `session: "default"`.
   */
  static async create({ template, ttl, image, on_ttl, egress, allow_domains, mem_mib, vcpus, labels, volumes, env, ...conn } = {}) {
    const c = Sandbox.#cliente(conn);
    const info = await c.createSandbox({
      template, image, ttl_seconds: ttl, on_ttl, egress,
      allow_domains: allow_domains?.length ? [...allow_domains] : undefined, mem_mib, vcpus, labels,
      volumes: volumes?.length ? volumes.map((v) => ({ ...v })) : undefined,
      env: env && Object.keys(env).length ? { ...env } : undefined,
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
  /**
   * Array de Sandbox; `.partial` trae X-Kindling-Partial (hosts caídos) o null.
   * Filtros: `state`, `label` (`{k: v}`), `limit` (página) y `cursor`; con `limit`,
   * `.nextCursor` es el cursor de la siguiente página (null en la última).
   */
  static async list({ state, label, limit, cursor, ...conn } = {}) {
    const c = Sandbox.#cliente(conn);
    const { sandboxes, partial, nextCursor } = await c.listSandboxes({ state, label, limit, cursor });
    return Object.assign(sandboxes.map((i) => new Sandbox(i, c)), { partial, nextCursor });
  }

  async refresh() { this.info = await this.#client.getSandbox(this.id); return this; }
  /** Renueva el TTL (segundos; sin valor, el del sandbox). */
  async renew(ttl) { this.info = await this.#client.renewSandbox(this.id, ttl); return this.info; }
  /** Pausa un sandbox en marcha sin volcar su memoria. */
  async pause() { this.info = await this.#client.pauseSandbox(this.id); return this.info; }
  /** Reanuda un sandbox pausado o congelado. */
  async resume() { this.info = await this.#client.resumeSandbox(this.id); return this.info; }
  /** Cambia la política de red (`none`, `internet`, `allowlist`); `applied` dice si vale ya (`live`). */
  setEgress(egress, allowDomains) {
    return this.#client.setEgress(this.id, { egress, allow_domains: allowDomains });
  }
  set_egress(egress, allowDomains) { return this.setEgress(egress, allowDomains); }
  /** Añade o cambia etiquetas propias (`labels`) y quita otras (`remove`). Las reservadas dan BadRequest. */
  async setMetadata({ labels, remove } = {}) {
    this.info = await this.#client.patchMetadata(this.id, { labels, remove });
    return this.info;
  }
  /** Ramifica en `count` copias (memoria, procesos y disco tal como están); cuentan para la cuota. */
  async fork({ count = 1, ttl } = {}) {
    const infos = await this.#client.forkSandbox(this.id, { count, ttl_seconds: ttl });
    return infos.map((i) => new Sandbox(i, this.#client));
  }
  /** Memoria (`memory_mib`) y CPU (`cpu_seconds`, si el daemon la mide). */
  metrics() { return this.#client.sandboxMetrics(this.id); }
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
