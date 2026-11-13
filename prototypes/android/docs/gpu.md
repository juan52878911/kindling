# GPU para el prototipo Android: qué se puede en el Mac y en Proxmox

Medido el 2026-09-28 en un MacBook Air M4 (16 GiB, macOS 26.5) y en el
Proxmox `pve` (i7-8700T, 15 GiB, Intel UHD 630). Resumen:

| | Mac (`vz`) | Proxmox (UHD 630) |
|---|---|---|
| Aceleración 3D para Android | **no existe**: virtio-gpu de Apple es 2D para Linux | **sí**, Redroid `gpu_mode=host` sobre `/dev/dri/renderD128` (Mesa iris) |
| Ver el teléfono sin VNC | **sí**: ventana nativa + capturas en 11–20 ms, a ~14 fps, y sobrevive a save/restore | con contenedor: el VNC/scrcpy de Redroid (no probado aquí) |
| Coste | +12 MiB por VM, restaurar igual | ~580 MiB de RAM y ~0,14 núcleos por teléfono en UI |
| Con frontera de VM (2026-11-12) | — | **sí**: la imagen de siempre en QEMU `microvm` con virtio-gpu (virgl): UI a 60 fps con 0,25 núcleos, WebGL 60 fps; ~2,2 GiB por teléfono y sin dorado ([prueba de concepto](#prueba-de-concepto-un-teléfono-en-una-vm-con-virgl-issue-95)) |

## Mac: lo que da Virtualization.framework

`VZVirtioGraphicsDeviceConfiguration` es lo único gráfico para invitados Linux,
y **es solo 2D**. Comprobado dentro del invitado, con el kernel de este perfil:

```
[drm] features: -virgl -edid -resource_blob -host_visible
[drm] features: -context_init
[drm] number of scanouts: 1
[drm] number of cap sets: 0
```

Sin `virgl` ni `context_init` y con cero capsets no hay nada que Mesa
(`virtio_gpu_dri`, `vulkan.virtio`/venus, que la imagen de Redroid sí trae)
pueda usar: `gpu_mode=host` no tiene GPU debajo. **En el Mac Android sigue
pintando por CPU (SwiftShader).** Teclado y puntero: vz solo los ofrece como
USB para Linux, y el kernel de kindling no lleva USB; la entrada sigue siendo
`input tap/swipe` por `kling exec`.

Lo que sí sirve es **ver el teléfono de forma nativa**, y funciona con
save/restore.

### Cómo se usa

1. **Kernel** con el perfil gpu (en Lima, igual que el normal):
   ```sh
   ANDROID_PROFILE=gpu KERNEL_SHA256=... OUT=/var/tmp/android-kernel-gpu \
     prototypes/android/kernel/build.sh      # → vmlinux-6.1.140-kindling-arm64-android-gpu
   ```
   El perfil es [`kernel/config-android-gpu`](../kernel/config-android-gpu):
   `DRM`, `DRM_VIRTIO_GPU`, `DRM_FBDEV_EMULATION`, `FB`, sin fbcon. El
   comprobador del núcleo sigue prohibiendo DRM/FB a todo invitado: `build.sh`
   le pasa el `.config` sin esas dos líneas y
   [`check-android-gpu-config.sh`](../kernel/check-android-gpu-config.sh)
   exige lo del perfil y ningún otro driver DRM.
2. **kling-vz** de esta rama y un daemon privado con el kernel gpu en
   `images/vmlinux`. La pantalla se activa con el entorno del daemon (lo hereda
   `kling-vz`):
   ```sh
   KLING_VMM=/ruta/kling-vz KLING_VZ_GRAPHICS=720x1280 KLING_VZ_WINDOW=1 \
     kling daemon -root ~/.kindling-android-gpu -socket ~/.kindling-android-gpu/kling.sock
   ```
   - `KLING_VZ_GRAPHICS=ANCHOxALTO`: un `VZVirtioGraphicsDevice` con un
     scanout de ese tamaño en las máquinas que **arrancan**. Se guarda en el
     JSON del snapshot (`"graphics"`), así que un dorado con pantalla restaura
     con pantalla aunque el daemon ya no tenga la variable (el framework exige
     los mismos dispositivos al restaurar). Un kling-vz anterior ignoraría el
     campo y la restauración fallaría.
   - `KLING_VZ_WINDOW=1`: una ventana por máquina con su
     `VZVirtualMachineView` (AppKit en el hilo principal de `kling-vz`). Cerrar
     la ventana la minimiza: la máquina es del daemon, no de la ventana.
   - `GET /kling/screenshot` en el socket de la API de la máquina
     (`machines/<id>/fc.sock`) devuelve la ventana en PNG:
     `curl --unix-socket fc.sock http://x/kling/screenshot > p.png`. Sale al
     tamaño de la vista (573×1018 en esta pantalla), no a 720×1280.
3. **El espejo** dentro de la VM. Redroid compone en una pantalla virtual
   (`hwcomposer.redroid`) y no pinta en ningún KMS; sin esto la ventana queda
   en negro. [`gpu/android-mirror`](../gpu/android-mirror) lanza
   `screenrecord --output-format=raw-frames` (RGB888 crudo, solo cuando la
   pantalla cambia) y [`gpu/fbmirror`](../gpu/fbmirror/main.go) lo copia a
   `/dev/fb0`:
   ```sh
   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o fbmirror ./prototypes/android/gpu/fbmirror
   kling cp fbmirror <m>:/usr/local/bin/fbmirror
   kling cp prototypes/android/gpu/android-mirror <m>:/usr/local/bin/android-mirror
   kling exec <m> -- android-mirror start        # o: once 10 (mide fps)
   ```
   No va en la imagen (`build-image.sh` no es de este cambio); con `kling cp`
   basta, y queda dentro del dorado si se guarda después.

### Cifras (Android 13, 2 vCPU, 1,5 GiB, `cpu-pct 200`)

| | sin pantalla | con pantalla + ventana |
|---|---|---|
| `boot_completed` en frío | 5,6 s | 5,6 s |
| footprint en frío (`kling top`) | 1584 MiB | 1596 MiB |
| `kling save` | 1,14 s | 1,2 s |
| restaurar (kling-vz), 3 veces | 604 / 573 / 1260 ms | 654 / 663 / 1027 ms |
| footprint tras restaurar | 2137 / 2146 / 2060 MiB | 2039 / 2037 / 2037 MiB |

- La pantalla **no cuesta memoria** apreciable (+12 MiB en frío; tras restaurar,
  dentro del ruido) ni tiempo de restaurar (±50–90 ms, menos que la variación
  entre repeticiones con el Mac en swap).
- **save/restore funciona** con la pantalla: `ValidateSaveRestoreSupport` no
  se queja, la ventana vuelve a abrirse al restaurar y el framebuffer vuelve
  con lo que tenía al guardar (la primera captura tras restaurar enseñaba
  Ajustes, lo último que pintó el espejo antes de `kling save`).
- **Espejo**: 13,8–14,8 fps durante un scroll continuo en Ajustes (4 medidas),
  la VM pasa de ~10 % a 19–27 % de CPU (de 2 vCPU). El techo es
  `screenrecord` leyendo de SwiftShader, no el framebuffer: a `/dev/null`, sin
  fbmirror, da el mismo ritmo (138–143 fotogramas de 720×1280 en 10 s).
- **Captura** por `GET /kling/screenshot`: 11–20 ms y ~0,1–0,5 MB de PNG, frente
  a 0,27 s de `screencap -p` por `kling exec`. Solo con ventana.
- **Sandbox**: con pantalla el framework emite una extensión de IOSurface para
  su proceso auxiliar; el perfil de `kling-vz` la permite solo si la máquina
  tiene pantalla (`GFX=1` → `generic-issue-extension`). Sin eso, crear la VM
  falla con "Failed to issue IOSurface sandbox extension".

### Lo que no está resuelto (y un aviso)

- **Por máquina de verdad**: hoy la pantalla la decide el entorno del daemon
  (y el snapshot). Que sea por máquina pide al núcleo una opción (`kling run
  -graphics 720x1280` o una etiqueta de la receta) que llegue a kling-vz, p. ej.
  como `PUT /kling/graphics` antes de `InstanceStart`.
- **Entrada desde la ventana**: no hay teclado/puntero (USB en vz, sin USB en el
  kernel). Ratón en la ventana → `input tap` pasaría por un canal propio.
- **Corrupción de memoria del invitado con el Mac saturado** (swap 8–12 GiB
  de 9–13): varias VMs acabaron con procesos que mueren con SIGSEGV, un
  `libz.so` ilegible, system_server abortando al leer su XML de `/data` y
  `kling-guest` con la pila rota (kernel panic por muerte de PID 1). Pasó
  **también sin pantalla** (una VM de control sin dispositivo gráfico ni
  espejo dio `unsupported version 0 of Verneed record` al arrancar), así que
  no se atribuye a esto; en una misma sesión el md5 de
  `NotoSansCJK-Regular.ttc` cambió tras restaurar. Con el Mac holgado las
  mismas pruebas pasan limpias. Las cifras de arriba son de pasadas en que el
  md5 y los 40 `exec` de control salieron bien.

## Proxmox: aceleración real con la UHD 630

Medido en un CT LXC privilegiado desechable (id 190, 4 núcleos, 6 GiB, ya
destruido) con Docker y `redroid/redroid:13.0.0_64only-latest`
(`sha256:5a42a569…ac13b`), `/dev/dri` pasado al CT y `binder_linux` cargado en
caliente en el host (`modprobe binder_linux devices=binder,hwbinder,vndbinder`;
el módulo es permanente y **sigue cargado hasta el próximo reinicio**, sin
nada persistente en `/etc/modules` ni `modprobe.d`).

Con `gpu_mode=host` Android usa de verdad la iGPU:
`GLES: Intel, Mesa Intel(R) UHD Graphics 630 (CFL GT2), OpenGL ES 3.2 Mesa 24.0.8`,
`gralloc.gbm.device=/dev/dri/renderD128`, `vulkan=intel`, y surfaceflinger,
systemui, launcher y ajustes aparecen como clientes del i915. En `guest` sale
ANGLE sobre SwiftShader y nadie abre `renderD128`.

| un teléfono, 720×1280 | host (iGPU) | guest (SwiftShader) |
|---|---|---|
| `boot_completed` | 10,4–11,3 s | 6,95 s |
| UI (arrastre continuo en Ajustes, 30 s) | 60 fps, **0,13–0,15 núcleos** | 60 fps, **0,73 núcleos** |
| WebGL (raymarch a pantalla completa) | **60 fps** (vsync), 0,23 núcleos, GPU 47 % | **16,5 fps**, 3,72 núcleos |
| RAM del host por teléfono extra | ~550–585 MiB | (no medido) |

Densidad medida con `gpu_mode=host`: 3 teléfonos → UI a 60 fps cada uno con la
GPU al 7 %; 5 teléfonos → UI a 60 fps cada uno, GPU al 8,6 %, 0,14 núcleos
cada uno; con WebGL los 5 a ~31 fps (GPU al 100 %, ~155 fps de techo total).

Métodos: fps de `dumpsys gfxinfo` y `dumpsys SurfaceFlinger --timestats`; CPU
del `cpu.stat` del cgroup; ocupación de GPU de `drm-engine-render` en el
`fdinfo` del i915; memoria de `MemAvailable` del host.

### Caminos para varios teléfonos acelerados

| camino | aislamiento | teléfonos por host (15 GiB) | esfuerzo |
|---|---|---|---|
| **Redroid en contenedor + renderD128 compartido** | kernel compartido, contenedor privilegiado, binder en el host | **~20 en UI** (manda la RAM), ~10–12 hoy con los CT vivos; 3D pesado ~155 fps a repartir | ninguno: medido |
| crosvm + virtio-gpu (virgl/venus/gfxstream) | VM | ~10–12 (estimado, RAM) | semanas: backend VMM nuevo + imagen Android de VM (tipo Cuttlefish) |
| QEMU `microvm` + virtio-gpu (virgl), en un CT | VM | **~5–6** con 2 GiB, ~7 con 1,5 GiB (RAM; medido un teléfono, ver abajo) | **medido** (prueba de concepto de abajo): la imagen y el kernel `gpu` de siempre, QEMU 10 de bookworm-backports; backend = un ayudante tipo `kling-vz` |
| QEMU (Proxmox) `vga: virtio-gl` | VM | ~6–7 (estimado) | virgl ya enlazado en pve-qemu 10.1.2 (virglrenderer 1.1.0); venus pide `mesa-vulkan-drivers` en el host; imagen Android-x86 |
| GVT-g (vGPU mediada) | VM | ~2 con 256 MiB de apertura, ~7 con 1 GiB | cmdline `intel_iommu=on i915.enable_gvt=1`, módulos `kvmgt vfio_mdev`, reinicio, BIOS; Intel lo abandonó tras Gen11 |
| Firecracker (kindling hoy) | microVM | 0 acelerados | no tiene virtio-gpu ni vfio de GPU |
| cloud-hypervisor + vfio | VM | 1 (GPU entera) | no hay virtio-gpu upstream |

**Recomendación**: para meter más teléfonos acelerados por host, Redroid en
contenedor con `renderD128` compartido; da ~9× los fps de SwiftShader en 3D por
teléfono y gasta 5× menos CPU en UI. Si hace falta frontera de VM, el siguiente
paso es crosvm con virtio-gpu (virgl o venus). Nada de esto cambió la
configuración de arranque de `pve`; GVT-g queda como recomendación no aplicada.

Diseño y plan por fases para GPU con frontera de VM (crosvm + virtio-gpu, sin
dorados con GPU 3D en ningún VMM hoy): [`gpu-proxmox.md`](gpu-proxmox.md).

## Prueba de concepto: un teléfono en una VM con virgl (issue #95)

Medido el 2026-11-12 en `pve` (Proxmox VE 9.1, kernel 6.17, i7-8700T, UHD 630),
**sin tocar el host**: todo dentro del CT 106 `phones` (privilegiado, ya tenía
`/dev/kvm`, `/dev/dri` y `/dev/net/tun`), con su memoria bajada a 3 GiB durante
la prueba. Es la fase 0 de [`gpu-proxmox.md`](gpu-proxmox.md) §5, pero con
QEMU en vez de crosvm (abajo, por qué).

**Resultado: funciona.** El teléfono de kindling, con la **misma imagen por
capas de Firecracker sin reconstruir** (`android-base` + `android13.layer`,
Redroid 13 `64only-240527`), arranca en una VM de QEMU `microvm` con
virtio-gpu, y Android pinta con la iGPU a través de virgl:

```
GLES: Mesa, virgl (Mesa Intel(R) UHD Graphics 630 (CFL GT2)), OpenGL ES 3.2 Mesa 24.0.8
[drm] features: +virgl +edid -resource_blob -host_visible
ro.boot.redroid_gpu_mode=host  ro.hardware.egl=mesa  gralloc.gbm.device=/dev/dri/renderD128
```

El riesgo de `gpu-proxmox.md` §4.2 (redroid-doc #705: `gpu_mode=host` sobre
virtio-gpu que no funcionaba en QEMU aarch64) no aparece en x86_64.

### Cómo

| pieza | qué se usó |
|---|---|
| VMM | QEMU 10.0.2 (`bookworm-backports`) dentro del CT, `-M microvm,acpi=off,pcie=off`, KVM, 4 vCPU, 2 GiB |
| discos y red | los de una máquina de kindling: base ro en `vda`, overlay escribible (copia del molde) en `vdb`, capa Android ro en `vdc`, virtio-net a un tap en un netns propio con `172.16.0.1/30`; la línea de kernel de `internal/machine` (`bootArgs`) |
| GPU | `-device virtio-gpu-gl-device` + `-display egl-headless,rendernode=/dev/dri/renderD128` (Mesa iris 22.3 del CT, virglrenderer 0.10.4) |
| kernel | el perfil `gpu` de siempre con `TARGET_ARCH=amd64` (`DRM_VIRTIO_GPU`, virtio-mmio, PVH): **sin cambios** en `kernel/`; sale `vmlinux-6.1.140-kindling-amd64-android-gpu` |
| Android | en la overlay, un `android.conf` con `ANDROID_GPU_MODE=host`, `ANDROID_GPU_NODE=/dev/dri/renderD128` (el virtio-gpu **del invitado**) y `ANDROID_FPS=60`, y el `android-launch.sh` de este árbol, que ahora lee esas dos variables (también `kling-phoned`). Modo CPU: lo mismo con `gpu_mode=guest` (SwiftShader) y un `virtio-gpu-device` 2D |

Scripts: [`gpu/qemu-phone.sh`](../gpu/qemu-phone.sh) (`up N gpu|cpu`, `down`,
`adb`, `monitor`) y [`gpu/qemu-bench.sh`](../gpu/qemu-bench.sh), con la página
[`gpu/webgl-raymarch.html`](../gpu/webgl-raymarch.html):

```sh
# en el CT (QEMU 10, Mesa, adb; cabeceras de los scripts)
TARGET_ARCH=amd64 ANDROID_PROFILE=gpu KERNEL_SHA256=5779f9ca… \
  OUT=/root/android-kernel-gpu prototypes/android/kernel/build.sh
prototypes/android/gpu/qemu-bench.sh 1 gpu       # arranca, mide y deja vivo
prototypes/android/gpu/qemu-phone.sh down 1
prototypes/android/gpu/qemu-bench.sh 1 cpu
SKIP_UP=1 SETTLE=0 PASSES=4 prototypes/android/gpu/qemu-bench.sh 1 cpu
```

Método, el de arriba con dos diferencias: la CPU es la del **proceso QEMU
entero** (vCPU más virglrenderer, que corre dentro de QEMU: `utime+stime` de
`/proc/PID/stat`), no la de un cgroup; y la página WebGL es **otra** (la de la
medida del contenedor no se guardó): un raymarch a pantalla completa
(720×990 físicos, debajo de la barra de URL del navegador de WebView) que con
`?passes=K` dibuja la escena K veces por fotograma, para ver el techo cuando
60 fps (vsync) ya no dice nada. UI: 30 s de `input swipe` arriba y abajo en
Ajustes, fps de `dumpsys SurfaceFlinger --timestats` (y `gfxinfo`). GPU:
`drm-engine-render` del `fdinfo` del i915 del proceso QEMU.

### Cifras (un teléfono, 720×1280@320dpi, 4 vCPU, 2 GiB)

| | **virgl** (VM) | SwiftShader (misma VM) | contenedor `host` (arriba) | contenedor `guest` (arriba) |
|---|---|---|---|---|
| `boot_completed` en frío | 10,7–10,8 s (3 de 6 arranques en 17,5–17,7 s) | 10,1 s (4 de 4) | 10,4–11,3 s | 6,95 s |
| UI: fps (SurfaceFlinger) | **59,8–60,1**, 0–0,06 % de fotogramas lentos, p90 11 ms | 59,8–60,2, 0–0,23 %, p90 9–16 ms | 60 | 60 |
| UI: núcleos | **0,24–0,25** | 1,21–1,25 | 0,13–0,15 | 0,73 |
| UI: GPU | 5,7 % | — | (8,6 % con 5 teléfonos) | — |
| WebGL ×1: fps · núcleos · GPU | **60** (vsync) · 0,33 · 47 % | 19,7 · 3,90 · — | 60 · 0,23 · 47 % (otra página) | 16,5 · 3,72 (otra página) |
| WebGL ×4 (techo): escenas/s · núcleos · GPU | **161** (40,3 fps) · 0,25 · 97 % | 22 (5,6 fps) · 3,93 | — | — |
| RAM del proceso QEMU (RSS ≈ PSS) | 1891–2120 MiB | 1997–2091 MiB | ~580 MiB por teléfono | — |
| memoria del i915 (`drm-resident-system0`) | ~165–185 MiB (RAM del sistema, **aparte** del RSS) | 0 | — | — |
| con `-m 1536` | RSS 1668 MiB + 160 MiB del i915; misma UI (0,24 núcleos) y WebGL (60 fps, 0,34) | — | — | — |

En la misma VM, virgl gasta **5 veces menos CPU en UI** (1,23 → 0,25 núcleos)
y en 3D da **~7 veces más** (161 frente a 22 escenas/s) con **12–16 veces menos
CPU** (0,25–0,33 frente a 3,9 núcleos). Frente al contenedor con
`gpu_mode=host`, la VM cuesta en UI ~0,1 núcleos más (0,25 frente a 0,14: la
traducción de virgl y el salto invitado → QEMU), y la GPU va igual de holgada
(5,7 % por teléfono). SwiftShader en la VM gasta más que en el contenedor
(1,23 frente a 0,73 núcleos en UI); no se investigó, y aquí se cuenta el
proceso QEMU entero. El arranque con virgl es como el del contenedor (10,7 s);
los 17,5–17,7 s de tres de los seis arranques no se investigaron.

### Densidad estimada

Sin dorado, cada teléfono paga su RAM entera: **~2,2 GiB con `-m 2048`** (RSS
de QEMU + memoria del i915) y **~1,8 GiB con `-m 1536`**. Con ~12 GiB libres
para teléfonos en este host (15,8 − ~3 de Proxmox y el resto) salen **~5–6
teléfonos con 2 GiB, ~6–7 con 1,5 GiB**, frente a ~20 en contenedor y ~21
clones de Firecracker en 6 GiB (que comparten el dorado). La GPU no limita en
UI (5,7 % por teléfono, ~17 a la vez antes de saturarla); en 3D continuo el
techo es ~161 escenas/s de esta página a repartir (≈ 2–3 teléfonos con 3D
pesado a 60 fps). **No se midieron varios teléfonos a la vez**: durante la
prueba alguien apagó el CT 106 y arrancó la VM 201 (12 GiB), y con lo que
quedaba (~5 GiB disponibles) dos teléfonos no cabían con margen; tampoco se
midió KSM.

### Qué falló por el camino (y cómo quedó)

| síntoma | causa | arreglo |
|---|---|---|
| con QEMU 7.2 (Debian 12) y `egl-headless`, la VM no escribe ni una línea: vCPU en `HLT` antes del kernel | el qboot de `microvm` (y el `bios-256k` de SeaBIOS) no arrancan con una pantalla GL en 7.2 | **QEMU 10.0.2** de `bookworm-backports` (`qemu-system-x86 qemu-system-gui qemu-system-modules-opengl`). Con 7.2, el `bios.bin` de SeaBIOS arranca pero da **1 CPU** (tabla MP) y cuelga al sondear `vdb` salvo con `noapic` (las cifras de 7.2 con 1 vCPU no se usan) |
| no hay `/dev/dri` en el invitado; el kernel no dice nada del virtio-gpu | virtio-mmio de QEMU es **legacy** por defecto y virtio-gpu exige `VIRTIO_F_VERSION_1` | `-global virtio-mmio.force-legacy=false` (Firecracker ya habla v2) |
| `WebGL1 blocklisted` en el navegador de WebView | el Chromium de WebView tiene a virgl en su lista negra de GPU | `_ --ignore-gpu-blocklist` en `/data/local/tmp/webview-command-line` (Redroid es `debuggable`). **Para un backend real:** decidirlo por imagen; las apps con su propio WebView verán lo mismo |
| la UI se quedaba en 15 fps | `android.conf` de la imagen trae `ANDROID_FPS=15` | `ANDROID_FPS=60` en la overlay para medir como el contenedor |
| venus: `old virglrenderer, blob resources unsupported` | QEMU pide virglrenderer ≥ 1.0 para blobs; bookworm trae 0.10.4 | **no probado**. Un CT Debian 13 (virglrenderer 1.1.0, QEMU 10, Mesa 25) lo tendría de serie, más `/dev/udmabuf` pasado al CT (`gpu-proxmox.md` §6.1) |
| `savevm` con virgl | `Error: virgl is not yet migratable` (medido) | sin dorado ni restore: cada teléfono arranca en frío (~11 s) |
| en el lab: el `flock /tmp/lab.lock` de pve quedó cogido por QEMU | QEMU hereda el descriptor del cerrojo a través de `pct exec` | `flock -o` (cierra el descriptor antes de ejecutar). Y lanzar las pasadas con `systemd-run` dentro del CT, que al acabar la unidad mata su cgroup (QEMU incluido) |

Sin agente: la PoC no pasa vsock, así que no hay `kling exec` ni sonda de
listo; todo va por adb (puerto 5555 reenviado como siempre). Para un backend,
QEMU `microvm` tiene `vhost-vsock-device` y el host ya trae `/dev/vhost-vsock`
(10:241): bastaría pasarlo al CT.

### Recomendación y esfuerzo para un backend real

Por el criterio de parada de `gpu-proxmox.md` (0.3 sí: ≤ 0,4 núcleos; 0.4 no:
1,8–2,2 GiB por teléfono frente a ≤ 1,6 GiB), una VM con GPU **solo compensa si
la frontera de VM es requisito** (APKs de terceros): da 3D y UI baratas en CPU,
pero ~5–7 teléfonos por host. Si lo es, la PoC cambia la recomendación de
`gpu-proxmox.md` (crosvm): **el camino más corto es QEMU `microvm`** detrás de
un ayudante `kling-qemu` que hable la API de Firecracker, igual que `kling-vz`
en el Mac:

- reutiliza **sin cambios** la imagen, el kernel `gpu` (virtio-mmio, sin PCI ni
  ACPI, que el comprobador del núcleo sigue prohibiendo y que crosvm sí
  necesitaría), la disposición de discos y el netns/tap de `internal/net`;
- QEMU, Mesa y virglrenderer vienen empaquetados (Debian 13, o backports en
  12); crosvm habría que compilarlo y fijarlo;
- lo que cuesta lo mismo en los dos: **sin dorado** (QEMU bloquea `savevm` con
  virgl; crosvm solo guarda virtio-gpu en 2D), así que el núcleo tiene que
  saber que ese backend no congela (`snapshot:false`, `gpu-proxmox.md` §4.1) y
  la admisión contar la RAM entera.

Lo que se pierde frente a crosvm es aislamiento: virglrenderer corre **dentro**
del proceso QEMU (crosvm lo encierra en un proceso con minijail). Mitigación:
QEMU sin privilegios en el netns, `-sandbox on`, sin monitor expuesto y, en
cuanto haya virglrenderer ≥ 1.0, venus con su `render_server` aparte.

| paso | esfuerzo |
|---|---|
| `kling-qemu` (Go): subconjunto de la API de Firecracker → argv de `microvm` + QMP, MMDS propio, vsock por `vhost-vsock-device`, tests con un QEMU falso; con un daemon privado (fase 1 de `gpu-proxmox.md`) | 1–2 semanas |
| núcleo: `VMMQemu`, capacidades por backend (`snapshot:false`, `gpu`), admisión sin compartir memoria, `kling save/freeze` → 409 (fase 2) | 1–2 semanas |
| imagen: `build-image.sh GPU=virgl` que escriba `ANDROID_GPU_MODE/NODE` y `ANDROID_FPS=60` en `android.conf`, la bandera de WebView, y que `kling-phoned` diga en `/v1/health` qué GPU tiene | 2–3 días |
| medir varios teléfonos a la vez (1 h de UI, 5 teléfonos) con el host libre, y venus en un CT Debian 13 | 1–2 días |

Total: **~3–5 semanas** hasta un teléfono `-gpu` utilizable junto a los de
Firecracker, para ~5–7 teléfonos acelerados por host frente a ~20 en
contenedor. Si la frontera de VM no es requisito, el contenedor sigue siendo
el que más mete.

Estado en el que quedó el lab: CT 106 parado y con su memoria devuelta a
6 GiB; dentro quedan QEMU 10 (con `bookworm-backports` en
`/etc/apt/sources.list.d/bpo-gpu-poc.list`), `mesa-vulkan-drivers`, `strace`,
el kernel en `/root/android-kernel-gpu` y los registros en `/root/gpu-poc`. En
el host no se instaló ni cambió nada; el guardián de memoria temporal de la
prueba (mataba las QEMU del CT por debajo de 1,5 GiB disponibles; no llegó a
actuar) se paró al acabar.
