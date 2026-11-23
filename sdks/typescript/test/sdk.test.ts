import { test, before, after, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { inspect } from "node:util";
import { randomBytes } from "node:crypto";
import { Admin, Client, Sandbox, Forbidden, NotFound, Unauthorized, RateLimited, ApiError, ServerError, TransportError, ConfigError, Conflict } from "../src/index.js";
import { FakeGateway, TOKEN, TOKEN_USER } from "./fake_server.ts";

const gw = new FakeGateway();
before(() => gw.start());
after(() => gw.stop());
beforeEach(() => { gw.tenants.clear(); gw.batchFailFrom = 0; gw.streamChunks = []; gw.rateLimitNext = 0; gw.unavailableNext = 0; gw.requests = []; });
const conn = () => ({ url: gw.url, token: TOKEN });

test("create / get / list / renew / delete", async () => {
  const sb = await Sandbox.create({ template: "py", ttl: 120, labels: { a: "b" }, ...conn() });
  assert.match(sb.id, /^sbx-/);
  assert.equal(sb.info.ttl_seconds, 120);
  assert.equal(sb.info.campo_nuevo, 1); // campos nuevos se toleran
  const otra = await Sandbox.connect(sb.id, conn());
  assert.equal(otra.id, sb.id);
  const lista = await Sandbox.list(conn());
  assert.ok(lista.length >= 1);
  assert.equal(lista.partial, "host2");
  const info = await sb.renew(900);
  assert.equal(info.ttl_seconds, 900);
  await sb.renew();
  await sb.delete();
  await assert.rejects(Sandbox.connect(sb.id, conn()), NotFound);
});

test("asyncDispose (await using) borra al salir", async () => {
  let id = "";
  const sb = await Sandbox.create(conn());
  try {
    id = sb.id;
    assert.ok(gw.sandboxes.has(id));
  } finally { await sb[Symbol.asyncDispose](); } // lo que hace `await using` (Node ≥ 24 / TS ≥ 5.2)
  await sb[Symbol.asyncDispose](); // idempotente: el 404 se ignora
  assert.ok(!gw.sandboxes.has(id));
});

test("exec wait: stdout/stderr/exit_code y petición", async () => {
  const sb = await Sandbox.create(conn());
  const r = await sb.exec("echo hola", { cwd: "/tmp", env: { A: "1" }, timeout: 5, stdin: "x" });
  assert.equal(r.exit_code, 3);
  assert.equal(r.stdout, "héllo\n");
  assert.equal(r.stderr, "err\n");
  const q = gw.requests.at(-1)!;
  assert.match(q.url, /exec\?wait=1$/);
});

test("exec stream: líneas partidas a mitad", async () => {
  const sb = await Sandbox.create(conn());
  const l1 = JSON.stringify({ stream: "stdout", data: Buffer.from("uno").toString("base64") });
  const l2 = JSON.stringify({ stream: "stderr", data: Buffer.from("dós").toString("base64") });
  const l3 = JSON.stringify({ exit: 0, duration_ms: 4, truncated: false, timed_out: false });
  const todo = `${l1}\n${l2}\n${l3}\n`;
  // trozos de 7 bytes: parten líneas y hasta caracteres multibyte
  const buf = Buffer.from(todo);
  gw.streamChunks = [];
  for (let i = 0; i < buf.length; i += 7) gw.streamChunks.push(buf.subarray(i, i + 7) as unknown as string);
  const evs = [];
  for await (const e of sb.execStream(["echo"])) evs.push(e);
  assert.equal(evs.length, 3);
  assert.deepEqual([evs[0].stream, evs[0].text], ["stdout", "uno"]);
  assert.deepEqual([evs[1].stream, evs[1].text], ["stderr", "dós"]);
  assert.equal(evs[2].exit, 0);
});

test("exec stream: error previo al flujo → excepción tipada", async () => {
  const sb = await Sandbox.create(conn());
  await assert.rejects(async () => { for await (const _ of sb.exec_stream([])) { /* nada */ } }, ApiError);
});

test("429 con Retry-After", async () => {
  const sb = await Sandbox.create(conn());
  gw.rateLimitNext = 1;
  await assert.rejects(sb.exec("x"), (e: RateLimited) => {
    assert.ok(e instanceof RateLimited);
    assert.equal(e.status, 429);
    assert.equal(e.retry_after, 7);
    assert.equal(e.code, "quota_exceeded");
    return true;
  });
});

test("ficheros: binario de ida y vuelta, stat, delete, mode", async () => {
  const sb = await Sandbox.create(conn());
  const bin = new Uint8Array(randomBytes(200_000));
  bin[0] = 0; bin[1] = 255; // no es UTF-8
  const st = await sb.files.write("/tmp/a.bin", bin, { mode: 0o600, mkdir: true });
  assert.equal(st.size, bin.length);
  assert.equal(st.mode, "0600");
  assert.match(gw.requests.at(-1)!.url, /mode=0600&mkdir=1/);
  const back = await sb.files.read("/tmp/a.bin");
  assert.ok(Buffer.from(back).equals(Buffer.from(bin)));
  assert.equal((await sb.files.stat("/tmp/a.bin")).size, bin.length);
  await sb.files.write("/tmp/t.txt", "ñandú");
  assert.equal(await sb.files.readText("/tmp/t.txt"), "ñandú");
  await sb.files.delete("/tmp/a.bin");
  await assert.rejects(sb.files.read("/tmp/a.bin"), NotFound);
});

test("ficheros: list de un directorio", async () => {
  const sb = await Sandbox.create(conn());
  await sb.files.write("/d/a.txt", "x", { mkdir: true });
  await sb.files.write("/d/b.txt", "yy");
  await sb.files.write("/otro/c.txt", "z");
  const ls = await sb.files.list("/d");
  assert.match(gw.requests.at(-1)!.url, /list=1/);
  assert.equal(ls.path, "/d");
  assert.deepEqual(ls.entries.map((e) => [e.name, e.type, e.size]), [["a.txt", "file", 1], ["b.txt", "file", 2]]);
});

test("errores: 401, 404, 5xx genérico y red", async () => {
  await assert.rejects(Sandbox.create({ url: gw.url, token: "otro" }), Unauthorized);
  const c = new Client(conn());
  await assert.rejects(c.getSandbox("nope"), NotFound);
  const c2 = new Client({ url: "http://127.0.0.1:1", token: TOKEN, timeout: 2 });
  await assert.rejects(c2.getSandbox("x"), TransportError);
  assert.ok(new ServerError(503, "m") instanceof ApiError);
  assert.throws(() => new Client({ url: "ftp://x" }), ConfigError);
});

test("el token no aparece en repr, inspect, JSON ni mensajes de error", async () => {
  const c = new Client(conn());
  const sb = await Sandbox.create({ client: c });
  for (const s of [String(c), inspect(c), JSON.stringify(c), inspect(sb), String(sb), JSON.stringify(sb)]) {
    assert.ok(!s.includes(TOKEN), s);
  }
  // el servidor devuelve el token dentro del mensaje (peor caso): el SDK lo oculta
  let msg = "";
  try { await new Client({ url: gw.url, token: TOKEN + "x" }).getSandbox("a"); } catch (e: any) { msg = e.message + inspect(e); }
  assert.ok(msg.includes("401") || msg.includes("Unauthorized"));
  assert.ok(!msg.includes(TOKEN + "x"), msg);
  try { await new Client({ url: "http://u:" + TOKEN + "@127.0.0.1:1", token: TOKEN, timeout: 2 }).health(); } catch (e: any) { msg = e.message + inspect(e); }
  assert.ok(!msg.includes(TOKEN), msg);
  assert.ok(!c.url.includes(TOKEN));
});

test("configuración por entorno", async () => {
  const antes = [process.env.KLING_SANDBOX_URL, process.env.KLING_SANDBOX_TOKEN];
  process.env.KLING_SANDBOX_URL = gw.url; process.env.KLING_SANDBOX_TOKEN = TOKEN;
  try { const sb = await Sandbox.create(); assert.ok(sb.id); } finally {
    process.env.KLING_SANDBOX_URL = antes[0]; process.env.KLING_SANDBOX_TOKEN = antes[1];
    if (antes[0] === undefined) delete process.env.KLING_SANDBOX_URL;
    if (antes[1] === undefined) delete process.env.KLING_SANDBOX_TOKEN;
  }
});

test("lote: count, atomic y lote parcial", async () => {
  const lote = await Sandbox.createMany(3, { template: "py", atomic: true, ...conn() });
  assert.equal(lote.length, 3);
  assert.deepEqual(lote.errors, []);
  assert.deepEqual(gw.lastBody, { template: "py", count: 3, atomic: true });
  gw.batchFailFrom = 2;
  const parcial = await Sandbox.createMany(4, conn());
  assert.equal(parcial.length, 2);
  assert.deepEqual(parcial.errors.map((e) => e.index), [2, 3]);
  const antes = gw.sandboxes.size;
  gw.batchFailFrom = 1;
  await assert.rejects(Sandbox.createMany(3, { atomic: true, ...conn() }), ServerError);
  assert.equal(gw.sandboxes.size, antes); // nada queda creado
});

test("create con volúmenes y CRUD de volúmenes", async () => {
  await Sandbox.create({ volumes: [{ name: "datos", mount: "/data", read_only: true }], ...conn() });
  assert.deepEqual(gw.lastBody, { volumes: [{ name: "datos", mount: "/data", read_only: true }] });
  const c = new Client(conn());
  const v = await c.createVolume("datos", { size_mib: 128, host: "h1" });
  assert.deepEqual([v.name, v.size_mib, v.host], ["datos", 128, "h1"]);
  assert.equal((await c.listVolumes()).length, 1);
  assert.equal((await c.getVolume("datos")).name, "datos");
  await c.deleteVolume("datos");
  await assert.rejects(c.getVolume("datos"), NotFound);
});

test("procesos de fondo: start / list / get con wait / signal / delete", async () => {
  const sb = await Sandbox.create(conn());
  const p = await sb.processes.start("sleep 9", { cwd: "/w", env: { A: "1" }, timeout: 30, session: "dev" });
  assert.deepEqual([p.pid, p.state], [1, "running"]);
  assert.deepEqual(gw.lastBody, { cmd: ["sh", "-c", "sleep 9"], dir: "/w", env: ["A=1"], timeout_seconds: 30, session: "dev" });
  assert.equal((await sb.processes.list()).length, 1);
  const fin = await sb.processes.get(p.pid, { wait: 5 });
  assert.deepEqual([fin.state, fin.exit_code], ["exited", 0]);
  assert.match(gw.requests.at(-1)!.url, /\?wait=5$/);
  const q = await sb.processes.start(["x"]);
  assert.equal((await sb.processes.signal(q.pid)).signal, "TERM");
  assert.equal((await sb.processes.kill(q.pid)).signal, "KILL");
  assert.deepEqual(gw.lastBody, { signal: "KILL" });
  await sb.processes.delete(p.pid);
  await assert.rejects(sb.processes.get(p.pid), NotFound);
  await assert.rejects(sb.processes.start([]), (e: any) => e.status === 400);
});

test("procesos de fondo: output con offset y follow, con líneas partidas", async () => {
  const sb = await Sandbox.create(conn());
  const p = await sb.processes.start(["x"]);
  const evs = [];
  for await (const ev of sb.processes.output(p.pid, { follow: true })) evs.push(ev);
  assert.equal(evs.map((e) => e.text).join(""), "hola\nmundo\n");
  assert.deepEqual([evs.at(-1)!.exit, evs.at(-1)!.offset], [0, 11]);
  const resto = [];
  for await (const ev of sb.processes.output(p.pid, { offset: 5 })) resto.push(ev);
  assert.equal(resto.map((e) => e.text).join(""), "mundo\n");
  assert.equal(resto.at(-1)!.exit, null); // sin follow no hay exit
  assert.match(gw.requests.at(-1)!.url, /offset=5$/);
});

test("sesiones: create / exec / list / delete", async () => {
  const sb = await Sandbox.create(conn());
  const se = await sb.sessions.create({ id: "dev", cwd: "/w", env: { A: "1" }, ttl: 60 });
  assert.deepEqual([se.id, se.dir, se.env], ["dev", "/w", ["A=1"]]);
  assert.deepEqual(gw.lastBody, { id: "dev", dir: "/w", env: ["A=1"], ttl_seconds: 60 });
  const r = await sb.sessions.exec("dev", "pwd");
  assert.equal(r.exit_code, 3);
  assert.equal((await sb.sessions.list()).length, 1);
  await sb.sessions.delete("dev");
  await assert.rejects(sb.sessions.get("dev"), NotFound);
  await sb.exec("pwd", { session: "dev" }); // exec normal con sesión
});

test("admin: ciclo de inquilinos", async () => {
  const adm = new Admin(conn());
  const t = await adm.createTenant("ana", { limits: { max_sandboxes: 3 } });
  assert.deepEqual([t.id, t.source, t.read_only, t.limits], ["ana", "store", false, { max_sandboxes: 3 }]);
  assert.equal(t.campo_nuevo, 1); // campos nuevos se toleran
  assert.deepEqual(gw.lastBody, { name: "ana", admin: false, limits: { max_sandboxes: 3 } });
  assert.deepEqual((await adm.listTenants()).map((x) => x.id), ["ana"]);
  assert.equal((await adm.updateTenant("ana", { admin: true })).admin, true);
  assert.deepEqual(gw.lastBody, { admin: true });
  assert.deepEqual((await adm.getTenant("ana")).limits, { max_sandboxes: 3 });
  await adm.deleteTenant("ana");
  await assert.rejects(adm.getTenant("ana"), NotFound);
});

test("admin: claves crear, rotar y revocar", async () => {
  const adm = new Admin(conn());
  await adm.createTenant("ana");
  const k = await adm.createKey("ana");
  assert.match(k.key, new RegExp(`^ksk_${k.id}_`));
  const n = await adm.rotateKey("ana", k.id, { graceMinutes: 5 });
  assert.deepEqual([n.old_key_id, n.old_key_expires], [k.id, "en 5 min"]);
  assert.notEqual(n.key, k.key);
  assert.deepEqual(gw.lastBody, { grace_minutes: 5 });
  await adm.rotateKey("ana", n.id);
  assert.equal(gw.lastBody, null); // sin cuerpo: la gracia por defecto es del gateway
  await assert.rejects(adm.rotateKey("ana", n.id, { graceMinutes: 99999 }), ApiError);
  await adm.revokeKey("ana", n.id);
  const fichas = (await adm.getTenant("ana")).keys;
  assert.equal(fichas.length, 3);
  assert.ok(fichas[1].revoked);
  assert.equal(JSON.stringify(fichas).includes("secreto"), false); // la lectura no trae secretos
  await assert.rejects(adm.revokeKey("ana", "nope"), NotFound);
});

test("admin: la clave no sale en inspect ni en JSON; no-admin es Forbidden", async () => {
  const adm = new Admin(conn());
  await adm.createTenant("ana");
  const k = await adm.createKey("ana");
  assert.equal((inspect(k) + JSON.stringify(k) + String(k)).includes(k.key), false);
  assert.equal(inspect(adm.client).includes(TOKEN), false);
  await assert.rejects(new Admin({ url: gw.url, token: TOKEN_USER }).listTenants(), Forbidden);
});

// ---- superficie añadida en la ronda 6

const limpia = () => { gw.sandboxes.clear(); gw.n = 0; gw.endpoints.clear(); gw.contexts.clear(); };

test("list con filtros y cursor", async () => {
  limpia();
  for (const v of ["v", "v", "w"]) await Sandbox.create({ labels: { k: v }, ...conn() });
  assert.equal((await Sandbox.list({ label: { k: "v" }, ...conn() })).length, 2);
  assert.match(gw.requests.at(-1)!.url, /\?label=k%3Dv$/);
  assert.equal((await Sandbox.list({ state: "paused", ...conn() })).length, 0);
  const pag = await Sandbox.list({ limit: 2, ...conn() });
  assert.deepEqual([pag.length, pag.nextCursor, pag.partial], [2, "sbx-2", "host2"]);
  const resto = await Sandbox.list({ limit: 2, cursor: pag.nextCursor!, ...conn() });
  assert.deepEqual([resto.length, resto.nextCursor], [1, null]);
});

test("create(env) llega al gateway", async () => {
  limpia();
  await Sandbox.create({ env: { A: "1" }, ...conn() });
  assert.deepEqual(gw.createBody.env, { A: "1" });
  await Sandbox.create(conn());
  assert.equal("env" in gw.createBody, false);
});

test("pause, resume y set_egress", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  assert.equal((await sb.pause()).state, "paused");
  assert.equal(sb.info.state, "paused");
  assert.equal((await sb.resume()).state, "running");
  const pol = await sb.setEgress("allowlist", ["a.example.test"]);
  assert.deepEqual([pol.egress, pol.allow_domains, pol.applied], ["allowlist", ["a.example.test"], "live"]);
  assert.deepEqual(gw.lastBody, { egress: "allowlist", allow_domains: ["a.example.test"] });
  assert.equal(gw.requests.at(-1)!.method, "PATCH");
});

