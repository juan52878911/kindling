// Transporte HTTP y rutas de la API /v1 (solo fetch nativo).

import { ApiError, ConfigError, TransportError, errorPara } from "./errors.js";

export const ENV_URL = "KLING_SANDBOX_URL";
export const ENV_TOKEN = "KLING_SANDBOX_TOKEN";
export const URL_POR_DEFECTO = "http://127.0.0.1:8090";

const INSPECT = Symbol.for("nodejs.util.inspect.custom");

/** Segundos desde un Retry-After (número o fecha HTTP); null si no hay o es ilegible. */
export function parseRetryAfter(v) {
  if (!v) return null;
  if (/^\s*\d+(\.\d+)?\s*$/.test(v)) return Math.max(0, Number(v));
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : Math.max(0, (t - Date.now()) / 1000);
}

export class Client {
  #token;

  /**
   * @param {{url?: string, token?: string, timeout?: number|null, fetch?: typeof fetch,
   *   retries?: number, maxRetryWait?: number}} [opts]
   *   timeout en segundos (por defecto 60; null = sin plazo). `retries` > 0 reintenta
   *   un 429 o 503 esperando su `Retry-After` (1 s si no lo trae; nunca más de
   *   `maxRetryWait`, 30 s por defecto). Por defecto no reintenta nada.
   */
  constructor({ url, token, timeout = 60, fetch: f, retries = 0, maxRetryWait = 30 } = {}) {
    const env = typeof process !== "undefined" ? process.env : {};
    const crudo = url || env[ENV_URL] || URL_POR_DEFECTO;
    let u;
    try { u = new URL(crudo); } catch { throw new ConfigError("URL del gateway inválida"); }
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new ConfigError("URL del gateway inválida");
    this.url = `${u.protocol}//${u.host}${u.pathname.replace(/\/+$/, "")}`; // sin credenciales ni query
    this.#token = token ?? env[ENV_TOKEN] ?? null;
    this.timeout = timeout;
    this.retries = Math.max(0, Math.trunc(retries));
    this.maxRetryWait = maxRetryWait;
    this._dormir = (seg) => new Promise((r) => setTimeout(r, seg * 1000));
    this._fetch = f ?? globalThis.fetch;
    if (typeof this._fetch !== "function") throw new ConfigError("hace falta fetch (Node >= 18)");
  }

