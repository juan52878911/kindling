// Recorre el SDK de TypeScript contra el frontal real (lo lanza TestContratoSDKs, en Go).
// Uso: node contrato.mjs URL TOKEN BASE DOMINIO. BASE es un directorio de esta máquina donde
// el invitado de prueba (un daemon que ejecuta de verdad) deja ficheros. Imprime "CONTRATO OK"
// al final; cualquier fallo termina con código 1 diciendo en qué paso.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { ApiError, BadRequest, Client, Conflict, NotFound, Sandbox } from "../src/index.js";

const [url, token, base, dominio] = process.argv.slice(2);
let paso = "inicio";
const p = (nombre) => { paso = nombre; console.log("paso:", nombre); };
const falla = async (promesa, clase, msg) => {
  try { await promesa; } catch (e) { assert.ok(e instanceof clase, `${msg}: ${e}`); return e; }
  assert.fail(msg);
};

// Un tar de un solo fichero, a mano (sin dependencias).
function tar(nombre, contenido) {
  const cab = Buffer.alloc(512);
  cab.write(nombre, 0);
  cab.write("0000644\0", 100); cab.write("0000000\0", 108); cab.write("0000000\0", 116);
  cab.write(contenido.length.toString(8).padStart(11, "0") + "\0", 124);
  cab.write("00000000000\0", 136);
  cab.write("        ", 148); cab.write("0", 156); cab.write("ustar\0" + "00", 257);
  let suma = 0; for (const b of cab) suma += b;
  cab.write(suma.toString(8).padStart(6, "0") + "\0 ", 148);
  const datos = Buffer.from(contenido);
  return Buffer.concat([cab, datos, Buffer.alloc((512 - (datos.length % 512)) % 512), Buffer.alloc(1024)]);
}

