# Imágenes solo-amd64 en un anfitrión arm64 (Mac, Graviton, Pi)

Muchas imágenes de Docker Hub siguen saliendo solo para `linux/amd64`. En un Mac
Apple Silicon o en un Linux arm64 kindling corre microVMs arm64, y una imagen así
no tenía variante que importar. Desde esta versión se puede importar **emulada**:
el kernel y el agente del invitado siguen siendo arm64 y cada binario x86-64 de
la imagen pasa por **qemu-user** dentro de la microVM.

```sh
kling image import -arch amd64 some/amd64-only:tag     # emulada (nombre: <ref>-amd64)
kling run -image some/amd64-only:tag -platform linux/amd64 -- uname -m   # x86_64
```

Es **opt-in**: sin `-arch amd64` (o `-platform linux/amd64`) nada cambia, y en un
anfitrión x86 tampoco. Va varias veces más lento en CPU (cifras abajo): sirve
para la herramienta o el servicio que no tiene build arm64, no para cargas
pesadas.

## Cómo funciona

1. **Kernel**: el de invitado arm64 lleva `CONFIG_BINFMT_MISC=y`
   (`scripts/builders/kernel/config-arm64`; `check-kernel-config.sh` lo exige en
   arm64). El x86 no cambia.
2. **Constructor oci** (`cmd/kling/builder_oci.go`, `OCISpec.Emulate`): baja la
   variante `linux/amd64` del índice (el `Pull` de `internal/oci` ya elegía por
   `arch`; no hacía falta tocar `pickPlatform`), mete el agente **del anfitrión**
   (arm64) y, de `internal/imagen/emulador.go`:
   - `/usr/local/lib/kindling/qemu-x86_64-binfmt-P`: el `qemu-x86_64` estático
     del paquete `qemu-user` de Debian 13 (trixie, `1:10.0.13+ds-0+deb13u1`),
     con el sha256 del `.deb` (64 MB, de `deb.debian.org` o, si ya no está, de
     `snapshot.debian.org`) **y** el del binario (14 MB) fijados en el código;
     se comprueba además que es un ELF aarch64. El `.deb` queda en la caché de
     `.deb` del daemon (`cache/debs/<sha256>.deb`): la segunda importación no
     baja nada.
   - `/etc/kindling/emulate` con `amd64`: la marca.
   - Siempre con el **init en Go** (`/sbin/overlay-init` -> `kling-guest`): el
     script de init es `#!/bin/sh` de la imagen, x86, y el kernel no lo puede
     ejecutar antes de que alguien registre qemu.
3. **Agente** (`pkg/guest/binfmt.go`): el init, ya con la raíz definitiva y
   `/proc` montados y antes de lanzar nada de la imagen, monta `binfmt_misc` y
   registra `qemu-x86_64` con la magia y máscara de Debian y las banderas
   **`POCF`**:
   - `F` (fix-binary): el kernel abre el intérprete al registrarlo, así que vale
     en un chroot, en otro espacio de montajes o después de un `pivot_root`, sin
     que la ruta exista allí;
   - `P`: conserva el `argv[0]` original (busybox y compañía lo miran);
   - `C` (y con ella `O`): las credenciales salen del binario emulado, no de
     qemu, así que un setuid x86 (`su`, `sudo`) funciona como en nativo. Son las
     mismas que registra Docker Desktop.

   Un fallo no para el arranque (el agente es nativo y contesta igual): se avisa
   en la consola y el servicio dirá `exec format error`.
4. **Receta**: `built.emulated: true`, `agent_arch` y `qemu_sha256`. La clave de
   reutilización de bases (`agent_sha256`) es `<agente>+qemu:<sha256>`, así que
   una imagen emulada nunca se apila sobre una base nativa ni sobre otra con
   otro qemu, y la identidad de una nativa no cambia.

`-arch amd64` sin emulación sigue existiendo: **`-emulate=false`** construye la
imagen para un anfitrión amd64 (con `KLING_GUEST_AGENT_amd64`), como antes, para
copiarla después con `kling image copy`.

### Lo que no hace (todavía)

- `-isolate`: el qemu se baja durante la construcción y la VM de herramientas no
  tiene red; se rechaza con un error claro.
