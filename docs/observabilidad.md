# Observabilidad del gateway: OTLP y auditoría

El gateway de kling-sandbox (`kling sbx gateway`) puede exportar trazas y métricas a un
colector OpenTelemetry y escribir un registro de auditoría por inquilino. Las dos cosas
están **apagadas por defecto** y, apagadas, no cuestan nada: el handler del frontal se usa
tal cual, sin envoltorio.

No hay SDK de OpenTelemetry: la extensión no tiene dependencias, así que el exportador
(`ext/sandbox/internal/telemetria`) habla OTLP/HTTP con `Content-Type: application/json`
(`/v1/traces` y `/v1/metrics`), que los colectores aceptan en el puerto 4318.

## Trazas y métricas (OTLP)

| Bandera | Qué hace |
|---|---|
| `-otlp-endpoint URL` | URL base del colector (`http://colector:4318`). Por defecto, `OTEL_EXPORTER_OTLP_ENDPOINT`. Vacío = apagado. |
| `-otlp-metrics-interval D` | Cada cuánto se exportan las métricas (30s). |

`OTEL_EXPORTER_OTLP_HEADERS` (`k=v,k2=v2`) se manda como cabeceras en cada envío (token del colector).

**Un span por petición** (tipo SERVER, `service.name=kindling-sandbox-gateway`), salvo `GET /v1/health`
(la sonda de vida). Nombre `MÉTODO plantilla`. Atributos:

- `http.request.method`, `http.route` (plantilla: `/v1/sandboxes/{id}/exec`, nunca el camino
  con ids; lo desconocido es `other`), `http.response.status_code`, `kling.duration_ms`.
- `kling.tenant`: el **nombre** del inquilino (nunca el token ni la clave), si el token abre algo.
- `kling.sandbox`: `host/máquina`, en las rutas de un sandbox. `kling.action`: la acción de la auditoría.
- Estado de error en las respuestas 5xx.

**Propagación W3C.** Si llega `traceparent` válido, el span es su hijo (misma traza, `parentSpanId`
el del cliente) y respeta la bandera `sampled` (con `00` no se exporta, pero la traza se sigue
propagando). Si no, se abre una traza nueva. La respuesta lleva `Traceparent` con el span del gateway,
y el mismo id de traza queda en la auditoría (`trace_id`).

**Hacia los daemons.** El contexto de cada petición lleva la traza, y `telemetria.Transporte` es el
`http.RoundTripper` que la inyecta como `traceparent` en las llamadas salientes (probado contra un
servidor falso). Con `-otlp-endpoint` activo, el gateway instala ese transporte en los clientes de los
hosts (`hosts.Nuevo(..., hosts.ConTransporte(telemetria.Transporte))`, sobre el gancho aditivo
`api.Client.WrapTransport`), así que cada llamada al daemon lleva `traceparent` con el trace-id de la
petición y el span del gateway. El daemon no exporta spans: si llega un `traceparent` bien formado, lo
añade a la línea de log de los errores de petición (`… traceparent=00-<trace>-<span>-01`), para casar
esa línea con la traza. Sin OTLP no se propaga nada.

**Lotes y colector caído.** Los spans van a una cola acotada (2048); un envío cada 5 s o cada 256
spans, con plazo de 5 s. Encolar nunca bloquea: con la cola llena, o si el colector no responde o
contesta no-2xx, los spans se **descartan y se cuentan** (sin reintentos, para no acumular memoria).
Las cuentas salen como métricas propias: `kling_sandbox_otlp_spans_exported_total`,
`kling_sandbox_otlp_spans_dropped_total` y `kling_sandbox_otlp_export_errors_total`. Al apagar el
gateway se intenta vaciar la cola (5 s).

**Métricas.** Cada intervalo se lee lo mismo que un admin vería en `GET /v1/metrics` (todos los
inquilinos) y se envía: gauges como `gauge`, `*_total`, `_sum` y `_count` como sumas monótonas
acumulativas, y los cuantiles del resumen de creación como gauge con la etiqueta `quantile`.
Las series por inquilino llevan el nombre del inquilino: el colector ve datos de todos, así que
protégelo como a un admin.

## Auditoría por inquilino

| Bandera | Qué hace |
|---|---|
| `-audit-dir D` | Directorio del registro (también `KLING_SANDBOX_AUDIT_DIR`). Vacío = apagado. Se crea 0700. |
| `-audit-max-mib N` | Tamaño de rotación (8). |
| `-audit-keep N` | Ficheros rotados que se guardan (5): `audit.jsonl`, `.1`… `.N`. |

El formato sigue el de `credaudit` del núcleo: un JSON por línea, fichero abierto sin seguir enlaces
(`O_NOFOLLOW`), regular y 0600 (se endurece si ya existía con otros permisos), escritura asíncrona
por una goroutine con cola que no bloquea. Si la cola se llena, la línea se pierde y la cuenta viaja en
`dropped` de la siguiente. Lo que cae de la última generación al rotar desaparece.

```json
{"ts":"2026-11-19T10:00:00.123Z","tenant":"ana","action":"exec","sandbox":"lab/m1","result":"ok",
 "status":200,"duration_ms":41,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736",
 "detail":{"argv":["ls","-la"],"argc":2,"dir":"/work","env_names":["PATH"]}}
```

`result` es `ok` (2xx/3xx), `denied` (401/403/429: permiso o cuota) o `error`. Solo se audita lo que
tiene efecto; las lecturas no. Un token que no abre nada no genera línea (no hay inquilino al que
atribuirla).