test("endpoints create / list / revoke", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  const ep = await sb.endpoints.create(8000, { ttl: 60 });
  assert.deepEqual([ep.port, ep.token, ep.public], [8000, "tok-ep", false]);
  const pub = await sb.endpoints.create(9000, { public: true });
  assert.deepEqual([pub.public, pub.token], [true, undefined]);
  assert.deepEqual((await sb.endpoints.list()).map((e) => e.port), [8000, 9000]);
  await sb.endpoints.revoke(8000);
  assert.deepEqual((await sb.endpoints.list()).map((e) => e.port), [9000]);
  await assert.rejects(sb.endpoints.revoke(8000), NotFound);
});

test("code.run acaba en done y runWait agrega", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  const evs = [];
  for await (const ev of sb.code.run("1+1", { language: "python", timeout: 5 })) evs.push(ev);
  assert.deepEqual(evs.map((e) => e.type), ["stdout", "result", "done"]);
  assert.deepEqual(evs[1].results, { "text/plain": "2" });
  assert.deepEqual(gw.codeBody, { code: "1+1", language: "python", timeout_seconds: 5 });
  const r = await sb.code.runWait("fallo()");
  assert.deepEqual([r.ok, r.stdout, r.error?.ename], [false, "hola\n", "NameError"]);
  const ok = await sb.code.runWait("1+1");
  assert.deepEqual([ok.ok, ok.results["text/plain"]], [true, "2"]);
});