- `kling compose` con `platform: linux/amd64`: el CLI lo tiene en
  `asegurarImagenDockerPlataforma` (`cmd/kling/run_docker.go`); falta que compose
  lo lea de cada servicio (otro trabajo toca compose a la vez).
- arm64 en un anfitrión amd64: no hay pin de `qemu-aarch64` para x86 (sería otra
  entrada de `qemuPins`).
- `-archive` con emulación sí funciona, pero baja el `.deb` de qemu de la red si
  no está en la caché (el resto de la imagen sigue sin red).

## Cifras (Mac mini M4, macOS 26.5)

`amd64/alpine:3.20` (solo amd64) antes y ahora (medido con el kernel de prueba de HZ 100; con el final, `uname -m` da lo mismo en hv; la emulación no depende de HZ):

```text
$ kling image import amd64/alpine:3.20
error: registry-1.docker.io/amd64/alpine@sha256:0d2a3e86… has no linux/arm64 image
$ kling run -image amd64/alpine:3.20 -platform linux/amd64 -allow-exec
$ kling exec <m> -- uname -m
x86_64
$ kling exec <m> -- cat /proc/sys/fs/binfmt_misc/qemu-x86_64
enabled
interpreter /usr/local/lib/kindling/qemu-x86_64-binfmt-P
flags: POCF
```

Igual en vz y en hv, también en una copia de un dorado (`save` y `run -from`):
la entrada de binfmt_misc vive en la memoria del kernel y viaja con el volcado.

`python:3.12-alpine` nativa (arm64) frente a la misma importada con `-arch amd64`,
1 vCPU, 512 MiB, n=10, p50:

| | vz nativa | vz emulada | hv nativa | hv emulada |
|---|---|---|---|---|
| bucle de 5 M sumas en Python | 0,397 s | 6,221 s (15,7×) | 0,334 s | 5,870 s (17,6×) |
| `kling exec -- true` (pared, host) | 84 ms | 89 ms | 81 ms | 99 ms |
| `kling exec -- python3 -c pass` | 88 ms | 221 ms | 88 ms | 295 ms |

La importación emulada tarda más por el qemu (7,8 s frente a 1,9 s, con el
`.deb` ya en la caché; la primera vez baja además 64 MB). La imagen pesa 14 MB
más.

## Kernel de invitado arm64: binfmt_misc y tics en reposo

El fragmento arm64 añade `BINFMT_MISC` y deja dicho que no hay detector de
bloqueos (`SOFTLOCKUP_DETECTOR`, `DETECT_HUNG_TASK`, `WQ_WATCHDOG`):
`allnoconfig` ya los dejaba fuera, el `.config` resuelto (`olddefconfig`) lo
confirma, y así `build.sh` avisa si alguno vuelve. El watchdog de dispositivos
(`CONFIG_WATCHDOG`) ya estaba fuera en `config-common`. Páginas de 4 KiB como
siempre (`docs/linux-arm64.md`). El kernel del CI de Firecracker (6.1.177, el que
lleva hoy el Mac si no se compila el nuestro) también trae `binfmt_misc`, así
que la emulación funciona con los dos.

**HZ se queda en 250.** Se probó `HZ_100` y se descartó. Interrupciones del
invitado en reposo (`/proc/interrupts` en 60 s, imagen alpine con `sleep` de
servicio, 256 MiB), n=10 máquinas, p50 por segundo:

| | vz 1 vCPU | vz 2 vCPU | hv 1 vCPU | hv 2 vCPU |
|---|---|---|---|---|
| ci (6.1.177, HZ 100) | 1,52 | 3,73 | 1,74 | 3,69 |
| nuestro 6.1.140, HZ 250 (antes) | 0,73 | 1,30 | 0,77 | 1,74 |
| nuestro 6.1.140, HZ 100 (probado) | 0,73 | 1,59 | 0,73 | 1,74 |
| nuestro 6.1.140, HZ 250 + binfmt (final) | 0,75 | 2,06 | 0,72 | 1,75 |

(2 vCPU es ruidoso: min-max 1,2-2,5 en todas las filas de nuestro kernel.)

- En reposo HZ no importa: con `NO_HZ_IDLE` un vCPU dormido ya no recibe el
  tic; lo que queda son temporizadores (`arch_timer`, ~0,6-1,1/s) e IPIs, y las
  diferencias entre 100 y 250 están dentro del ruido (min-max de 2 vCPU 1,1-2,3
  en las dos).
