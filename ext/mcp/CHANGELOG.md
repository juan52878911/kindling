# Changelog

Novedades de kindling-mcp, la extensión de `kling` que aloja servidores MCP en las
microVMs de [kindling](https://github.com/juan52878911/kindling).

| kindling-mcp | kindling |
|---|---|
| v0.4.x | v0.9.x |
| v0.3.x | v0.8.x |
| v0.2.x | v0.7.x |
| v0.1.x | v0.6.x, v0.7.x |

## v0.4.0 — 2026-09-23

- **El `initialize` de una sesión nueva se buferea con tope** (8 MiB, el de
  `maxProxyBody`): el invitado es hostil, y una respuesta sin fin llenaba la
  memoria del gateway. Pasado el tope, 502.
- **El gateway acuña sus propios ids de sesión.** Hasta ahora el
  `Mcp-Session-Id` que daba el invitado era la clave del mapa de rutas, y se
  pisaba a ciegas: el smoke test sobre el backend nativo de macOS vio a dos
  réplicas restauradas del mismo snapshot dar el MISMO id (el CSPRNG del
  invitado no se resembraba) y el segundo cliente reapuntó la sesión del
  primero a su microVM. Un invitado hostil podía hacerlo a propósito. Ahora
  cada sesión nueva recibe 128 bits de `crypto/rand` acuñados en el gateway;
  el id del invitado se guarda en la ruta y se traduce en cada petición y en
  cada respuesta (también en las de streaming y en DELETE). Un id desconocido,
  caducado, inventado o de otro servicio recibe `404` sin llegar a ningún
  invitado, y el cliente rehace el `initialize`, como pide el transporte
  Streamable HTTP. Antes se reenviaba al invitado tal cual.
- **DELETE olvida la ruta de la sesión.** Solo se olvidaba cuando la sesión
  era desconocida; las cerradas por el cliente se quedaban en el mapa hasta
  que caducaban.
- **El agregador `_all` no lleva el id de sesión de una instancia a otra**: la
  sesión de detrás se guarda por servicio Y máquina, y una primaria nueva
  recibe su propio `initialize`.
- **Las rutas de control del agente ya no se reenvían** (`/resync`,
  `/volume/*`, `/exec`, `/files`, `/dns`, y `/reset` del puente): un cliente
  con token podía mover el reloj de la microVM, desmontarle los volúmenes o
  cerrar las sesiones de los demás. Usa `guest.IsControlPath` del núcleo.
- El puente embebe `pkg/guest`, así que recompilado contra kindling v0.9.1
  sirve `POST /resync` y cada instancia restaurada tiene reloj y aleatorios
  propios.
- **Funciona sobre el backend nativo de macOS** (`kling-vz`, kindling v0.9).
  Allí todos los invitados comparten la misma IP interna y no es alcanzable
  desde el host: se llega a cada uno por el puerto que reenvía en loopback
  (`api.Machine.Forwards`). El gateway, el agregador, el modo efímero y el
  informe HTML dejan de construir `IP:puerto` a mano y usan `Machine.Addr` /
  `scheduler.Instance.Addr` en su lugar, que en Linux se comporta exactamente
  igual que antes. Requiere kindling v0.9.1 (`BindGuest`, `Route.GuestSID`,
  `guest.IsControlPath`); de momento `go.mod` apunta con `replace` a una copia
  local del núcleo hasta que se publique la etiqueta.

## v0.3.0 — 2026-09-23

- **Un `kling mcp link` recién hecho funciona a la primera.** El gateway guarda
  los enlaces 30 s en caché, y hasta que caducaba la primera petición al
  servicio nuevo acababa en 502 "no snapshot for service". Ahora, antes de dar
  ese error, relee los enlaces. Lo encontró el e2e nuevo.
- Depende de kindling v0.8. El enrutador multi-host reintenta también en otro
  host cuando uno tiene el disco casi lleno (el 503 nuevo del núcleo), además de
  cuando no le cabe en memoria o llegó a su tope de máquinas.
- **`kling gateway` habla con varios daemons a la vez.** Hasta ahora un gateway
  solo podía hablar con UN daemon, así que cada servicio quedaba atado al host
  donde se importó. Con `-hosts nombre=endpoint,nombre2=endpoint2` (o la clave
  de configuración `mcp.hosts`, mismo formato) levanta un `Gateway` completo por
  host y enruta `/mcp/<servicio>` al que lo tiene en su catálogo; si varios lo
  tienen, al que más memoria disponible reporte (`GET /procstats`), con
  reintento automático en el siguiente si ese contesta con falta de memoria o
  tope de máquinas. `/mcp/_all` agrega el catálogo de todos los hosts y reenvía
  cada `call_tool` al dueño del servicio; un host que no contesta no tumba a los
  demás. Sin `-hosts`/`mcp.hosts`, el comportamiento es EXACTAMENTE el de
  siempre: un solo host, el del contexto activo.
- **`scripts/90-e2e.sh`**: la extensión tiene ya su propia prueba de extremo a
  extremo contra un daemon y un gateway reales, con el mismo estilo que la del
  núcleo — catálogo (`kling add` desde el registro), el gateway con y sin
  token, `/mcp/<servicio>` y `/mcp/_all`, `mcp health`/`heal`,
  `refresh-bridge` idempotente y `mcp link`/`unlink` contra un `kling-bridge`
  local.

## v0.2.0 — 2026-09-23

- Sube la dependencia a kindling v0.7. El puente embebe el agente de invitado nuevo
  (exec en streaming y ficheros), aunque en una microVM de servicio siguen sin
  existir: solo se encienden con `allow_exec`, y un servicio no lo lleva nunca.

## v0.1.0 — 2026-09-23

Primera versión por separado. Es la mitad MCP de kindling v0.4/v0.5, sacada del
núcleo sin cambiar lo que se teclea: con kindling-mcp instalado, `kling mcp`,
`kling add`, `kling search`, `kling connect`, `kling gateway`, `kling export`,
`kling memory` y `kling migrate` funcionan como antes.

- **`kling-mcp`**, una extensión de `kling` (ver el protocolo en
  [docs/extensions.md](https://github.com/juan52878911/kindling/blob/main/docs/extensions.md)).
  Declara `min_kling` 0.6.0, los ganchos de `kling status` y las unidades
  `kling-gateway.service` y `kling-heal.timer`, que `kling up` arranca.
- **Solo habla con el daemon por su API**: el catálogo y la salud son las
  anotaciones `mcp.tools` y `mcp.health` de cada snapshot; los servidores
  enlazados, `store/mcp/links`; empaquetar es el constructor `mcp`; poner el
  puente al día, `PUT /images/{name}/files`.
- **`kling mcp refresh-bridge`** sustituye a `kling images refresh`.
- La configuración de memoria vive en `extensions.mcp` (`kling config set
  mcp.memory.enabled true`); kindling mueve sola la sección `memory` antigua.
- Las unidades de systemd llaman a `/usr/local/bin/kling-mcp`. `make deploy`
  instala la extensión, el puente, `80-mcp-image.sh`, el constructor `mcp` y las
  unidades, y genera una vez el token del gateway en `/etc/kling/gateway.env`.
- El historial anterior está en el
  [CHANGELOG de kindling](https://github.com/juan52878911/kindling/blob/main/CHANGELOG.md).
