# @kindling/sandbox (SDK de JavaScript/TypeScript)

Cliente de la API `/v1` del gateway de sandboxes de kindling. Node ≥ 18, `fetch` nativo,
**sin dependencias**. El nombre del paquete es provisional.

No hay `tsc` en el entorno de construcción, así que el SDK está escrito en JavaScript ESM
con JSDoc y los tipos van a mano en `src/index.d.ts`; no hay paso de compilación.

## Instalación local

```sh
npm install ./sdks/typescript        # o: npm pack ./sdks/typescript && npm install kindling-sandbox-0.1.0.tgz
```

Configuración por argumentos (`url`, `token`) o por `KLING_SANDBOX_URL` / `KLING_SANDBOX_TOKEN`
(URL por defecto `http://localhost:8088`).

## Ejemplo

```js
import { Sandbox } from "@kindling/sandbox";

const sb = await Sandbox.create({ template: "python", ttl: 300 });
try {
  const r = await sb.exec("echo hola; exit 2", { cwd: "/tmp", env: { A: "1" }, timeout: 10 });
  console.log(r.exit_code, r.stdout, r.stderr);
  for await (const ev of sb.execStream(["ls", "/"])) if (ev.stream) process.stdout.write(ev.data);
  await sb.files.write("/tmp/x.bin", new Uint8Array([0, 255]), { mode: 0o600, mkdir: true });
  console.log(await sb.files.read("/tmp/x.bin"));
} finally { await sb.delete(); }   // o `await using sb = …` (Node ≥ 24 / TS ≥ 5.2)
```

`await using` llama a `[Symbol.asyncDispose]()`, que borra el sandbox e ignora un 404.
Un `cmd` de tipo string se ejecuta con `sh -c`; un array, tal cual. `timeout` y `ttl` van en segundos.
Los campos del resultado conservan los nombres del cable (`exit_code`, `retry_after`);
los métodos son camelCase (`execStream`, con alias `exec_stream`).

## Errores

`SandboxError` → `ConfigError`, `TransportError` (red, plazo, flujo cortado) y `ApiError`
(`status`, `apiMessage`, `code`, `retry_after`) con `BadRequest` 400, `Unauthorized` 401, `Forbidden` 403,
`NotFound` 404, `Conflict` 409, `PayloadTooLarge` 413, `RateLimited` 429, `ServerError` ≥ 500.
Se leen de `{"message","code"}` y de `Retry-After` (segundos o fecha HTTP). El token nunca sale en
`toString`/`inspect`/JSON de `Client` y `Sandbox`, y se sustituye por `<oculto>` si aparece en un mensaje.

## Cobertura frente a `ext/sandbox/api/openapi.yaml`

| Ruta | SDK | Estado |
|---|---|---|
| `GET /v1/health` | `client.health()` | sí (sin token) |
| `GET /v1/hosts`, `/v1/templates`, `/v1/metrics` | `client.hosts()`, `templates()`, `metrics()` | sí |
| `POST/GET /v1/sandboxes` | `Sandbox.create`, `Sandbox.list` (`.partial` = `X-Kindling-Partial`) | sí |
| `GET/DELETE /v1/sandboxes/{id}` | `Sandbox.connect`, `refresh`, `delete`/`kill` | sí |
| `POST …/renew` | `sb.renew(ttl?)` | sí |
| `POST …/exec` (NDJSON) | `sb.execStream` | sí, con líneas partidas entre trozos |
| `POST …/exec?wait=1` | `sb.exec` | sí |
| `GET/PUT/DELETE …/files` | `sb.files.read/readText/write/delete`, `stat` (`stat=1`) | sí |
| `POST …/shell` (`kling-shell/1`) | — | **no implementado**: upgrade a tramas binarias, fuera de alcance |
| `POST /v1/sandboxes` con `count`/`atomic` | `Sandbox.createMany(n, {atomic})` (`.errors` = los que no salieron) | sí |
| `/v1/volumes`, `/v1/volumes/{id}`; `volumes` al crear | `client.createVolume/getVolume/listVolumes/deleteVolume`, `Sandbox.create({volumes})` | sí |
| `/v1/graphs` (GET, POST, GET/DELETE `{id}`) | `client.createGraph/getGraph/listGraphs/deleteGraph` | sí, solo a nivel `Client` |

## Pruebas

```sh
cd sdks/typescript && npm test    # node --test sobre test/sdk.test.ts
```

Los tests (y el servidor falso `test/fake_server.ts`) están en TypeScript y se ejecutan con el
*type stripping* de Node ≥ 22.18, sin compilar; no se comprueban tipos sin `tsc`. Cubren CRUD, exec
en flujo (trozos de 7 bytes que parten líneas y caracteres) y en espera, 429 con `Retry-After`,
binario de ida y vuelta (200 kB), 401/404/red y la ausencia del token en repr y mensajes.
