# kindling en Mac Apple Silicon

kindling corre en un Mac con chip Apple Silicon, pero **no de forma nativa**:
Firecracker es un VMM sobre **Linux/KVM**, y macOS no expone KVM. La solución es
una VM Linux **arm64** intermedia con **virtualización anidada**, y dentro de esa
VM corre el daemon (`kling daemon`) exactamente igual que en cualquier host Linux
con KVM. Tu CLI (`kling`) sigue viviendo en macOS y habla con el daemon por SSH.

```
  macOS (CLI kling) ──ssh──▶ VM Linux arm64 (kling daemon + KVM) ──▶ microVMs Firecracker
```

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
