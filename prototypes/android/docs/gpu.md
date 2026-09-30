# GPU para el prototipo Android: qué se puede en el Mac y en Proxmox

Medido el 2026-09-28 en un MacBook Air M4 (16 GiB, macOS 26.5) y en el
Proxmox `pve` (i7-8700T, 15 GiB, Intel UHD 630). Resumen:

| | Mac (`vz`) | Proxmox (UHD 630) |
|---|---|---|
| Aceleración 3D para Android | **no existe**: virtio-gpu de Apple es 2D para Linux | **sí**, Redroid `gpu_mode=host` sobre `/dev/dri/renderD128` (Mesa iris) |
| Ver el teléfono sin VNC | **sí**: ventana nativa + capturas en 11–20 ms, a ~14 fps, y sobrevive a save/restore | con contenedor: el VNC/scrcpy de Redroid (no probado aquí) |
| Coste | +12 MiB por VM, restaurar igual | ~580 MiB de RAM y ~0,14 núcleos por teléfono en UI |

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