- Con HZ 100 el e2e de Mac (`92-e2e-mac.sh`, vz) falló 2 de 2 en
  «freeze/thaw with a live share»: la escritura en vuelo de un fichero abierto
  en una carpeta compartida recibe `EIO` al volver la sesión FUSE tras el thaw
  (`pkg/guest/fuse_fs.go`, `attach`: lo que estaba en vuelo en la sesión vieja
  recibe EIO). Con HZ 250, y con el kernel del CI, 145/145. Es una carrera del
  agente que HZ solo destapa; queda apuntada, no arreglada aquí.
- La bajada que sí hay, ~2-3× (de ~3,7/s a ~1,3-1,7/s con 2 vCPU), es **usar
  nuestro kernel en vez del del CI**.

Kernel final: `vmlinux-6.1.140-kindling-arm64`, sha256
`549ff41e63e967c3ee3f98a5474b074fcd2100c1e163f4f8da5dca80504cfc9a`, compilado
con `build.sh` en kling-pve (CT 109, gcc-aarch64-linux-gnu 12.2 de Debian 12,
~200 s).

**Dorados**: el sello (`kernel_sha256` de `meta.json` y del volcado) pasa a ser
el del kernel nuevo. Un dorado hecho con el anterior se sigue restaurando (el
kernel va en su memoria): el daemon avisa ("was frozen with a different kernel
than the one installed now") y la copia sigue con el kernel viejo; comprobado en
vz con un dorado del kernel anterior (la copia seguía sin `BINFMT_MISC`). Para
que un dorado tenga binfmt_misc hay que rehacerlo con el kernel nuevo instalado;
uno de una imagen emulada ya se hace con él.

## Rosetta en vz (no implementado)

`Code-Hex/vz v3.7.1` sí expone `VZLinuxRosettaDirectoryShare`
(`NewLinuxRosettaDirectoryShare`, `LinuxRosettaDirectoryShareAvailability`,
`LinuxRosettaDirectoryShareInstallRosetta` y, en macOS 14+, `SetOptions` con
`LinuxRosettaUnixSocketCachingOptions`/`AbstractSocket` para la caché de
traducciones). En este Mac la disponibilidad es `Installed`. Rosetta sería
bastante más rápido que qemu-user (traduce por adelantado y con caché). Lo que
haría falta:

1. **kling-vz**: un `VZVirtioFileSystemDeviceConfiguration` con etiqueta
   `rosetta` y el share de Rosetta, solo para las máquinas de imágenes emuladas
   (la receta lo dice), y un error claro si la disponibilidad no es `Installed`
   (`softwareupdate --install-rosetta`, o `InstallRosetta`).
2. **Kernel de invitado**: `CONFIG_VIRTIO_FS=y` (hoy no está: los shares de
   kindling van por su propio FUSE sobre vsock), además de `FUSE_FS` (ya está) y
   `BINFMT_MISC`.
3. **Agente**: montar `virtiofs` con la etiqueta `rosetta` (p. ej. en
   `/run/rosetta`) y registrar `/run/rosetta/rosetta` en binfmt_misc con la
   magia de x86-64 y la bandera `F` (y `C` como arriba), en lugar del qemu de la
   imagen. Con la caché de macOS 14, además, lanzar `rosettad` en el invitado
   con el socket acordado.
4. **save/restore**: comprobado con una sonda contra el framework en este Mac:
   `ValidateSaveRestoreSupport()` da `true` con el dispositivo de Rosetta en la
   configuración, igual que sin él. Lo que NO está probado es el invitado tras
   restaurar: binfmt_misc con `F` guarda abierto el fichero `rosetta` del
   virtiofs, y habría que ver que la sesión FUSE del dispositivo sobrevive al
   restore (y en otro proceso de kling-vz, en un dorado clonado). Si no, el
   agente tendría que re-registrar tras cada restore (el `resync` es el sitio).
   Y un dorado hecho con Rosetta solo se podría restaurar con Rosetta.

**hv no puede tener Rosetta**: es un servicio de Virtualization.framework, y
kling-hv usa Hypervisor.framework directamente (sin VZ ni sus dispositivos).
Allí, como en Linux arm64 con Firecracker, la vía es qemu-user.
