// Servidor falso del gateway /v1 según ext/sandbox/api/openapi.yaml (solo node:http).
import http from "node:http";
import type { AddressInfo } from "node:net";

export const TOKEN = "tok-secreto-1234";
export const TOKEN_USER = "tok-sin-admin-1234";

interface Fake {
  id: string; files: Map<string, Buffer>; info: Record<string, unknown>;
  procs: Map<number, Record<string, any>>; sessions: Map<string, Record<string, any>>;
}

export class FakeGateway {
  sandboxes = new Map<string, Fake>();
  n = 0;
  volumes = new Map<string, Record<string, unknown>>();
  /** En un lote, los índices >= esto fallan. */
  batchFailFrom = 0;
  lastBody: any = null;
  /** Trozos del flujo NDJSON de exec; cada uno se escribe por separado (puede partir líneas). */
  streamChunks: string[] = [];
  rateLimitNext = 0;
  /** Cuántas peticiones siguientes devuelven 503 con Retry-After: 2. */
  unavailableNext = 0;
  /** Cuerpo del último POST /v1/sandboxes (lastBody lo pisa cualquier otra llamada). */
  createBody: any = null;
  codeBody: any = null;
  interrupted: string[] = [];
  endpoints = new Map<string, Record<string, any>>();
  contexts = new Map<string, Record<string, any>>();
  requests: { method: string; url: string; auth?: string }[] = [];
  server: http.Server;
  url = "";

  constructor() { this.server = http.createServer((q, r) => { this.handle(q, r).catch((e) => { r.statusCode = 500; r.end(String(e)); }); }); }

  async start() {
    await new Promise<void>((ok) => this.server.listen(0, "127.0.0.1", ok));
    this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }
  async stop() { this.server.closeAllConnections(); await new Promise((ok) => this.server.close(ok)); }

  private err(r: http.ServerResponse, code: number, message: string, errCode?: string, extra: Record<string, string> = {}) {
    r.writeHead(code, { "Content-Type": "application/json", ...extra });
    r.end(JSON.stringify({ message, ...(errCode && { code: errCode }) }));
  }
  private json(r: http.ServerResponse, code: number, v: unknown, h: Record<string, string> = {}) {
    r.writeHead(code, { "Content-Type": "application/json", ...h });
    r.end(JSON.stringify(v));
  }
  private async body(q: http.IncomingMessage) {
    const parts: Buffer[] = [];
    for await (const c of q) parts.push(c as Buffer);
    return Buffer.concat(parts);
  }

  tenants = new Map<string, Record<string, any>>();
  keyCount = 0;

