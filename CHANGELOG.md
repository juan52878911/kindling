# Changelog

Todas las novedades relevantes de kindling. Los binarios pre-compilados están
en [Releases](https://github.com/juan52878911/kindling/releases) para
linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64.

## v0.3.0 — 2026-08-13

Notas completas, con tablas comparativas: [`RELEASE-v0.3.0.md`](RELEASE-v0.3.0.md).

La v0.2.0 hizo kindling instalable, autenticado y con estado. Esta lo hace denso, paralelo
y compartido, y estrena soporte (limitado) para Mac Apple Silicon.

### Novedades

- **Misma herramienta en paralelo.** El gateway crea **réplicas por servicio** bajo demanda
  desde el snapshot dorado (COW); varias sesiones concurrentes ya no las topa el cap de
  sesión del puente.
- **`kling migrate`.** Mueve un MCP a kindling **conservando el nombre de la entrada y de
  las herramientas** (endpoint per-servicio): las skills que lo usaban siguen funcionando
  sin reescribirse.
- **Secretos por sesión vía MMDS**, inyectados en la microVM viva; un snapshot congelado
  nunca lleva secretos dentro.
- **Egress allowlist de dominios** (tercer modo, fail-closed): solo salen los dominios
  declarados, con resolver dinámico DNS→ipset.
- **Cuotas por token/tenant** en el gateway (reparto justo).
- **Devolver la RAM**: `kling squeeze` (balloon) reclama la memoria disponible; `/metrics`
  y `kling top` (PSS) hacen visible el peso real, contando el `mem.file` compartido.
- **Modo proxy HTTP/SSE** en el puente: soporta MCP que no hablan stdio.
- **Auto-detección de capacidades** (navegador/internet/nativo) y Chromium compartido con
  contexto por sesión.
- **zram opt-in** en el host para densificar.
- **Mac Apple Silicon (arm64), compatibilidad limitada.** `make deploy-mac` y binarios
  `kling-darwin-arm64`/`kling-linux-arm64`. Requiere M3+ y virtualización anidada; el
  arranque en frío es ~16 s bajo KVM anidado (vs ~3 s en Linux nativo) y el paralelismo
  práctico ronda ~8 réplicas. Límites y receta en [`docs/mac-arm64.md`](docs/mac-arm64.md).

### Correcciones

- `mcp import` respeta `-cpus` y `defaults.mem_mib`.
- El puente **recicla la sesión más ociosa** al llegar al tope (reconexión limpia en
  servicios de 1 sesión); `-e KEY=VAL` para hornear env que apagan el phone-home (semgrep:
  124 s → ~10 s).
- GET sin sesión a un servicio devuelve **405, no 404** (clientes streamable-HTTP cargan
  el endpoint per-servicio).
- Segador/evict sin perder trabajo en vuelo, reconciliación de rutas pegajosas por vida,
  suelo de `MemFree` y cierre del TOCTOU de memoria. Jailer opt-in en frío/restauración,
  matando VMMs huérfanos. Integridad (sha256) y salud del catálogo.

### Actualizar desde v0.2.0

- Corre `kling images refresh`: el puente trae el modo proxy y la inyección de secretos.
- El paralelismo de la misma herramienta no pide configuración; ajusta las cuotas por
  tenant si repartes un mismo token.
- En Mac: necesitas M3+ y `nested virt` (ver [`docs/mac-arm64.md`](docs/mac-arm64.md)).

## v0.2.0 — 2026-08-11

Notas completas, con tablas comparativas: [`RELEASE-v0.2.0.md`](RELEASE-v0.2.0.md).

La v0.1.0 demostraba que la idea funciona: microVMs que descongelan en milisegundos y un
gateway que las despierta bajo demanda. Esta la hace instalable, autenticada y capaz de
guardar estado.

### Novedades

- **`kling up` y `kling status`.** Instalar deja de ser tres scripts a mano como root: el
  kernel y la imagen base van dentro del binario.
- **El gateway exige token.** Despertar un snapshot es ejecutar código, y el gateway es lo
  único que escucha en red. Se genera solo la primera vez.
- **7 clientes de IA** en `connect -install`, frente a 2.
- **Catálogo oficial**: `kling search` y `kling add` contra `registry.modelcontextprotocol.io`.
- **Volúmenes persistentes**, con journal, hasta cuatro por microVM y compartibles en solo
  lectura: un escritor exclusivo, o cuantos lectores hagan falta.
- **`kling volume populate`**: instala paquetes DENTRO de una microVM desechable en vez de
  como root en el anfitrión.
- **`kling images toolchain`**, **`kling images refresh`** y **`kling images recipe`**.
- **NODE_PATH y PYTHONPATH automáticos** apuntando a los volúmenes que traen paquetes.

### Correcciones

Nueve fallos que solo aparecen metiendo servicios de verdad, entre ellos: `mcp import`
ignoraba `defaults.mem_mib` (la causa real de los timeouts en paralelo que se achacaban al
gateway), los snapshots no guardaban ni su política de red ni su volumen, los volúmenes se
formateaban sin journal, y el segador del gateway congelaba microVMs con trabajo en vuelo.

### Actualizar desde v0.1.0

- El gateway pide token: cópialo con `kling config set gateway.token …`.
- Reimporta los servicios: sus snapshots no guardan la política de red.
- Corre `kling images refresh`: el puente vive dentro de cada imagen.

## v0.1.0 — 2026-08-08

Primera release con binarios distribuidos. Antes de esta versión, `kling` solo
estaba disponible vía `make install` (compilación local).

### Novedades

- **Instalación con una línea** desde releases: `curl -fsSL .../install.sh | sh`.
  Sin Go instalado, sin clonar el repo, sin sudo.
- **Releases multi-plataforma** vía GitHub Actions: binarios pre-compilados para
  Linux (amd64/arm64), macOS (amd64/arm64). El bridge se publica por separado
  porque va dentro de las microVMs (estático, musl-safe).
- **Verificación SHA256** antes de instalar: cada release incluye `SHA256SUMS`
  y el instalador aborta si el checksum no coincide.
- **CLI `--dry-run`** para previsualizar qué se descargará y dónde quedará.

### Arreglos desde la última versión funcional (HEAD)

- **Snapshots stateful ya no rompen los restores.** El daemon ahora expone
  `POST /reset` en el bridge, y `kling mcp import` lo invoca (o espera al
  auto-reset del wrapper HTTP) antes de hacer commit. Sin esto, los snapshots
  dorados se congelaban con el servidor ya inicializado, y al restaurar el
  puerto 8080 nunca abría o el handshake daba 400/406.
- **`everything` (HTTP nativo)** ya funciona end-to-end con el wrapper de
  auto-reset (`/var/run/kling-http-reset-done` persiste en el overlay).
- **`filesystem-mcp` (stdio + bridge)** ya funciona end-to-end: el bridge
  tiene el endpoint `/reset` y `mcpImport` lo invoca tras capturar el catálogo.
- **CLI actualizado detecta las nuevas respuestas del daemon** — antes, un CLI
  viejo podía mostrar mensajes de error engañosos.

### Limitaciones conocidas

- **Windows no soportado.** `internal/machine/manager.go` usa `syscall.Kill`,
  `Setsid`, `Stat_t` que son POSIX. Si necesitas Windows, abre un issue.
- **Solo se publica el CLI para macOS** — el daemon requiere KVM, que en macOS
  no existe fuera de máquinas virtuales con VT-x anidado (que es justamente
  cómo se mide aquí: Proxmox + KVM + Firecracker).
- **El bridge solo se publica para Linux.** Va dentro de microVMs Alpine (musl);
  en macOS no tiene sentido empaquetarlo.