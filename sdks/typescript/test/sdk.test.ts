import { test, before, after, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { inspect } from "node:util";
import { randomBytes } from "node:crypto";
import { Client, Sandbox, NotFound, Unauthorized, RateLimited, ApiError, ServerError, TransportError, ConfigError } from "../src/index.js";
import { FakeGateway, TOKEN } from "./fake_server.ts";

const gw = new FakeGateway();
before(() => gw.start());
after(() => gw.stop());
beforeEach(() => { gw.batchFailFrom = 0; gw.streamChunks = []; gw.rateLimitNext = 0; gw.requests = []; });
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
