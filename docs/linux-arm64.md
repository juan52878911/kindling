# kindling en Linux arm64

kindling corre igual en un host Linux arm64 con KVM que en uno x86: mismo
daemon, mismo Firecracker, mismas imágenes (construidas para arm64). Esta guía
dice qué hosts sirven, cómo dejarlo andando y qué tener en cuenta para que vaya
bien. Para un Mac, el camino es el backend nativo `vz` ([mac.md](mac.md)).

## Hosts

Firecracker habla con KVM y solo con KVM: el host tiene que ser hierro o una VM
cuyo hipervisor exponga virtualización anidada. En arm64 eso deja fuera a casi
todas las VMs de nube.

| Host | ¿Sirve? | Notas |
|---|---|---|
| AWS Graviton `*.metal` (`c7g.metal`, `m7g.metal`, `c8g.metal-24xl`...) | sí | lo que valida Firecracker; GICv3, páginas de 4 KiB |
| AWS Graviton no-metal (`c7g.large`...) | **no** | no hay `/dev/kvm`: AWS no da (hoy) virtualización anidada en Graviton |
| OCI `BM.Standard.A1.160` (Ampere Altra, hierro) | sí | Neoverse N1, GICv3 |
| OCI `VM.Standard.A1.Flex` (la capa gratuita) | **no** | la VM no expone KVM |
| Ampere Altra / AmpereOne en hierro propio o alquilado | sí | igual que el anterior |
| Raspberry Pi 5 (8 GiB, Raspberry Pi OS de 64 bits) | sí, **con el kernel de 4K** | por defecto arranca `kernel_2712.img`, de 16 KiB; ver abajo. GIC-400 (GICv2) |
| Mac M3 o posterior con Lima y virtualización anidada | solo para probar | funciona ([mac-arm64.md](mac-arm64.md)), pero el backend `vz` nativo es 4-8× mejor en latencia; M1/M2 no tienen anidada |
| Cualquier otra VM arm64 | depende | si `/dev/kvm` existe dentro, sirve |

Lo primero, en cualquier caso:

```sh
getconf PAGESIZE     # 4096; 16384 o 65536 = otro kernel (ver abajo)
ls -l /dev/kvm       # tiene que existir
dmesg | grep -i kvm  # "Hyp mode initialized successfully" = el kernel arrancó en EL2
```

En arm64 KVM va **compilado dentro** del kernel de las distribuciones: no hay
`modprobe kvm` que valga. Si `/dev/kvm` no está, el kernel no arrancó en EL2
(VM sin anidada, o firmware que no la da). `kling up` lo dice así en arm64.

### Raspberry Pi 5: el kernel de 4 KiB

Raspberry Pi OS arranca en la Pi 5 un kernel de páginas de 16 KiB. Firecracker
solo se valida en hosts de 4 KiB, así que `kling up` avisa. El kernel de 4 KiB
viene en el mismo paquete; basta elegirlo en `/boot/firmware/config.txt`:

```ini
kernel=kernel8.img
```

Reinicia y comprueba `getconf PAGESIZE` → `4096`. El invitado de kindling es de
4 KiB siempre, a propósito (`scripts/builders/kernel/config-arm64`).

## Pasos

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
sudo ./scripts/20-install-firecracker.sh            # en una copia del repo
sudo KERNEL_SOURCE=build ./scripts/30-fetch-artifacts.sh   # kernel propio (ver abajo)
sudo ./scripts/70-build-minimal-image.sh            # imagen base "min"
kling up                                            # comprueba e imprime lo que pide sudo
kling try -- uname -m                               # aarch64
kling image import python:3.12-alpine               # una imagen de Docker, arm64
sudo ./scripts/40-bench-boot.sh                     # arranque en frío, snapshot y restore
```

`kling up -check` repasa `/dev/kvm`, la página del host, Firecracker, red y
usuarios, con el remedio de cada fallo.

## Eficiencia

- **Kernel propio** (`KERNEL_SOURCE=build` en `30-fetch-artifacts.sh`, versión
  y sha256 fijados en `scripts/builders/kernel/kernel.pin`): 4 KiB, sin módulos,
  con `CONFIG_DM_VERITY` (el `vmlinux` del CI de Firecracker no lo trae y las
  capas con verity no montan) y las erratas de Neoverse N1/N2/V1/V2, Ampere y
  Cortex-A72/A76. En aarch64 el script lo recomienda si no se elige; el defecto
  sigue siendo el del CI porque compilar pide toolchain y, en una Pi, un rato.
- **Memoria justa**: 128-256 MiB por microVM (`kling run -mem 128M`) bastan para
  casi todo; el `mem_mib` configurado es lo que limita cuántas caben a la vez.
- **Globo**: cada máquina lleva virtio-balloon con informe de páginas libres;
  `kling machine squeeze` devuelve al host lo que el invitado no usa.
- **Node**: `kling mcp add -bundle` empaqueta el servidor en un fichero con
  esbuild. En arm64 el arranque en frío de node es lo caro, y esto lo recorta
  mucho.
- **Pi 5**: dorados y capas en un NVMe (HAT PCIe), no en la microSD: restaurar
  es leer memoria del disco, y la SD no da para eso.

## Snapshots: un dorado no viaja entre CPUs arm64

Un snapshot de Firecracker solo se restaura en **hardware idéntico** al que lo
creó: mismo modelo de CPU y mismo controlador de interrupciones. En arm64 eso
pesa más que en x86:

- **GICv2 y GICv3 no se mezclan.** Un dorado de Graviton o Ampere (GICv3) no se
  restaura en una Pi 5 (GICv2), ni al revés.
- **Ni siquiera entre generaciones de Neoverse.** Graviton2 (N1), Graviton3 (V1)
  y Graviton4 (V2) exponen registros de identificación distintos; Firecracker
  no soporta restaurar entre ellos y, si se intenta, falla o el invitado queda
  inestable.
- Firecracker tiene **plantillas de CPU** para arm64 (`V1N1`, que enseña a un
  Graviton3 como si fuera un Graviton2, y plantillas propias) para igualar lo que
  ve el invitado. kindling no las aplica hoy: cada host congela sus propios
  dorados.

En la práctica: las **imágenes** (ext4 arm64) sí se copian entre hosts arm64
(`kling image copy`); los **dorados** se vuelven a congelar en cada modelo de
CPU, y un parque mixto necesita un dorado por modelo.