test("contextos de código e interrupt", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  const c = await sb.code.contexts.create({ language: "bash", cwd: "/w" });
  assert.deepEqual([c.language, c.cwd], ["bash", "/w"]);
  assert.deepEqual((await sb.code.contexts.list()).map((x) => x.id), [c.id]);
  assert.equal((await sb.code.contexts.get(c.id)).id, c.id);
  for await (const _ of sb.code.run("x", { context: c.id })) { /* vaciar */ }
  assert.equal(gw.codeBody.context_id, c.id);
  await sb.code.interrupt(c.id);
  assert.deepEqual(gw.interrupted, [c.id]);
  await sb.code.contexts.delete(c.id);
  await assert.rejects(sb.code.contexts.get(c.id), NotFound);
});

test("ficheros: range, move, mkdir, search", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  await sb.files.write("/d/a.log", "0123456789");
  await sb.files.write("/d/b.txt", "x");
  const t = async (o: any) => new TextDecoder().decode(await sb.files.read("/d/a.log", o));
  assert.equal(await t({ range: [2, 5] }), "2345");
  assert.equal(await t({ range: [7, null] }), "789");
  assert.equal(await t({ range: "bytes=-3" }), "789");
  await assert.rejects(t({ range: [50, null] }), (e: any) => e instanceof ApiError && e.status === 416 && e.code === "range_not_satisfiable");
  assert.deepEqual((await sb.files.search("/d", "*.log")).matches, ["/d/a.log"]);
  await sb.files.move("/d/b.txt", "/d/c.txt");
  await assert.rejects(sb.files.stat("/d/b.txt"), NotFound);
  await sb.files.write("/d/b.txt", "y");
  await assert.rejects(sb.files.move("/d/b.txt", "/d/c.txt"), Conflict);
  await sb.files.move("/d/b.txt", "/d/c.txt", { overwrite: true });
  assert.equal(await sb.files.readText("/d/c.txt"), "y");
  assert.equal((await sb.files.mkdir("/d/sub/x")).is_dir, true);
});

