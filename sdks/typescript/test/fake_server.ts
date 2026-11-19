// Servidor falso del gateway /v1 según ext/sandbox/api/openapi.yaml (solo node:http).
import http from "node:http";
import type { AddressInfo } from "node:net";

export const TOKEN = "tok-secreto-1234";

interface Fake { id: string; files: Map<string, Buffer>; info: Record<string, unknown> }

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

  private async handle(q: http.IncomingMessage, r: http.ServerResponse) {
    const u = new URL(q.url!, "http://x");
    this.requests.push({ method: q.method!, url: q.url!, auth: q.headers.authorization });
    if (u.pathname === "/v1/health") return this.json(r, 200, { ok: true });
    if (q.headers.authorization !== `Bearer ${TOKEN}`) return this.err(r, 401, `token inválido: ${q.headers.authorization ?? ""}`);
    if (this.rateLimitNext > 0) {
      this.rateLimitNext--;
      return this.err(r, 429, "cuota superada", "quota_exceeded", { "Retry-After": "7" });
    }
    const p = u.pathname.split("/").filter(Boolean); // v1 sandboxes {id} {sub}
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
        if (req.count !== undefined) {
          const ok: unknown[] = [], errors: unknown[] = [];
          for (let i = 0; i < req.count; i++) {
            if (this.batchFailFrom && i >= this.batchFailFrom) { errors.push({ index: i, status: 500, error: "boom" }); continue; }
            const id = `sbx-${++this.n}`;
            const info = { id, host: "h1", state: "running" };
            this.sandboxes.set(id, { id, files: new Map(), info });
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
        this.sandboxes.set(id, { id, files: new Map(), info });
        return this.json(r, 201, info);
      }
      if (q.method === "GET") return this.json(r, 200, [...this.sandboxes.values()].map((s) => s.info), { "X-Kindling-Partial": "host2" });
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
      if (sub === "files") {
        const path = u.searchParams.get("path");
        if (!path) return this.err(r, 400, "falta path");
        if (q.method === "PUT") {
          const b = await this.body(q);
          s.files.set(path, b);
          return this.json(r, 200, { path, size: b.length, mode: u.searchParams.get("mode") ?? "0644", is_dir: false, mod_time: "2026-01-01T00:00:00Z" });
        }
        const f = s.files.get(path);
        if (!f) return this.err(r, 404, `no existe ${path}`, "not_found");
        if (q.method === "GET") {
          if (u.searchParams.get("stat") === "1") return this.json(r, 200, { path, size: f.length, mode: "0644", is_dir: false, mod_time: "2026-01-01T00:00:00Z" });
          r.writeHead(200, { "Content-Type": "application/octet-stream" });
          return r.end(f);
        }
        if (q.method === "DELETE") { s.files.delete(path); r.writeHead(204); return r.end(); }
      }
    }
    this.err(r, 404, "ruta desconocida");
  }
}
