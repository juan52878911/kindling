// Cada operación de ext/sandbox/api/openapi.yaml tiene un método en el SDK o figura en
// EXCLUIDAS; si no, el test falla. También genera la tabla de cobertura del README (entre
// las marcas `coverage`): KLING_UPDATE_README=1 npm test la reescribe; sin esa variable el
// test falla si el README no la tiene al día.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import * as sdk from "../src/index.js";

const SPEC = fileURLToPath(new URL("../../../ext/sandbox/api/openapi.yaml", import.meta.url));
const README = fileURLToPath(new URL("../README.md", import.meta.url));

/** Operaciones que el SDK no cubre a propósito, con el motivo. */
const EXCLUIDAS: Record<string, string> = {
  "POST /v1/sandboxes/{id}/shell": "interactive shell: upgrade to binary frames (kling-shell/1), out of scope",
};

/** operación -> "Clase.método" del SDK de TypeScript. */
const METODOS: Record<string, string> = {
  "GET /v1/health": "Client.health",
  "GET /v1/hosts": "Client.hosts",
  "GET /v1/metrics": "Client.metrics",
  "GET /v1/templates": "Client.templates",
  "GET /v1/sandboxes": "Sandbox.list",
  "POST /v1/sandboxes": "Sandbox.create",
  "GET /v1/sandboxes/{id}": "Sandbox.connect",
  "DELETE /v1/sandboxes/{id}": "Sandbox.delete",
  "POST /v1/sandboxes/{id}/renew": "Sandbox.renew",
  "POST /v1/sandboxes/{id}/pause": "Sandbox.pause",
  "POST /v1/sandboxes/{id}/resume": "Sandbox.resume",
  "PATCH /v1/sandboxes/{id}/egress": "Sandbox.setEgress",
  "POST /v1/sandboxes/{id}/endpoints": "Endpoints.create",
  "GET /v1/sandboxes/{id}/endpoints": "Endpoints.list",
  "DELETE /v1/sandboxes/{id}/endpoints/{port}": "Endpoints.revoke",
  "POST /v1/sandboxes/{id}/exec": "Sandbox.exec",
  "POST /v1/sandboxes/{id}/code": "Code.run",
  "POST /v1/sandboxes/{id}/code/contexts": "CodeContexts.create",
  "GET /v1/sandboxes/{id}/code/contexts": "CodeContexts.list",
  "GET /v1/sandboxes/{id}/code/contexts/{cid}": "CodeContexts.get",
  "DELETE /v1/sandboxes/{id}/code/contexts/{cid}": "CodeContexts.delete",
  "DELETE /v1/sandboxes/{id}/code/contexts/{cid}/execution": "Code.interrupt",
  "POST /v1/sandboxes/{id}/processes": "Processes.start",
  "GET /v1/sandboxes/{id}/processes": "Processes.list",
  "GET /v1/sandboxes/{id}/processes/{pid}": "Processes.get",
  "DELETE /v1/sandboxes/{id}/processes/{pid}": "Processes.delete",
  "POST /v1/sandboxes/{id}/processes/{pid}/signal": "Processes.signal",
  "GET /v1/sandboxes/{id}/processes/{pid}/output": "Processes.output",
  "POST /v1/sandboxes/{id}/sessions": "Sessions.create",
  "GET /v1/sandboxes/{id}/sessions": "Sessions.list",
  "GET /v1/sandboxes/{id}/sessions/{sid}": "Sessions.get",
  "DELETE /v1/sandboxes/{id}/sessions/{sid}": "Sessions.delete",
  "GET /v1/sandboxes/{id}/files": "Files.read",
  "PUT /v1/sandboxes/{id}/files": "Files.write",
  "DELETE /v1/sandboxes/{id}/files": "Files.delete",
  "POST /v1/sandboxes/{id}/files/move": "Files.move",
  "POST /v1/sandboxes/{id}/files/mkdir": "Files.mkdir",
  "GET /v1/sandboxes/{id}/files/search": "Files.search",
  "GET /v1/volumes": "Client.listVolumes",
  "POST /v1/volumes": "Client.createVolume",
  "GET /v1/volumes/{id}": "Client.getVolume",
  "DELETE /v1/volumes/{id}": "Client.deleteVolume",
  "GET /v1/graphs": "Client.listGraphs",
  "POST /v1/graphs": "Client.createGraph",
  "GET /v1/graphs/{id}": "Client.getGraph",
  "DELETE /v1/graphs/{id}": "Client.deleteGraph",
  "GET /v1/tenants": "Admin.listTenants",
  "POST /v1/tenants": "Admin.createTenant",
  "GET /v1/tenants/{id}": "Admin.getTenant",
  "PATCH /v1/tenants/{id}": "Admin.updateTenant",
  "DELETE /v1/tenants/{id}": "Admin.deleteTenant",
  "POST /v1/tenants/{id}/keys": "Admin.createKey",
  "POST /v1/tenants/{id}/keys/{kid}/rotate": "Admin.rotateKey",
  "DELETE /v1/tenants/{id}/keys/{kid}": "Admin.revokeKey",
};