  private async tenantsApi(q: http.IncomingMessage, r: http.ServerResponse, p: string[], admin: boolean) {
    if (!admin) return this.err(r, 403, "managing tenants and keys needs an admin token", "forbidden");
    const raw = (await this.body(q)).toString();
    const req = raw ? JSON.parse(raw) : {};
    this.lastBody = raw ? req : null;
    const none = () => { r.writeHead(204); r.end(); };
    if (p.length === 0) {
      if (q.method === "GET") return this.json(r, 200, { tenants: [...this.tenants.values()] });
      if (this.tenants.has(req.name)) return this.err(r, 409, "tenant already exists", "conflict");
      const t = { id: req.name, name: req.name, source: "store", read_only: false, admin: req.admin ?? false,
        limits: req.limits ?? {}, created: "2026-01-01T00:00:00Z", keys: [], campo_nuevo: 1 };
      this.tenants.set(t.id, t);
      return this.json(r, 201, t);
    }
    const t = this.tenants.get(decodeURIComponent(p[0]));
    if (!t) return this.err(r, 404, "tenant not found", "not_found");
    if (p.length === 1) {
      if (q.method === "GET") return this.json(r, 200, t);
      if (q.method === "PATCH") {
        if ("admin" in req) t.admin = req.admin;
        if ("limits" in req) t.limits = req.limits;
        return this.json(r, 200, t);
      }
      if (q.method === "DELETE") { this.tenants.delete(t.id); return none(); }
    }
    const nueva = (vieja?: Record<string, any>) => {
      const kid = String(++this.keyCount).padStart(12, "0");
      const ficha: Record<string, any> = { id: kid, prefix: "ksk_" + kid, created: "2026-01-01T00:00:00Z" };
      t.keys.push(ficha);
      return { ...ficha, key: `ksk_${kid}_secreto`, ...(vieja && { old_key_id: vieja.id, old_key_expires: vieja.expires }) };
    };
    if (p[1] === "keys" && p.length === 2 && q.method === "POST") return this.json(r, 201, nueva());
    const ficha = t.keys.find((k: any) => k.id === p[2]);
    if (p[1] === "keys" && p.length >= 3 && !ficha) return this.err(r, 404, "key not found", "not_found");
    if (p[1] === "keys" && p.length === 3 && q.method === "DELETE") { ficha.revoked ??= "2026-01-01T00:00:00Z"; return none(); }
    if (p[1] === "keys" && p[3] === "rotate" && q.method === "POST") {
      const gracia = req.grace_minutes ?? 10;
      if (gracia < 0 || gracia > 1440) return this.err(r, 400, "grace period out of range", "bad_request");
      ficha.expires = `en ${gracia} min`;
      return this.json(r, 201, nueva(ficha));
    }
    return this.err(r, 404, "no route", "not_found");
  }

  /** Salida que sirve el proceso falso. */
  procOutput = "hola\nmundo\n";