test("tar de subida y de bajada", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  const cab = Buffer.alloc(512);
  cab.write("a.txt", 0); cab.write("00000000002", 124);
  const tar = Buffer.concat([cab, Buffer.from("hi"), Buffer.alloc(510), Buffer.alloc(1024)]);
  const r = await sb.files.uploadTar("/w", tar, { mkdir: true });
  assert.equal(r.entries, 1);
  assert.match(gw.requests.at(-1)!.url, /tar=1/);
  assert.equal(await sb.files.readText("/w/a.txt"), "hi");
  const baja = Buffer.from(await sb.files.downloadTar("/w"));
  assert.equal(baja.subarray(0, 5).toString(), "a.txt");
  assert.equal(baja.subarray(512, 514).toString(), "hi");
});

test("ApiError.code y reintento con Retry-After", async () => {
  limpia();
  const sb = await Sandbox.create(conn());
  await assert.rejects(Sandbox.connect("nada", conn()), (e: any) => e instanceof NotFound && e.code === "not_found");
  gw.unavailableNext = 1;
  await assert.rejects(sb.renew(), (e: any) => e instanceof ServerError && e.retry_after === 2 && e.code === "service_unavailable");
  const c = new Client({ ...conn(), retries: 2 });
  const pausas: number[] = [];
  c._dormir = async (s: number) => { pausas.push(s); };
  gw.unavailableNext = 2;
  await c.renewSandbox(sb.id);
  assert.deepEqual(pausas, [2, 2]);
  gw.unavailableNext = 5;
  await assert.rejects(c.renewSandbox(sb.id), ServerError);
  gw.unavailableNext = 0; pausas.length = 0;
  gw.rateLimitNext = 1;
  await c.createSandbox({});
  assert.deepEqual(pausas, [7]);
});

test("URL por defecto 127.0.0.1:8090", () => {
  assert.equal(new Client({ token: "x" }).url, process.env.KLING_SANDBOX_URL ?? "http://127.0.0.1:8090");
});
