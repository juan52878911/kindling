# Changelog

Novedades de kindling-mcp, la extensión de `kling` que aloja servidores MCP en las
microVMs de [kindling](https://github.com/juan52878911/kindling).

| kindling-mcp | kindling |
|---|---|
| v0.2.x | v0.7.x |
| v0.1.x | v0.6.x, v0.7.x |

## Sin publicar — v0.2.0

- Sube la dependencia a kindling v0.7. El puente embebe el agente de invitado nuevo
  (exec en streaming y ficheros), aunque en una microVM de servicio siguen sin
  existir: solo se encienden con `allow_exec`, y un servicio no lo lleva nunca.

## v0.1.0 — sin publicar

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
