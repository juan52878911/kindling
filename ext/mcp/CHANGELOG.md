# Changelog

Novedades de kindling-mcp, la extensión de `kling` que aloja servidores MCP en las
microVMs de [kindling](https://github.com/juan52878911/kindling).

| kindling-mcp | kindling |
|---|---|
| v0.3.x | v0.7.x |
| v0.2.x | v0.7.x |
| v0.1.x | v0.6.x, v0.7.x |

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
