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
medido) sino el **cold start del servidor MCP en el `initialize`**: en modo stdio el puente
lanza un proceso node por sesión, y bajo KVM anidado eso tarda segundos. Dos palancas
validadas, de más a menos general:

1. **Importa con `-cpu 100`.** El cuello es CPU: el techo por defecto (`cpu_pct=50`, media
   vCPU) estrangula el arranque de node. Subirlo al 100 % de un core casi lo parte por dos
   —medido en `seqthink`: **~16 s → ~7 s** por el camino real del gateway—. El techo viaja
   con el snapshot (`kling mcp import -cpu 100 <servicio>`), así que es **por-servicio y por
   host**: no toca el default de Proxmox. Subir a 2 vCPU **no** ayuda (es estrangulamiento de
   techo, no de paralelismo).

2. **Para servicios STATELESS, constrúyelos en modo http-proxy.** En stdio node arranca por
   sesión; en http-proxy el puente arranca **un** node al bootear y queda **congelado vivo**
   en el snapshot, así que al restaurar no se re-arranca. Medido con `context7` (mismo
   servidor, dos modos): stdio **~25.7 s** el primer `initialize` → http-proxy **~8.4 s** el
   primero y **~2.4 s** en estable. Matiz honesto: congelar node vivo **no** da restore
   instantáneo —V8 se re-calienta al reanudar (~6 s la primera vez)—, pero es 3× mejor en
   frío y ~10× en estable. Solo para servicios sin estado (el hijo node es **compartido**
   entre sesiones): kindling lo auto-detecta como efímero. Construcción:
   `sudo ./scripts/80-mcp-image.sh http <n> -n "<paquete-npm>" -- <bin> --transport http --port 8090`.

Lo que **no** mueve la aguja en Mac: el `keep-warm` de instancias (el restore ya es ~140 ms,
no el cuello) y subir los `cpus` de la VM Lima (ya suele haber margen; el límite es el techo
de CPU **por microVM**, no las pCPUs del host anidado).

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
