// Transporte HTTP y rutas de la API /v1 (solo fetch nativo).

import { ConfigError, TransportError, errorPara } from "./errors.js";

export const ENV_URL = "KLING_SANDBOX_URL";
export const ENV_TOKEN = "KLING_SANDBOX_TOKEN";
export const URL_POR_DEFECTO = "http://localhost:8088";

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
   * @param {{url?: string, token?: string, timeout?: number|null, fetch?: typeof fetch}} [opts]
   *   timeout en segundos (por defecto 60; null = sin plazo).
   */
  constructor({ url, token, timeout = 60, fetch: f } = {}) {
    const env = typeof process !== "undefined" ? process.env : {};
    const crudo = url || env[ENV_URL] || URL_POR_DEFECTO;
    let u;
    try { u = new URL(crudo); } catch { throw new ConfigError("URL del gateway inválida"); }
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new ConfigError("URL del gateway inválida");
    this.url = `${u.protocol}//${u.host}${u.pathname.replace(/\/+$/, "")}`; // sin credenciales ni query
    this.#token = token ?? env[ENV_TOKEN] ?? null;
    this.timeout = timeout;
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

  /** Devuelve la Response ya validada (2xx); si no, lanza el error tipado. */
  async _abrir(metodo, ruta, { query, body, contentType, timeout = this.timeout, auth = true } = {}) {
    const qs = query ? new URLSearchParams(query).toString() : "";
    const destino = this.url + ruta.split("/").map(encodeURIComponent).join("/") + (qs ? "?" + qs : "");
    const cab = { Accept: "application/json, application/x-ndjson, application/octet-stream" };
    if (auth && this.#token) cab.Authorization = "Bearer " + this.#token;
    if (contentType) cab["Content-Type"] = contentType;
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
  async listSandboxes() {
    const resp = await this._abrir("GET", "/v1/sandboxes");
    const partial = resp.headers.get("x-kindling-partial");
    const crudo = await this._leer(resp, "/v1/sandboxes");
    let lista;
    try { lista = JSON.parse(new TextDecoder().decode(crudo) || "[]") ?? []; } catch {
      throw new TransportError("respuesta no JSON de /v1/sandboxes");
    }
    return { sandboxes: lista, partial };
  }
  async deleteSandbox(id) { await this._json("DELETE", "/v1/sandboxes/" + id); }
  renewSandbox(id, ttlSeconds) {
    return this._json("POST", `/v1/sandboxes/${id}/renew`,
      ttlSeconds != null ? { cuerpo: { ttl_seconds: ttlSeconds } } : {});
  }

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

  // ---- ficheros
  async readFile(id, path) {
    const ruta = `/v1/sandboxes/${id}/files`;
    return this._leer(await this._abrir("GET", ruta, { query: { path }, timeout: null }), ruta);
  }
  statFile(id, path) {
    return this._json("GET", `/v1/sandboxes/${id}/files`, { query: { path, stat: "1" }, timeout: null });
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
