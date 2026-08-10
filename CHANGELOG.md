# Changelog

Todas las novedades relevantes de kindling. Los binarios pre-compilados están
en [Releases](https://github.com/juan52878911/kindling/releases) para
linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64.

## perf-improvements — 2026-08-09

Optimizaciones para llevar el rendimiento al límite sobre la base ya
optimizada de v0.1.0. La rama `perf-improvements` aún no está mergeada —
este changelog documenta el trabajo en curso para revisión.

### Rendimiento medido

Cuellos de botella detectados con pprof y eliminados durante esta ronda:

- `find_tools` (gateway): 11.270 ns/op y 200 allocs → 1.212 ns/op y 0 allocs
  con haystack precomputado y ranking por índices.
- `schedulePersist()` (machine): de 30-50 ms bloqueando bajo el lock
  global a 3,9 ns/op y 0 allocs moviendo el fsync fuera del lock de
  escritura.
- `Manager.List()` (machine): 50 walks por VM → O(1) por máquina con
  `DiskBytes` cacheado en `State`.
- Template overlay (machine): mkfs se ejecuta una sola vez para la primera
  VM; el resto reutiliza el overlay — ahorra 30-50 ms por VM a partir de
  la segunda.

### Cambios estructurales

- `persist()` con debounce de 50 ms y fsync fuera del lock.
- `encodeRPC` con `json.Marshal` y un id único por invocación — además de
  la mejora de velocidad, corrige una condición de carrera latente cuando
  el RPC respondía antes de que el request terminara de serializarse.
- Pool de `http.Client` por socket en fc y `sync.Pool` de `bytes.Buffer`
  para evitar asignaciones en el hot path de los clientes api/fc.
- `bytes.Cut` en lugar de `strings.Split` + trim para parsing de headers
  HTTP.

### Seguridad

- `http.Server` con timeouts `ReadHeader`, `Read`, `Write`, `Idle` — el
  daemon y el gateway ya no son vulnerables a Slowloris.

### Arreglos

- El agregador del gateway trataba enlaces externos (como engram) como
  microVMs internas; ahora los etiqueta y excluye correctamente.
- pprof disponible en el daemon (socket Unix) y en el gateway (opt-in por
  flag) para diagnóstico en caliente.
- `kling-bridge` con shutdown limpio: SIGTERM cierra sesiones y libera el
  socket — antes un `kill` dejaba sesiones huérfanas.
- `pool.fill` con cancel path — los fills cancelados ya no quedaban sin
  procesar.
- `kling stop` con SIGTERM y escalado a SIGKILL tras 500 ms de gracia.

### Tests

Seis archivos de test añadidos con cobertura para todas las optimizaciones
de la ronda: persist debounce, DiskBytes cache, haystack, encodeRPC,
pool de buffers y timeouts del servidor HTTP.

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