/** Operaciones de la spec, en orden, con un parser mínimo (sin dependencias). */
function operaciones(): string[] {
  const texto = readFileSync(SPEC, "utf8");
  const paths = texto.split("\npaths:")[1].split("\ncomponents:")[0];
  let ruta = "";
  const ops: string[] = [];
  for (const linea of paths.split("\n")) {
    let m = /^ {2}(\/\S+):\s*$/.exec(linea);
    if (m) { ruta = m[1]; continue; }
    m = /^ {4}(get|post|put|delete|patch):\s*$/.exec(linea);
    if (m && ruta) ops.push(`${m[1].toUpperCase()} ${ruta}`);
  }
  return ops;
}

function tabla(): string {
  const filas = ["| Operation | SDK |", "|---|---|"];
  for (const op of operaciones()) {
    const sdkTxt = op in EXCLUIDAS ? `not covered: ${EXCLUIDAS[op]}` : `\`${METODOS[op]}()\``;
    filas.push(`| \`${op}\` | ${sdkTxt} |`);
  }
  return filas.join("\n");
}

test("toda operación de la spec tiene método en el SDK o exclusión", () => {
  const ops = operaciones();
  assert.ok(ops.length > 40, "el parser no leyó la spec");
  assert.deepEqual(ops.filter((o) => !(o in METODOS) && !(o in EXCLUIDAS)), [], "operaciones sin método ni exclusión");
  assert.deepEqual([...Object.keys(METODOS), ...Object.keys(EXCLUIDAS)].filter((o) => !ops.includes(o)), [], "entradas que ya no están en la spec");
});

test("los métodos del mapa existen", () => {
  const clases = sdk as unknown as Record<string, { prototype: object }>; // los estáticos (Sandbox.create) cuelgan de la clase
  for (const [op, ref] of Object.entries(METODOS)) {
    const [clase, metodo] = ref.split(".");
    assert.ok(clases[clase], `${op}: falta la clase ${clase}`);
    assert.ok(Object.getOwnPropertyNames(clases[clase].prototype).includes(metodo) || Object.getOwnPropertyNames(clases[clase]).includes(metodo), `${op}: falta ${ref}`);
  }
});

test("el README tiene la tabla de cobertura al día", () => {
  const readme = readFileSync(README, "utf8");
  const re = /(<!-- coverage:begin -->\n)([\s\S]*?)(\n<!-- coverage:end -->)/;
  assert.match(readme, re, "faltan las marcas coverage en el README");
  if (process.env.KLING_UPDATE_README === "1") {
    writeFileSync(README, readme.replace(re, (_m, a, _b, c) => a + tabla() + c));
    return;
  }
  assert.equal(re.exec(readme)![2], tabla(), "tabla vieja: KLING_UPDATE_README=1 npm test");
});
