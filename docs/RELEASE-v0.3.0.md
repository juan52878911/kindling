# kindling v0.3.0

~51 commits desde `v0.2.0` (84 desde `v0.1.0`). 109 ficheros, **+20 026 / −468**
líneas, y **cero dependencias externas** — el `go.mod` sigue sin cambiar un byte desde
`v0.1.0`.

La v0.2.0 hizo kindling instalable, autenticado y capaz de guardar estado. Esta lo hace
**denso, paralelo y compartido**: la misma herramienta se puede usar en varias sesiones a
la vez, los servicios devuelven la RAM cuando no trabajan, los secretos entran por sesión
sin quedar horneados en el snapshot, y mover un MCP a kindling ya no obliga a reescribir
las skills que lo usaban. Además estrena **soporte para Mac Apple Silicon** (arm64), con
compatibilidad limitada y honesta sobre sus límites.

---

## Lo que cambia de un vistazo

| | v0.2.0 | v0.3.0 |
|---|---|---|
| Misma herramienta en paralelo | 1 sesión (la topaba el cap del puente) | **réplicas por servicio**: N sesiones concurrentes |
| Migrar un MCP existente | reimportar y reconfigurar clientes | **`kling migrate`**: conserva nombre y herramientas, sin tocar skills |
| Secretos | horneados en imagen/snapshot | **por sesión vía MMDS**, inyectados en máquina viva |
| Salida a internet | on/off | tercer modo: **allowlist de dominios** (fail-closed, DNS→ipset) |
| Reparto entre tenants | — | **cuotas por token/tenant** en el gateway |
| RAM ociosa | retenida | **balloon/`kling squeeze`** la devuelve al host; `/metrics`, `kling top` con PSS |
| Densidad | mem compartido (COW) | + **zram opt-in** en el host |
| Servidores MCP soportados | stdio | + **HTTP/SSE nativo** (modo proxy del puente) |
| Capacidades (navegador/internet/nativo) | a mano | **auto-detección**; Chromium compartido por sesión |
| Plataformas | Linux amd64/arm64 | + **Mac Apple Silicon (arm64)**, compatibilidad limitada |
| Dependencias externas | 0 | **0** |
| Tests | 80 | **145** (tests + benchmarks) |

---

## Novedades

### Paralelismo: la misma herramienta en varias sesiones a la vez

El gateway crea **réplicas por servicio** bajo demanda. Antes, todas las sesiones iban
pegajosas a una única instancia y el paralelismo lo topaba el cap de sesión del puente
(256 MiB → 1 sesión). Ahora, cuando las instancias están llenas, el gateway levanta una
réplica desde el snapshot dorado (COW, comparte el `mem.file`) y rutea cada conversación a
la suya. Reactivo, pegajoso y barato.

### `kling migrate`: mover un MCP sin reescribir las skills

Si tenías una skill apuntando a un MCP y lo pasas a kindling, `kling migrate` **conserva
el nombre de la entrada y los nombres de las herramientas 1:1** (endpoint per-servicio,
no el agregado). La skill sigue funcionando igual; no hay que tocar una línea.

### Secretos por sesión (MMDS)

Los secretos se inyectan en la microVM **viva** por sesión a través de MMDS, en vez de
hornearse en la imagen o quedar atrapados en el snapshot. Un snapshot congelado nunca
lleva secretos dentro.

### Egress allowlist de dominios

Tercer modo de salida a internet, **fail-closed**: solo los dominios declarados salen. Un
resolver dinámico traduce DNS→ipset y siembra el conjunto de forma estática al arrancar.

### Devolver la RAM: balloon, `squeeze`, métricas

`kling squeeze` reclama con virtio-balloon la memoria **disponible** (libre + caché), no
solo la libre. `/metrics` y `kling top` (con PSS real) hacen visible cuánto pesa de verdad
cada microVM, contando el `mem.file` compartido entre copias de un snapshot.

### Más superficie de servicios y capacidades

- **Modo proxy HTTP/SSE** en el puente: soporta servidores MCP que no hablan stdio.
- **Auto-detección de capacidades**: kindling detecta si un MCP necesita navegador,
  internet o binarios nativos y se configura solo. El Chromium se arranca de forma
  perezosa y se comparte con un contexto por sesión.
- **`-e KEY=VAL`** para hornear variables de entorno en la imagen (p. ej. apagar el
  phone-home de un servidor).

### Mac Apple Silicon (arm64) — compatibilidad limitada

kindling corre en un Mac M3+ dentro de una VM Linux arm64 con virtualización anidada
(Lima `vz`). El `Makefile` es arch-paramétrico (`GOARCH`) con atajo `make deploy-mac`, y
las releases ya publican `kling-darwin-arm64` y `kling-linux-arm64`. **Límites honestos**:
el `thaw` es de milisegundos y la RAM se comparte (~40× de ahorro), pero cada réplica
**nueva** arranca en frío en ~16 s bajo KVM anidado (vs ~3 s en Linux/x86 nativo), y el
paralelismo práctico ronda las ~8 réplicas concurrentes antes de que la compuerta de
arranque serialice. Ideal para desarrollo local y evaluación, no para alta concurrencia en
Mac. Receta completa y límites en [`docs/mac-arm64.md`](docs/mac-arm64.md).

---

## Correcciones y robustez

- **`mcp import` respeta `-cpus`** y `defaults.mem_mib` en todos los caminos.
- **Reciclaje de sesión**: al llegar al tope, el puente recicla la sesión más ociosa en
  vez de rechazar la reconexión (servicios de 1 sesión ya reconectan limpio).
- **`phone-home` de semgrep**: bakear `SEMGREP_SEND_METRICS=off` bajó el `initialize` de
  **124 s a ~10 s** (egress `none` dejaba la comprobación de versión colgada ~120 s).
- **GET sin sesión a un servicio** devuelve `405`, no `404`: clientes streamable-HTTP
  (opencode y otros) ya cargan el endpoint per-servicio.
- **Segador y evict** no pierden trabajo en vuelo; reconcilian rutas pegajosas por vida,
  no solo por machineID; el guard cierra el TOCTOU de memoria y respeta un suelo de
  `MemFree`.
- **Jailer opt-in** también en arranque en frío, restauración y descongelado; mata VMMs
  huérfanos.
- **Integridad (sha256) y salud** del catálogo de snapshots; recolección de disco a largo
  plazo.

---

## Actualizar desde v0.2.0

- Corre `kling images refresh`: el puente vive dentro de cada imagen y trae el modo proxy
  y la inyección de secretos por sesión.
- Para usar la misma herramienta en paralelo no hace falta nada: el gateway crea réplicas
  solo. Ajusta las cuotas por tenant si repartes un mismo token.
- En Mac: sigue [`docs/mac-arm64.md`](docs/mac-arm64.md); necesitas M3+ y `nested virt`.

---

*Verificación: `gofmt` + `go vet` + `go test -race` en cada push; 145 tests y benchmarks;
build para linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64.*
