# kindling en Mac Apple Silicon

kindling corre en un Mac con chip Apple Silicon, pero **no de forma nativa**:
Firecracker es un VMM sobre **Linux/KVM**, y macOS no expone KVM. La solución es
una VM Linux **arm64** intermedia con **virtualización anidada**, y dentro de esa
VM corre el daemon (`kling daemon`) exactamente igual que en cualquier host Linux
con KVM. Tu CLI (`kling`) sigue viviendo en macOS y habla con el daemon por SSH.

```
  macOS (CLI kling) ──ssh──▶ VM Linux arm64 (kling daemon + KVM) ──▶ microVMs Firecracker
```

## Estado: compatibilidad limitada

El soporte de Mac es **experimental**. Funciona de verdad —el ciclo completo
boot/freeze/thaw/squeeze corre igual que en Linux— pero con límites conocidos que
conviene tener claros antes de apoyarse en él para carga real. Verificado en un Mac
M4 (2026-08).

- **Solo M3 o superior.** La virtualización anidada del framework Virtualization de
  Apple no existe en M1/M2. Sin ella no hay `/dev/kvm` dentro de la VM y Firecracker
  no arranca. No hay rodeo.
- **El cuello de botella en Mac es el arranque, no la RAM.** Descongelar un snapshot
  ya materializado (`thaw`) es de milisegundos, y la memoria compartida por copia-en-
  escritura ahorra ~40× RAM frente a procesos nativos. Pero **cada réplica nueva
  arranca en frío en ~16 s** bajo KVM anidado arm64, frente a ~3 s en un host
  Linux/x86 nativo. La primera vez que se despierta un servicio se paga ese arranque.
- **Paralelismo práctico ~8 réplicas concurrentes** antes de que la compuerta de
  arranque (`KLING_MAX_PARALLEL_BOOT`, por defecto 2) serialice las ráfagas. Encender
  muchas microVMs a la vez puede **colgar la VM anidada** con pocos vCPU; por eso la
  compuerta es conservadora en este entorno.
- **Bueno para desarrollo local y evaluación**, no para alta concurrencia en Mac. Para
  producción de carga, un host Linux con KVM nativo (o la VM arm64 con más vCPU) rinde
  bastante mejor. Hay optimización en curso (mantener réplicas calientes para sacar el
  arranque del camino crítico); ver el historial de rendimiento del proyecto.

## Acelerar el arranque (medido en M4)

El cuello del arranque en Mac **no es el restore** (el `Resume` del snapshot son ~140 ms,
medido) sino el **cold start del servidor MCP en el `initialize`**. Y ese cold start **no es
cómputo**: el mismo servidor arranca en **~91-276 ms nativo** (chroot arm64) pero **7-16 s
dentro de la microVM**. La diferencia es que arrancar node es una tormenta de `stat`/`open`/
`mmap`/page-faults al cargar cientos de ficheros de `node_modules`, y bajo **KVM anidado
arm64** (macOS → VM Linux → Firecracker) cada uno se amplifica **~25-60×**. Un MCP mínimo de
un solo fichero arranca en ~1 s en la microVM; los ~6 s extra de un servidor real son la
carga de sus dependencias. Tres palancas validadas, de más a menos impacto:

1. **Empaqueta el servidor: `-bundle`.** Es la palanca mayor para servidores node. Colapsa
   `node_modules` (cientos/miles de ficheros) en **uno** con esbuild al construir la imagen,
   matando la tormenta de ficheros. Medido en `seqthink` (**1205 ficheros → 1**): el
   `initialize` en frío cae de **~7 s a ~2.5 s** (2.9×). Desde el registro:
   `kling add <servidor> -bundle`. A mano:
   `sudo ./scripts/80-mcp-image.sh stdio <n> -n "<paquete-npm>" -bundle -- <bin>`.

2. **Importa con `-cpu-pct 100`.** El techo por defecto (`cpu_pct=50`, media vCPU) estrangula el
   arranque; subirlo al 100 % de un core lo parte por ~2 (`seqthink` **~16 s → ~7 s** por el
   gateway). Viaja con el snapshot (`kling mcp import -cpu-pct 100 <servicio>`): **por-servicio y
   por-host**, no toca Proxmox. **Se acumula con el bundle.** Subir a 2 vCPU no ayuda (es el
   techo, no el paralelismo). En x86/Proxmox también rinde (~2×) y no penaliza bajo carga.

3. **Para servicios STATELESS, constrúyelos en modo http-proxy.** El puente arranca **un**
   node al bootear, **congelado vivo** en el snapshot → al restaurar no se re-arranca.
   `context7` (mismo servidor): stdio **~25.7 s** el 1er `initialize` → http-proxy **~8.4 s**
   el 1º y **~2.4 s** estable. Matiz: congelar node vivo no da restore instantáneo (V8 se
   re-calienta ~6 s la 1ª vez). Solo sin estado (el node es compartido). Construcción:
   `...80-mcp-image.sh http <n> -n "<npm>" -bundle -- <bin> --transport http --port 8090`.