try {
  const kw = { url, token };
  p("create con env, labels y ttl");
  const sb = await Sandbox.create({ image: "base", env: { A: "1" }, labels: { suite: "contrato" }, ttl: 600, ...kw });
  const otro = await Sandbox.create({ image: "base", labels: { suite: "otro" }, ...kw });
  assert.equal(sb.info.state, "running");

  p("list: filtros, limit y cursor");
  assert.deepEqual((await Sandbox.list({ label: { suite: "contrato" }, ...kw })).map((s) => s.id), [sb.id]);
  assert.equal((await Sandbox.list({ state: "running", ...kw })).length, 2);
  const pag = await Sandbox.list({ limit: 1, ...kw });
  assert.equal(pag.length, 1);
  assert.ok(pag.nextCursor, "sin cursor de la página siguiente");
  const sig = await Sandbox.list({ limit: 1, cursor: pag.nextCursor, ...kw });
  assert.equal(sig.length, 1);
  assert.notEqual(sig[0].id, pag[0].id);

  p("connect, renew, pause, resume, setEgress");
  assert.equal((await Sandbox.connect(sb.id, kw)).id, sb.id);
  await sb.renew(900);
  assert.equal((await sb.pause()).state, "paused");
  assert.equal((await sb.resume()).state, "running");
  assert.equal((await sb.setEgress("allowlist", ["a.example.test"])).applied, "live");

  p("endpoints: create, list, revoke");
  const ep = await sb.endpoints.create(3000, { ttl: 300, public: true });
  assert.deepEqual([ep.port, ep.public], [3000, true]);
  assert.ok(ep.url.includes(dominio), ep.url);
  assert.deepEqual((await sb.endpoints.list()).map((e) => e.port), [3000]);
  await sb.endpoints.revoke(3000);
  assert.deepEqual(await sb.endpoints.list(), []);

  p("exec");
  const r = await sb.exec("echo hola; exit 3");
  assert.deepEqual([r.exit_code, r.stdout], [3, "hola\n"]);

  p("ficheros: write, read, range, stat");
  const f = path.join(base, "a.log");
  await sb.files.write(f, "0123456789");
  await sb.files.write(path.join(base, "b.txt"), "x");
  assert.equal(await sb.files.readText(f), "0123456789");
  assert.equal(await sb.files.readText(f, { range: [2, 5] }), "2345");
  assert.equal(await sb.files.readText(f, { range: "bytes=-3" }), "789");
  const e416 = await falla(sb.files.read(f, { range: [50, null] }), ApiError, "un Range fuera del fichero tenía que dar 416");
  assert.deepEqual([e416.status, e416.code], [416, "requested_range_not_satisfiable"]);
  assert.equal((await sb.files.stat(f)).size, 10);

  p("ficheros: search y move");
  assert.deepEqual((await sb.files.search(base, "*.log")).matches, [f]);
  const destino = path.join(base, "c.txt");
  await sb.files.write(destino, "ya existe");
  await falla(sb.files.move(path.join(base, "b.txt"), destino), Conflict, "move sobre un destino que existe tenía que dar 409");
  await sb.files.move(path.join(base, "b.txt"), destino, { overwrite: true });
  assert.equal(await sb.files.readText(destino), "x");
  await falla(sb.files.search(path.join(base, "no-existe"), "*"), NotFound, "search en una raíz inexistente tenía que dar 404");

  p("ficheros: mkdir y tar");
  assert.equal((await sb.files.mkdir("/arbol/x")).is_dir, true);
  assert.equal((await sb.files.uploadTar("/paquete", tar("a.txt", "hi"), { mkdir: true })).entries, 1);
  const baja = Buffer.from(await sb.files.downloadTar("/paquete"));
  assert.equal(baja.subarray(0, 5).toString(), "a.txt");
  assert.equal(baja.subarray(512, 514).toString(), "hi");

  p("ficheros: chmod y replace");
  const conf = path.join(base, "app.conf");
  await sb.files.write(conf, "host=a\nport=1\nport=1\n");
  await sb.files.chmod(conf, 0o600);
  assert.equal((fs.statSync(conf).mode & 0o777).toString(8), "600");
  assert.equal((await sb.files.replace(conf, "host=a", "host=b")).replacements, 1);
  await falla(sb.files.replace(conf, "port=1", "port=2"), Conflict, "un old repetido sin all tenía que dar 409");
  assert.equal((await sb.files.replace(conf, "port=1", "port=2", { all: true })).replacements, 2);
  assert.equal(await sb.files.readText(conf), "host=b\nport=2\nport=2\n");

  p("metadata, metrics y fork");
  assert.deepEqual((await sb.setMetadata({ labels: { fase: "dos" }, remove: ["suite"] })).labels, { fase: "dos" });
  assert.deepEqual((await Sandbox.connect(sb.id, kw)).info.labels, { fase: "dos" });
  await falla(sb.setMetadata({ labels: { tenant: "otro" } }), BadRequest, "una etiqueta reservada tenía que dar 400");
  assert.equal((await sb.metrics()).id, sb.id);
  const copias = await sb.fork({ count: 2, ttl: 120 });
  assert.equal(copias.length, 2);
  assert.equal(copias[0].info.labels.fase, "dos");
  for (const c of copias) await c.delete();

  p("code: contextos, run, runWait, interrupt");
  const ctx = await sb.code.contexts.create({ language: "python" });
  assert.deepEqual((await sb.code.contexts.list()).map((c) => c.id), [ctx.id]);
  assert.equal((await sb.code.contexts.get(ctx.id)).language, "python");
  const evs = [];
  for await (const ev of sb.code.run("x = 21", { context: ctx.id })) evs.push(ev);
  assert.equal(evs.at(-1).type, "done");
  assert.equal((await sb.code.runWait("x * 2", { context: ctx.id })).results["text/plain"], "42");
  const mal = await sb.code.runWait("1/0", { context: ctx.id });
  assert.deepEqual([mal.ok, mal.error.ename], [false, "ZeroDivisionError"]);
  assert.equal((await sb.code.runWait("print(1 + 2)")).stdout, "3\n"); // contexto efímero
  await sb.code.interrupt(ctx.id);
  await sb.code.contexts.delete(ctx.id);
  await falla(sb.code.contexts.get(ctx.id), NotFound, "el contexto borrado tenía que dar 404");

  p("errores con code");
  const e404 = await falla(Sandbox.connect("a/no-existe", kw), NotFound, "tenía que dar 404");
  assert.equal(e404.code, "not_found");
  const e401 = await falla(new Client({ url, token: "mal" }).hosts(), ApiError, "tenía que dar 401");
  assert.deepEqual([e401.status, e401.code], [401, "unauthorized"]);

  p("delete");
  await sb.delete();
  await otro.delete();
  assert.deepEqual([...(await Sandbox.list(kw))], []);
  console.log("CONTRATO OK");
} catch (e) {
  console.log(`FALLO en el paso '${paso}': ${e?.message ?? e}`);
  process.exit(1);
}