  private async processes(q: http.IncomingMessage, r: http.ServerResponse, u: URL, s: Fake, resto: string[]) {
    if (resto.length === 0) {
      if (q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString() || "{}");
        this.lastBody = req;
        if (!Array.isArray(req.cmd) || !req.cmd.length) return this.err(r, 400, "cmd is required", "bad_request");
        const proc = { pid: s.procs.size + 1, cmd: req.cmd, state: "running", started_at: "2026-01-01T00:00:00Z", output_bytes: 0 };
        s.procs.set(proc.pid, proc);
        return this.json(r, 201, proc);
      }
      return this.json(r, 200, [...s.procs.values()]);
    }
    const proc = s.procs.get(Number(resto[0]));
    if (!proc) return this.err(r, 404, "no such process", "not_found");
    const sub = resto[1];
    if (!sub && q.method === "GET") {
      if (u.searchParams.get("wait")) Object.assign(proc, { state: "exited", exit_code: 0 });
      return this.json(r, 200, proc);
    }
    if (!sub && q.method === "DELETE") { s.procs.delete(proc.pid); r.writeHead(204); return r.end(); }
    if (sub === "signal" && q.method === "POST") {
      const req = JSON.parse((await this.body(q)).toString() || "{}");
      this.lastBody = req;
      Object.assign(proc, { state: "exited", signal: req.signal ?? "TERM" });
      return this.json(r, 200, proc);
    }
    if (sub === "output" && q.method === "GET") {
      const off = Number(u.searchParams.get("offset") ?? 0);
      const datos = Buffer.from(this.procOutput).subarray(off);
      r.writeHead(200, { "Content-Type": "application/x-ndjson" });
      const tramo = (b: Buffer, o: number) => JSON.stringify({ stream: "stdout", data: b.toString("base64"), offset: o }) + "\n";
      if (datos.length) {
        // Se parte una línea por la mitad para ejercitar el reensamblado.
        const l = tramo(datos.subarray(0, 6), off + Math.min(6, datos.length));
        r.write(l.slice(0, 10)); await new Promise((ok) => setTimeout(ok, 5)); r.write(l.slice(10));
        if (datos.length > 6) r.write(tramo(datos.subarray(6), off + datos.length));
      }
      const fin: Record<string, unknown> = { offset: Buffer.byteLength(this.procOutput) };
      if (u.searchParams.get("follow") === "1") fin.exit = 0;
      return r.end(JSON.stringify(fin) + "\n");
    }
    return this.err(r, 405, "no");
  }

  private async sessions(q: http.IncomingMessage, r: http.ServerResponse, s: Fake, resto: string[]) {
    if (resto.length === 0) {
      if (q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString() || "{}");
        this.lastBody = req;
        const se = { id: req.id ?? `s${s.sessions.size + 1}`, dir: req.dir ?? "", env: req.env ?? [], execs: 0,
          created_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z" };
        s.sessions.set(se.id, se);
        return this.json(r, 201, se);
      }
      return this.json(r, 200, [...s.sessions.values()]);
    }
    const se = s.sessions.get(resto[0]);
    if (!se) return this.err(r, 404, "no such session", "not_found");
    if (q.method === "GET") return this.json(r, 200, se);
    if (q.method === "DELETE") { s.sessions.delete(se.id); r.writeHead(204); return r.end(); }
    return this.err(r, 405, "no");
  }

  private async handle(q: http.IncomingMessage, r: http.ServerResponse) {
    const u = new URL(q.url!, "http://x");
    this.requests.push({ method: q.method!, url: q.url!, auth: q.headers.authorization });
    if (u.pathname === "/v1/health") return this.json(r, 200, { ok: true });
    if (q.headers.authorization !== `Bearer ${TOKEN}` && q.headers.authorization !== `Bearer ${TOKEN_USER}`) return this.err(r, 401, `token inválido: ${q.headers.authorization ?? ""}`);
    if (this.rateLimitNext > 0) {
      this.rateLimitNext--;
      return this.err(r, 429, "cuota superada", "quota_exceeded", { "Retry-After": "7" });
    }
    if (this.unavailableNext > 0) {
      this.unavailableNext--;
      return this.err(r, 503, "busy", "service_unavailable", { "Retry-After": "2" });
    }
    const p = u.pathname.split("/").filter(Boolean); // v1 sandboxes {id} {sub}
    if (p[1] === "tenants") return this.tenantsApi(q, r, p.slice(2), q.headers.authorization === `Bearer ${TOKEN}`);
    if (p[1] === "volumes") {
      if (p.length === 2 && q.method === "GET") return this.json(r, 200, [...this.volumes.values()]);
      if (p.length === 2 && q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString());
        this.lastBody = req;
        if (this.volumes.has(req.name)) return this.err(r, 409, "ya existe");
        const v = { name: req.name, host: req.host ?? "h1", size_mib: req.size_mib ?? 64, used_bytes: 0, in_use: false };
        this.volumes.set(req.name, v);
        return this.json(r, 201, v);
      }
      const v = this.volumes.get(p[2]);
      if (!v) return this.err(r, 404, `no volume ${p[2]}`, "not_found");
      if (q.method === "GET") return this.json(r, 200, v);
      if (q.method === "DELETE") { this.volumes.delete(p[2]); r.writeHead(204); return r.end(); }
    }
    if (p[1] === "sandboxes" && p.length === 2) {
      if (q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString() || "{}");
        this.lastBody = req;
        this.createBody = req;
        if (req.count !== undefined) {
          const ok: unknown[] = [], errors: unknown[] = [];
          for (let i = 0; i < req.count; i++) {
            if (this.batchFailFrom && i >= this.batchFailFrom) { errors.push({ index: i, status: 500, error: "boom" }); continue; }
            const id = `sbx-${++this.n}`;
            const info = { id, host: "h1", state: "running" };
            this.sandboxes.set(id, { id, files: new Map(), info, procs: new Map(), sessions: new Map() });
            ok.push(info);
          }
          if (req.atomic && errors.length) {
            for (const s of ok as { id: string }[]) this.sandboxes.delete(s.id);
            return this.err(r, 500, "boom");
          }
          return this.json(r, errors.length ? 207 : 201, errors.length ? { sandboxes: ok, errors } : { sandboxes: ok });
        }
        const id = `sbx-${++this.n}`;
        const ttl = req.ttl_seconds ?? 300;
        const info = { id, host: "h1", template: req.template ?? "base", image: "img", state: "running", vcpus: 1, mem_mib: 256,
          egress: "none", on_ttl: "delete", ttl_seconds: ttl, created_at: "2026-01-01T00:00:00Z", last_used_at: "2026-01-01T00:00:00Z",
          expires_at: "2026-01-01T00:05:00Z", labels: req.labels ?? {}, campo_nuevo: 1 };
        this.sandboxes.set(id, { id, files: new Map(), info, procs: new Map(), sessions: new Map() });
        return this.json(r, 201, info);
      }
      if (q.method === "GET") {
        let lista = [...this.sandboxes.values()].map((s) => s.info as Record<string, any>).sort((a, b) => (a.id < b.id ? -1 : 1));
        const estado = u.searchParams.get("state");
        if (estado) lista = lista.filter((i) => i.state === estado);
        for (const par of u.searchParams.getAll("label")) {
          const [k, ...v] = par.split("=");
          lista = lista.filter((i) => i.labels?.[k] === v.join("="));
        }
        const cab: Record<string, string> = { "X-Kindling-Partial": "host2" };
        const cur = u.searchParams.get("cursor");
        if (cur) lista = lista.filter((i) => i.id > cur);
        const limit = Number(u.searchParams.get("limit") ?? 0);
        if (limit && lista.length > limit) { lista = lista.slice(0, limit); cab["X-Kindling-Next-Cursor"] = lista[lista.length - 1].id; }
        return this.json(r, 200, lista, cab);
      }
    }
    if (p[1] === "sandboxes" && p.length >= 3) {
      const s = this.sandboxes.get(p[2]);
      if (!s) return this.err(r, 404, `sandbox ${p[2]} no existe`, "not_found");
      const sub = p[3];
      if (!sub) {
        if (q.method === "GET") return this.json(r, 200, s.info);
        if (q.method === "DELETE") { this.sandboxes.delete(s.id); r.writeHead(204); return r.end(); }
      }
      if (sub === "renew" && q.method === "POST") {
        const raw = (await this.body(q)).toString();
        const req = raw ? JSON.parse(raw) : {};
        s.info = { ...s.info, ttl_seconds: req.ttl_seconds ?? s.info.ttl_seconds, expires_at: "2026-01-01T01:00:00Z" };
        return this.json(r, 200, s.info);
      }
      if (sub === "exec" && q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString());
        if (!Array.isArray(req.cmd) || !req.cmd.length) return this.err(r, 400, "cmd vacío");
        if (u.searchParams.get("wait") === "1") {
          return this.json(r, 200, { exit_code: 3, stdout: Buffer.from("héllo\n").toString("base64"),
            stderr: Buffer.from("err\n").toString("base64"), duration_ms: 12, truncated: false, timed_out: false, echo: req });
        }
        r.writeHead(200, { "Content-Type": "application/x-ndjson" });
        for (const c of this.streamChunks) { r.write(c); await new Promise((ok) => setTimeout(ok, 5)); }
        return r.end();
      }
      if ((sub === "pause" || sub === "resume") && q.method === "POST") {
        s.info = { ...s.info, state: sub === "pause" ? "paused" : "running" };
        return this.json(r, 200, s.info);
      }
      if (sub === "egress" && q.method === "PATCH") {
        const req = JSON.parse((await this.body(q)).toString());
        this.lastBody = req;
        return this.json(r, 200, { egress: req.egress, allow_domains: req.allow_domains ?? [], applied: "live", narrowed: false, connections_closed: false });
      }
      if (sub === "endpoints") {
        if (p[4] !== undefined) {
          if (q.method !== "DELETE" || !this.endpoints.has(`${s.id}:${p[4]}`)) return this.err(r, 404, "no such endpoint", "not_found");
          this.endpoints.delete(`${s.id}:${p[4]}`);
          r.writeHead(204); return r.end();
        }
        if (q.method === "POST") {
          const req = JSON.parse((await this.body(q)).toString());
          this.lastBody = req;
          const ep: Record<string, any> = { url: `https://${req.port}-x.example.test`, port: req.port, public: Boolean(req.public), expires_at: "2026-01-01T00:00:00Z" };
          if (!ep.public) ep.token = "tok-ep";
          this.endpoints.set(`${s.id}:${req.port}`, ep);
          return this.json(r, 201, ep);
        }
        return this.json(r, 200, [...this.endpoints].filter(([k]) => k.startsWith(`${s.id}:`)).map(([, v]) => v));
      }
      if (sub === "code" && p[4] === undefined && q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString());
        this.codeBody = req;
        const evs: Record<string, unknown>[] = [{ type: "stdout", text: "hola\n" }];
        const ok = !req.code.includes("fallo");
        evs.push(ok ? { type: "result", text: "2", results: { "text/plain": "2" }, execution_count: 1 }
          : { type: "error", ename: "NameError", evalue: "fallo", traceback: ["t1"] });
        evs.push({ type: "done", ok, duration_ms: 3, execution_count: 1 });
        r.writeHead(200, { "Content-Type": "application/x-ndjson" });
        return r.end(evs.map((e) => JSON.stringify(e) + "\n").join(""));
      }
      if (sub === "code" && p[4] === "contexts") {
        if (p[5] === undefined) {
          if (q.method === "POST") {
            const req = JSON.parse((await this.body(q)).toString() || "{}");
            const c = { id: (this.contexts.size + 1).toString(16).padStart(12, "0"), language: req.language ?? "python", pid: 100 + this.contexts.size, cwd: req.cwd ?? "", started_at: "2026-01-01T00:00:00Z" };
            this.contexts.set(`${s.id}:${c.id}`, c);
            return this.json(r, 201, c);
          }
          return this.json(r, 200, [...this.contexts].filter(([k]) => k.startsWith(`${s.id}:`)).map(([, v]) => v));
        }
        const c = this.contexts.get(`${s.id}:${p[5]}`);
        if (!c) return this.err(r, 404, "no such context", "not_found");
        if (p[6] === "execution" && q.method === "DELETE") { this.interrupted.push(c.id); r.writeHead(204); return r.end(); }
        if (q.method === "GET") return this.json(r, 200, c);
        if (q.method === "DELETE") { this.contexts.delete(`${s.id}:${c.id}`); r.writeHead(204); return r.end(); }
      }
      if (sub === "files" && p[4] === "move" && q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString());
        const f = s.files.get(req.from);
        if (!f) return this.err(r, 404, "move failed: No such file", "not_found");
        if (s.files.has(req.to) && !req.overwrite) return this.err(r, 409, "already exists", "conflict");
        s.files.delete(req.from); s.files.set(req.to, f);
        return this.json(r, 200, req);
      }
      if (sub === "files" && p[4] === "mkdir" && q.method === "POST") {
        const req = JSON.parse((await this.body(q)).toString());
        return this.json(r, 201, { path: req.path, size: 0, mode: "0755", is_dir: true, mod_time: "2026-01-01T00:00:00Z" });
      }
      if (sub === "files" && p[4] === "search" && q.method === "GET") {
        const raiz = u.searchParams.get("path")!, glob = u.searchParams.get("glob")!;
        const re = new RegExp("^" + glob.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*") + "$");
        const matches = [...s.files.keys()].filter((f) => f.startsWith(raiz) && re.test(f.split("/").pop()!)).sort();
        return this.json(r, 200, { path: raiz, glob, matches, truncated: false });
      }
      if (sub === "processes") return this.processes(q, r, u, s, p.slice(4));
      if (sub === "sessions") return this.sessions(q, r, s, p.slice(4));
      if (sub === "files") {
        const path = u.searchParams.get("path");
        if (!path) return this.err(r, 400, "falta path");
        if (q.method === "PUT" && u.searchParams.get("tar") === "1") {
          // Un tar de un solo fichero basta: nombre en los primeros 100 bytes, tamaño en octal en 124.
          const b = await this.body(q);
          let n = 0;
          for (let o = 0; o + 512 <= b.length && b[o] !== 0; ) {
            const nombre = b.subarray(o, o + 100).toString().replace(/\0.*$/, "");
            const tam = parseInt(b.subarray(o + 124, o + 135).toString(), 8);
            s.files.set(path.replace(/\/+$/, "") + "/" + nombre, Buffer.from(b.subarray(o + 512, o + 512 + tam)));
            o += 512 + Math.ceil(tam / 512) * 512;
            n++;
          }
          return this.json(r, 200, { entries: n, files: n, bytes: b.length });
        }
        if (q.method === "GET" && u.searchParams.get("tar") === "1") {
          const pref = path.replace(/\/+$/, "") + "/";
          const trozos: Buffer[] = [];
          for (const [p2, d] of s.files) {
            if (!p2.startsWith(pref)) continue;
            const cab = Buffer.alloc(512);
            cab.write(p2.slice(pref.length), 0);
            cab.write(d.length.toString(8).padStart(11, "0"), 124);
            trozos.push(cab, d, Buffer.alloc((512 - (d.length % 512)) % 512));
          }
          trozos.push(Buffer.alloc(1024));
          r.writeHead(200, { "Content-Type": "application/x-tar" });
          return r.end(Buffer.concat(trozos));
        }
        if (q.method === "PUT") {
          const b = await this.body(q);
          s.files.set(path, b);
          return this.json(r, 200, { path, size: b.length, mode: u.searchParams.get("mode") ?? "0644", is_dir: false, mod_time: "2026-01-01T00:00:00Z" });
        }
        if (q.method === "GET" && u.searchParams.get("list") === "1") {
          const pref = path.replace(/\/+$/, "") + "/";
          const entries = [...s.files].filter(([p]) => p.startsWith(pref) && !p.slice(pref.length).includes("/"))
            .map(([p, b]) => ({ name: p.slice(pref.length), type: "file", size: b.length, mode: "0644", mod_time: "2026-01-01T00:00:00Z" }));
          return this.json(r, 200, { path, entries });
        }
        const f = s.files.get(path);
        if (!f) return this.err(r, 404, `no existe ${path}`, "not_found");
        if (q.method === "GET") {
          if (u.searchParams.get("stat") === "1") return this.json(r, 200, { path, size: f.length, mode: "0644", is_dir: false, mod_time: "2026-01-01T00:00:00Z" });
          const rango = q.headers.range;
          if (rango) {
            const [, a, b] = /^bytes=(\d*)-(\d*)$/.exec(rango)!;
            const ini = a === "" ? Math.max(f.length - Number(b), 0) : Number(a);
            const fin = a === "" || b === "" ? f.length - 1 : Math.min(Number(b), f.length - 1);
            if (ini >= f.length) return this.err(r, 416, "range outside the file", "range_not_satisfiable", { "Content-Range": `bytes */${f.length}` });
            r.writeHead(206, { "Content-Type": "application/octet-stream", "Content-Range": `bytes ${ini}-${fin}/${f.length}` });
            return r.end(f.subarray(ini, fin + 1));
          }
          r.writeHead(200, { "Content-Type": "application/octet-stream" });
          return r.end(f);
        }
        if (q.method === "DELETE") { s.files.delete(path); r.writeHead(204); return r.end(); }
      }
    }
    this.err(r, 404, "ruta desconocida");
  }
}