  toString() { return `Client(url=${JSON.stringify(this.url)}, token=${this.#token ? "<oculto>" : "null"})`; }
  toJSON() { return { url: this.url, token: this.#token ? "<oculto>" : null }; }
  [INSPECT]() { return this.toString(); }

  #limpia(texto) {
    const t = this.#token;
    return t && String(texto).includes(t) ? String(texto).split(t).join("<oculto>") : String(texto);
  }

  /** Devuelve la Response ya validada (2xx); si no, lanza el error tipado. Con `retries`, un 429/503 se reintenta. */
  async _abrir(metodo, ruta, opts = {}) {
    for (let intento = 0; ; intento++) {
      try {
        return await this._abrirUna(metodo, ruta, opts);
      } catch (e) {
        if (!(e instanceof ApiError) || (e.status !== 429 && e.status !== 503) || intento >= this.retries) throw e;
        await this._dormir(Math.min(Math.max(e.retry_after ?? 1, 0), this.maxRetryWait));
      }
    }
  }

  async _abrirUna(metodo, ruta, { query, body, contentType, headers, timeout = this.timeout, auth = true } = {}) {
    const qs = query ? new URLSearchParams(query).toString() : "";
    const destino = this.url + ruta.split("/").map(encodeURIComponent).join("/") + (qs ? "?" + qs : "");
    const cab = { Accept: "application/json, application/x-ndjson, application/octet-stream" };
    if (auth && this.#token) cab.Authorization = "Bearer " + this.#token;
    if (contentType) cab["Content-Type"] = contentType;
    if (headers) Object.assign(cab, headers);
    let resp;
    try {
      resp = await this._fetch(destino, {
        method: metodo, headers: cab, body, redirect: "error",
        signal: timeout ? AbortSignal.timeout(timeout * 1000) : undefined,
      });
    } catch (e) {
      const causa = e?.cause?.message ? `: ${e.cause.message}` : "";
      throw new TransportError(this.#limpia(`${metodo} ${ruta}: ${e?.message ?? e}${causa}`));
    }
    if (resp.status >= 300) {
      let datos = "";
      try { datos = await resp.text(); } catch { /* cuerpo cortado */ }
      let msg = "", code;
      try {
        const j = JSON.parse(datos);
        msg = String(j.message ?? ""); code = j.code ?? undefined;
      } catch { msg = datos.trim().slice(0, 500); }
      throw errorPara(resp.status, this.#limpia(msg || resp.statusText || ""), code,
        parseRetryAfter(resp.headers.get("retry-after")));
    }
    return resp;
  }

  async _leer(resp, ruta) {
    try { return new Uint8Array(await resp.arrayBuffer()); } catch (e) {
      throw new TransportError(this.#limpia(`${ruta}: ${e?.message ?? e}`));
    }
  }

  async _json(metodo, ruta, { query, cuerpo, timeout, auth } = {}) {
    const body = cuerpo !== undefined ? JSON.stringify(cuerpo) : undefined;
    const resp = await this._abrir(metodo, ruta, {
      query, body, contentType: body !== undefined ? "application/json" : undefined,
      ...(timeout !== undefined && { timeout }), ...(auth !== undefined && { auth }),
    });
    const crudo = await this._leer(resp, ruta);
    if (resp.status === 204 || crudo.length === 0) return null;
    try { return JSON.parse(new TextDecoder().decode(crudo)); } catch {
      throw new TransportError(`respuesta no JSON de ${ruta}`);
    }
  }

  // ---- ops
  async health() { return Boolean((await this._json("GET", "/v1/health", { auth: false }))?.ok); }
  async hosts() { return (await this._json("GET", "/v1/hosts")) ?? []; }
  async templates() { return (await this._json("GET", "/v1/templates")) ?? []; }
  async metrics() {
    const resp = await this._abrir("GET", "/v1/metrics");
    return new TextDecoder().decode(await this._leer(resp, "/v1/metrics"));
  }

  // ---- sandboxes
  async createSandbox(peticion) {
    const cuerpo = Object.fromEntries(Object.entries(peticion).filter(([, v]) => v !== undefined && v !== null));
    return this._json("POST", "/v1/sandboxes", { cuerpo });
  }
  /** Lote: devuelve `{ sandboxes, errors }` (errors solo sin `atomic`: lote parcial). */
  async createSandboxes(count, { atomic = false, ...peticion } = {}) {
    const cuerpo = { ...Object.fromEntries(Object.entries(peticion).filter(([, v]) => v !== undefined && v !== null)), count };
    if (atomic) cuerpo.atomic = true;
    const j = (await this._json("POST", "/v1/sandboxes", { cuerpo })) ?? {};
    return { sandboxes: j.sandboxes ?? [], errors: j.errors ?? [] };
  }
  getSandbox(id) { return this._json("GET", "/v1/sandboxes/" + id); }
  /**
   * `{ sandboxes, partial, nextCursor }`: `partial` es X-Kindling-Partial y `nextCursor`
   * el X-Kindling-Next-Cursor (null en la última página). `label` es un objeto
   * `{clave: valor}` o un array de "clave=valor".
   */
  async listSandboxes({ state, label, limit, cursor } = {}) {
    const query = [];
    if (state) query.push(["state", state]);
    if (label) {
      for (const par of Array.isArray(label) ? label : Object.entries(label).map(([k, v]) => `${k}=${v}`)) query.push(["label", par]);
    }
    if (limit) query.push(["limit", String(Math.trunc(limit))]);
    if (cursor) query.push(["cursor", cursor]);
    const resp = await this._abrir("GET", "/v1/sandboxes", query.length ? { query } : {});
    const partial = resp.headers.get("x-kindling-partial");
    const nextCursor = resp.headers.get("x-kindling-next-cursor");
    const crudo = await this._leer(resp, "/v1/sandboxes");
    let lista;
    try { lista = JSON.parse(new TextDecoder().decode(crudo) || "[]") ?? []; } catch {
      throw new TransportError("respuesta no JSON de /v1/sandboxes");
    }
    return { sandboxes: lista, partial, nextCursor };
  }
  async deleteSandbox(id) { await this._json("DELETE", "/v1/sandboxes/" + id); }
  renewSandbox(id, ttlSeconds) {
    return this._json("POST", `/v1/sandboxes/${id}/renew`,
      ttlSeconds != null ? { cuerpo: { ttl_seconds: ttlSeconds } } : {});
  }

  pauseSandbox(id) { return this._json("POST", `/v1/sandboxes/${id}/pause`); }
  resumeSandbox(id) { return this._json("POST", `/v1/sandboxes/${id}/resume`); }
  /** Cambia la política de red; devuelve `{ egress, allow_domains, applied, ... }`. */
  setEgress(id, { egress, allow_domains } = {}) {
    const cuerpo = {};
    if (egress !== undefined) cuerpo.egress = egress;
    if (allow_domains !== undefined) cuerpo.allow_domains = [...allow_domains];
    return this._json("PATCH", `/v1/sandboxes/${id}/egress`, { cuerpo });
  }

  /** Añade/cambia (`labels`) y quita (`remove`) etiquetas propias; devuelve el sandbox. */
  patchMetadata(id, { labels, remove } = {}) {
    const cuerpo = {};
    if (labels && Object.keys(labels).length) cuerpo.labels = { ...labels };
    if (remove && remove.length) cuerpo.remove = [...remove];
    return this._json("PATCH", `/v1/sandboxes/${id}/metadata`, { cuerpo });
  }
  /** Ramifica en `count` copias; devuelve la lista de sandboxes (info). */
  async forkSandbox(id, { count, ttl_seconds } = {}) {
    const cuerpo = {};
    if (count != null) cuerpo.count = Math.trunc(count);
    if (ttl_seconds != null) cuerpo.ttl_seconds = Math.trunc(ttl_seconds);
    const r = await this._json("POST", `/v1/sandboxes/${id}/fork`, { cuerpo });
    return r?.sandboxes ?? [];
  }
  sandboxMetrics(id) { return this._json("GET", `/v1/sandboxes/${id}/metrics`); }

  // ---- endpoints
  createEndpoint(id, { port, ttl_seconds, public: publico } = {}) {
    const cuerpo = { port };
    if (ttl_seconds != null) cuerpo.ttl_seconds = Math.trunc(ttl_seconds);
    if (publico) cuerpo.public = true;
    return this._json("POST", `/v1/sandboxes/${id}/endpoints`, { cuerpo });
  }
  async listEndpoints(id) { return (await this._json("GET", `/v1/sandboxes/${id}/endpoints`)) ?? []; }
  async revokeEndpoint(id, port) { await this._json("DELETE", `/v1/sandboxes/${id}/endpoints/${Math.trunc(port)}`); }

  // ---- código
  /** Iterador asíncrono de eventos NDJSON crudos; el último es siempre `done`. */
  async runCode(id, { code, language, context_id, timeout_seconds } = {}) {
    const cuerpo = { code };
    if (language) cuerpo.language = language;
    if (context_id) cuerpo.context_id = context_id;
    if (timeout_seconds) cuerpo.timeout_seconds = Math.trunc(timeout_seconds);
    const ruta = `/v1/sandboxes/${id}/code`;
    const resp = await this._abrir("POST", ruta, {
      body: JSON.stringify(cuerpo), contentType: "application/json", timeout: (timeout_seconds || 60) + 30,
    });
    return this.#eventos(resp, ruta);
  }
  createCodeContext(id, { language, cwd } = {}) {
    const cuerpo = {};
    if (language) cuerpo.language = language;
    if (cwd) cuerpo.cwd = cwd;
    return this._json("POST", `/v1/sandboxes/${id}/code/contexts`, { cuerpo });
  }
  async listCodeContexts(id) { return (await this._json("GET", `/v1/sandboxes/${id}/code/contexts`)) ?? []; }
  getCodeContext(id, cid) { return this._json("GET", `/v1/sandboxes/${id}/code/contexts/${cid}`); }
  async deleteCodeContext(id, cid) { await this._json("DELETE", `/v1/sandboxes/${id}/code/contexts/${cid}`); }
  async interruptCode(id, cid) { await this._json("DELETE", `/v1/sandboxes/${id}/code/contexts/${cid}/execution`); }

  // ---- exec
  // Sin tope de comando la lectura puede esperar indefinidamente.
  static _plazoExec(p) { return p.timeout_seconds ? p.timeout_seconds + 30 : null; }

  async execWait(id, peticion) {
    return this._json("POST", `/v1/sandboxes/${id}/exec`,
      { query: { wait: "1" }, cuerpo: peticion, timeout: Client._plazoExec(peticion) });
  }

  /** Iterador asíncrono de eventos NDJSON crudos. La petición se envía al llamar. */
  async execStream(id, peticion) {
    const ruta = `/v1/sandboxes/${id}/exec`;
    const resp = await this._abrir("POST", ruta, {
      body: JSON.stringify(peticion), contentType: "application/json", timeout: Client._plazoExec(peticion),
    });
    return this.#eventos(resp, ruta);
  }

  async *#eventos(resp, ruta) {
    if (!resp.body) return;
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let resto = "";
    const parsea = (linea) => {
      linea = linea.trim();
      if (!linea) return null;
      try { return JSON.parse(linea); } catch { throw new TransportError(`${ruta}: línea NDJSON inválida`); }
    };
    try {
      for (;;) {
        let r;
        try { r = await reader.read(); } catch (e) {
          throw new TransportError(this.#limpia(`${ruta}: flujo cortado: ${e?.message ?? e}`));
        }
        if (r.done) break;
        resto += dec.decode(r.value, { stream: true });
        let i;
        while ((i = resto.indexOf("\n")) >= 0) {
          const ev = parsea(resto.slice(0, i));
          resto = resto.slice(i + 1);
          if (ev) yield ev;
        }
      }
      resto += dec.decode();
      const ev = parsea(resto);
      if (ev) yield ev;
    } finally {
      reader.cancel().catch(() => {});
    }
  }

  // ---- procesos de fondo y sesiones
  startProcess(id, peticion) { return this._json("POST", `/v1/sandboxes/${id}/processes`, { cuerpo: peticion }); }
  async listProcesses(id) { return (await this._json("GET", `/v1/sandboxes/${id}/processes`)) ?? []; }
  getProcess(id, pid, wait) {
    // Con wait la lectura espera hasta ese tiempo: el timeout del cliente lo cubre.
    return this._json("GET", `/v1/sandboxes/${id}/processes/${pid}`,
      wait ? { query: { wait: String(Math.trunc(wait)) }, timeout: wait + 30 } : {});
  }
  signalProcess(id, pid, signal) {
    return this._json("POST", `/v1/sandboxes/${id}/processes/${pid}/signal`, { cuerpo: signal ? { signal } : {} });
  }
  async deleteProcess(id, pid) { await this._json("DELETE", `/v1/sandboxes/${id}/processes/${pid}`); }
  /** Iterador asíncrono de eventos crudos. Con follow no hay tope de tiempo. */
  async processOutput(id, pid, offset = 0, follow = false) {
    const ruta = `/v1/sandboxes/${id}/processes/${pid}/output`;
    const query = { offset: String(Math.trunc(offset)), ...(follow && { follow: "1" }) };
    const resp = await this._abrir("GET", ruta, { query, ...(follow && { timeout: null }) });
    return this.#eventos(resp, ruta);
  }
  createSession(id, peticion) { return this._json("POST", `/v1/sandboxes/${id}/sessions`, { cuerpo: peticion }); }
  async listSessions(id) { return (await this._json("GET", `/v1/sandboxes/${id}/sessions`)) ?? []; }
  getSession(id, sid) { return this._json("GET", `/v1/sandboxes/${id}/sessions/${sid}`); }
  async deleteSession(id, sid) { await this._json("DELETE", `/v1/sandboxes/${id}/sessions/${sid}`); }

  // ---- ficheros
  /**
   * `range`: `[inicio, fin]` inclusivos (`fin` null = hasta el final) o una cadena
   * `bytes=a-b` / `bytes=-n`. El gateway contesta 206 y se devuelve solo el trozo.
   */
  async readFile(id, path, range) {
    const ruta = `/v1/sandboxes/${id}/files`;
    const headers = range == null ? undefined
      : { Range: typeof range === "string" ? range : `bytes=${range[0]}-${range[1] ?? ""}` };
    return this._leer(await this._abrir("GET", ruta, { query: { path }, timeout: null, headers }), ruta);
  }
  async moveFile(id, from, to, overwrite = false) {
    await this._json("POST", `/v1/sandboxes/${id}/files/move`, { cuerpo: { from, to, ...(overwrite && { overwrite: true }) } });
  }
  chmodFile(id, path, mode, recursive = false) {
    return this._json("POST", `/v1/sandboxes/${id}/files/chmod`, { cuerpo: { path, mode, ...(recursive && { recursive: true }) } });
  }
  replaceInFile(id, path, old, nuevo, all = false) {
    return this._json("POST", `/v1/sandboxes/${id}/files/replace`, { cuerpo: { path, old, new: nuevo, ...(all && { all: true }) } });
  }
  mkdirFile(id, path) { return this._json("POST", `/v1/sandboxes/${id}/files/mkdir`, { cuerpo: { path } }); }
  searchFiles(id, path, glob) {
    return this._json("GET", `/v1/sandboxes/${id}/files/search`, { query: { path, glob } });
  }
  async uploadTar(id, path, datos, { mkdir = false } = {}) {
    const ruta = `/v1/sandboxes/${id}/files`;
    const resp = await this._abrir("PUT", ruta, {
      query: { path, tar: "1", ...(mkdir && { mkdir: "1" }) }, body: datos, contentType: "application/x-tar", timeout: null,
    });
    const crudo = await this._leer(resp, ruta);
    return crudo.length ? JSON.parse(new TextDecoder().decode(crudo)) : {};
  }
  async downloadTar(id, path) {
    const ruta = `/v1/sandboxes/${id}/files`;
    return this._leer(await this._abrir("GET", ruta, { query: { path, tar: "1" }, timeout: null }), ruta);
  }
  statFile(id, path) {
    return this._json("GET", `/v1/sandboxes/${id}/files`, { query: { path, stat: "1" }, timeout: null });
  }
  listFiles(id, path) {
    return this._json("GET", `/v1/sandboxes/${id}/files`, { query: { path, list: "1" }, timeout: null });
  }
  async writeFile(id, path, datos, { mode, mkdir } = {}) {
    const q = { path };
    if (mode != null) q.mode = mode.toString(8).padStart(4, "0");
    if (mkdir) q.mkdir = "1";
    const ruta = `/v1/sandboxes/${id}/files`;
    const resp = await this._abrir("PUT", ruta, {
      query: q, body: datos, contentType: "application/octet-stream", timeout: null,
    });
    const crudo = await this._leer(resp, ruta);
    return crudo.length ? JSON.parse(new TextDecoder().decode(crudo)) : {};
  }
  async deleteFile(id, path) {
    await this._json("DELETE", `/v1/sandboxes/${id}/files`, { query: { path }, timeout: null });
  }

  // ---- volúmenes
  createVolume(name, { size_mib, clone_from, host } = {}) {
    const cuerpo = Object.fromEntries(Object.entries({ name, size_mib, clone_from, host }).filter(([, v]) => v !== undefined && v !== null));
    return this._json("POST", "/v1/volumes", { cuerpo });
  }
  getVolume(name) { return this._json("GET", "/v1/volumes/" + name); }
  async listVolumes() { return (await this._json("GET", "/v1/volumes")) ?? []; }
  async deleteVolume(name) { await this._json("DELETE", "/v1/volumes/" + name); }

  // ---- grafos
  createGraph(template) { return this._json("POST", "/v1/graphs", { cuerpo: { template } }); }
  getGraph(id) { return this._json("GET", "/v1/graphs/" + id); }
  async listGraphs() { return (await this._json("GET", "/v1/graphs")) ?? []; }
  async deleteGraph(id) { await this._json("DELETE", "/v1/graphs/" + id); }
}