| `action` | `detail` |
|---|---|
| `sandbox.create` | `template`, `image`, `ttl_seconds`, `mem_mib`, `vcpus`, `egress`, `count`; `sandbox` sale de la respuesta (en lotes, `detail.sandboxes`) |
| `sandbox.delete`, `sandbox.renew`, `shell.open` | — |
| `exec`, `process.start` | `argv` (≤32 args de ≤256 bytes), `argc`, `dir`, `user`, `session`, `env_names` (solo nombres) |
| `file.write` / `file.delete` | `path`, y `bytes` (lo leído del cuerpo) en la escritura |
| `process.signal` / `process.kill` | `pid`, `signal` |
| `session.create` / `session.delete` | `session`, `dir` |
| `endpoint.create` | `port`, `ttl_seconds`, `public` |
| `tenant.*`, `key.create`/`rotate`/`revoke` | `target_tenant`, `key_id` (nunca el valor de la clave) |
| `volume.create`/`delete`, `graph.claim`/`release` | `volume`, `graph` |

**Qué no se escribe nunca:** el token del Authorization, claves de inquilino (ni en la respuesta de
`key.create`), los **valores** de variables de entorno, el contenido de ficheros ni de stdin, ni cuerpos
de respuesta. El argv se trunca y se tapa lo que parece un secreto (`password=…`, `Bearer …`,
`token …`), pero eso es una red de seguridad: un secreto pasado como argumento suelto no se puede
reconocer. Pásalos por stdin, por fichero o por entorno. Si el cuerpo de la petición pasa de 256 KiB,
el detalle lo marca (`body_truncated`) y puede faltar el argv.

**Consulta.** `GET /v1/audit?since=…&limit=…&tenant=…` con cualquier token válido:

- `since`: hora RFC 3339 o duración (`15m` = desde hace 15 minutos). `limit`: 1–10000 (1000).
- Un **admin** ve todos los inquilinos y puede filtrar con `tenant=`. Un **inquilino** ve solo lo suyo
  (si pide otro nombre, se ignora). Sin token, 401.
- Respuesta: `{"events":[…],"truncated":false}` de más antigua a más reciente; con `truncated` pide la
  siguiente página con `since` = el `ts` de la última.

## Cómo está enganchado

Todo va en un envoltorio (`telemetria.Medio`) alrededor del handler del frontal, en
`cmd/kling-sandbox/gateway_obs.go`; `frontal.go` no cambia. Desde fuera no se sabe de quién es un token
(auth lo resuelve dentro) ni se sacan las métricas sin token de admin, así que
`frontal/ganchos.go` expone dos funciones y nada más: `Identificar` (token → nombre y si es admin;
nunca el token) y `MetricasTexto`. El resultado de una acción se deduce del código de estado, y los
argumentos, de los primeros 256 KiB del cuerpo, que se devuelven intactos al handler.

## Helm

```yaml
gateway:
  otlp:
    endpoint: http://otel-collector.observability:4318
    metricsInterval: 15s
    headersSecret: {name: kindling-otlp-headers, key: headers}   # OTEL_EXPORTER_OTLP_HEADERS
  audit:
    path: /var/log/kling-audit
    maxMiB: 16
    keep: 3
    fsGroup: 65532
    persistence: {enabled: true, size: 500Mi}
```

El sistema de ficheros del Pod es de solo lectura, así que `gateway.audit.path` monta un `emptyDir`
(se pierde al reiniciar) o, con `persistence.enabled`, un PVC `<gateway>-audit` (se conserva al borrar el
release). Con PVC no se admite más de una réplica. Ejemplo completo en
`deploy/helm/kindling-sandbox/ci/gateway-observabilidad.yaml`.

## Desglose de cada creación desde un snapshot

Cada `run -from` (y cada copia de plantilla) deja su desglose por fases, en `/metrics` y en la máquina
(`Machine.create`, `kling inspect`; el banco lo lee por API):

- `kling_create_duration_ms`: histograma del total, desde la entrada a `runFromCon` hasta tener la copia lista.
- `kling_create_phase_ms{phase}`: histograma por fase, sin solapes (su suma es el total salvo redondeo):
  `checks`, `disk`, `net`, `persist`, `gate_wait`, `vmm`, `load`, `resume`, `resync`, `start`, `finish`.
- `kling_resync_guest_seconds` y `kling_resync_wake_seconds` (resúmenes `_sum`/`_count`): de qué está hecha la
  espera de `POST /resync` en cada restauración. `guest` es el camino crítico dentro del agente
  (`critical_us` de su respuesta) y `wake` el resto de lo que esperó el daemon: despertar del invitado, ARP,
  TCP y respuesta. Solo cuentan agentes que mandan `critical_us`. Con `KLING_RESYNC_TRACE=1` cada resync deja
  `resync split: connect=… first_byte=… guest_critical=… guest_origin=…` en el log; `first_byte −
  guest_critical` es el despertar. El `/healthz` del agente (con `Accept: application/json`) trae
  `segundos_tiempos_vencidos`: cuántos segundos tiempos de resync pasaron su tope y se dejaron colgados.

`persist` (escribir el estado en disco antes de lanzar el VMM) va ANTES de la puerta de arranque, así que ya
no ocupa un hueco de ella: `gate_wait` es solo la cola. `kling_operation_duration_ms{kind="restore"}` no
cambia de significado: sigue cubriendo solo la carga (desde el lanzamiento del VMM hasta cargar y reanudar).