Lo que **no** mueve la aguja en Mac (medido / razonado): el `keep-warm` de instancias y subir
los `cpus` de Lima (el restore ya es ~140 ms); **prefaultar el `mem.file`** (los ~6 s no son
page cache de Lima sino faults stage-2); **`NODE_COMPILE_CACHE`** (el compile es ms, no el
cuello); y el **rate limiter de disco** (capa bytes/s, pero el cuello es el *número* de ops,
que el bundle ya colapsa).

## Requisitos honestos

- **Chip M3 o superior.** La virtualización anidada en el framework Virtualization
  de Apple **solo existe desde M3**. En **M1 y M2 no hay nested virt**: KVM no
  arranca dentro de la VM y Firecracker no puede correr. No hay rodeo.
- **macOS 15 (Sequoia) o superior**, que es donde `vz` expone `nestedVirtualization`.
- **[Lima](https://lima-vm.io/)** para crear y manejar la VM (`brew install lima`).
- Un **Mac arm64**: la VM Linux es arm64 y los binarios del daemon se compilan para
  `linux/arm64` (ver más abajo).

## 1. Crear la VM Linux arm64 (Lima, vz + nested)

Lima usa el backend `vz` (framework Virtualization de Apple) y admite activar la
virtualización anidada. Un `lima.yaml` mínimo:

```yaml
# kindling-arm.yaml
vmType: "vz"
arch: "aarch64"
rosetta:
  enabled: false
# nested virt: imprescindible para KVM dentro de la VM (solo M3+ lo soporta)
nestedVirtualization: true
images:
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
    arch: "aarch64"
cpus: 4
memory: "8GiB"
disk: "40GiB"
mounts:
  - location: "~"
    writable: false
```

Arráncala:

```sh
limactl start --name=kindling-arm ./kindling-arm.yaml
```

Comprueba que KVM está disponible **dentro** de la VM (esto es lo que M1/M2 no dan):

```sh
limactl shell kindling-arm -- test -e /dev/kvm && echo "KVM OK"
```

Si `/dev/kvm` no existe, o el chip no es M3+ o falta `nestedVirtualization: true`.

Averigua el destino SSH para los despliegues:

```sh
limactl show-ssh kindling-arm          # imprime user@host y puerto
# o mira el config directamente:
cat ~/.lima/kindling-arm/ssh.config
```

## 2. Desplegar el daemon arm64 desde el Mac

El `Makefile` es paramétrico por arquitectura con la variable `GOARCH` (por
defecto `amd64`). Para producir binarios `linux/arm64` y desplegarlos:

```sh
# atajo (fija GOARCH=arm64 por ti):
make deploy-mac HOST=ssh://user@127.0.0.1:60022

# equivalente explícito:
make deploy GOARCH=arm64 HOST=ssh://user@127.0.0.1:60022
```

Esto compila `kling-linux-arm64` y `kling-bridge` para arm64, los copia por SSH e
instala el servicio systemd, igual que en amd64. Usa el `user@host:puerto` que te
dio `limactl show-ssh`.

## 3. Dejar el runtime listo dentro de la VM

Los scripts de provisión ya son **arch-aware** (`uname -m` resuelve `aarch64`
solo), así que se corren sin cambios dentro de la VM. Lo que `kling up` reporta
como faltante se resuelve con:

```sh
limactl shell kindling-arm

# dentro de la VM:
kling up                              # dice qué falta (KVM, nft, usuario, artefactos)
sudo ./scripts/20-install-firecracker.sh   # Firecracker + jailer (release aarch64)
sudo ./scripts/30-fetch-artifacts.sh       # kernel vmlinux + rootfs (aarch64)
sudo ./scripts/70-build-minimal-image.sh   # imagen base mínima (Alpine arm64)
```

Instala el kernel donde el daemon lo busca:

```sh
sudo install -D -m644 /opt/fc/vmlinux /var/lib/kindling/images/vmlinux
```

Arranca (o reinicia) el daemon y verifica:

```sh
sudo systemctl restart kling
systemctl is-active kling
kling up                              # ya sin avisos
```

## 4. Probar el ciclo completo

Desde el Mac, apunta el CLI a la VM y prueba boot/freeze/thaw/squeeze:

```sh
kling context add mac-arm ssh://user@127.0.0.1:60022
kling status
# arranca un MCP, congélalo, descongélalo... el ciclo funciona igual que en amd64.
```

## Notas

- Las **releases** ya publican `kling-linux-arm64` y `kling-bridge-linux-arm64`,
  así que también puedes instalar dentro de la VM con `scripts/install.sh`
  (`--bridge`) en vez de compilar desde el Mac.
- `daemon-full` (kernel + imagen embebidos) requiere blobs de la **misma
  arquitectura**: para arm64 hay que generarlos con los scripts 30/70 en un host
  arm64. El camino recomendado en Mac es `make deploy-mac` + scripts dentro de la
  VM, no embeber.
