# GPU acelerada para los teléfonos en Proxmox: diseño y plan

Issue #95. Documento de diseño, **sin implementación**: qué camino tomar para que
los teléfonos Android de kindling pinten con la GPU del host `pve`, con qué
aislamiento, cuánto cuesta en kindling y en qué orden hacerlo. Las cifras
medidas son de [`gpu.md`](gpu.md) y [`proxmox.md`](proxmox.md); las estimadas
van marcadas como tales, y las afirmaciones sobre software ajeno llevan fuente
al final (consultadas el 2026-09-29).

## 0. Resumen

- **Ningún camino con frontera de VM y GPU 3D conserva los dorados hoy.** crosvm
  solo sabe hacer snapshot de virtio-gpu en modo 2D ("3D is WIP" en su código),
  Cuttlefish solo admite snapshot con `guest_swiftshader`, y QEMU bloquea
  `savevm`/migración en cuanto hay virgl. Sin dorado no hay restauración en
  0,1 s ni páginas compartidas por MAP_PRIVATE: cada teléfono con GPU arranca en
  frío (~11–14 s) y paga su RAM entera.
- Eso cambia la cuenta de densidad: ~21 teléfonos por CPU en 6 GiB (Firecracker,
  medido) frente a **~5–9 teléfonos con GPU** en el host entero (estimado).
- **Recomendación**: dos clases de teléfono. El de siempre en Firecracker (CPU,
  dorados, barato) y uno `-gpu` bajo demanda en **crosvm + virtio-gpu**, detrás
  de un ayudante `kling-cvm` que habla la API de Firecracker, igual que
  `kling-vz` en el Mac. Primero **virgl** (no pide nada al host), luego **venus**
  (renderizador en un proceso aparte). Redroid en contenedor solo para cargas
  propias. GVT-g descartado.

## 1. El host, leído el 2026-09-29 (solo lectura, `ssh pve`)

| | valor | qué implica |
|---|---|---|
| CPU | i7-8700T, 6 núcleos / 12 hilos, VT-x con EPT, `kvm_intel nested=Y` | igual que en `proxmox.md` |
| RAM | 15 830 MiB, 8 709 disponibles con lo que corre hoy (CT 100, 102, 105, 106 y VMs 200 y 210), 3 GiB de swap usados | la densidad real depende de lo que se apague |
| GPU | `00:02.0` UHD 630 CoffeeLake-S GT2 `[8086:3e92]`, driver i915, grupo IOMMU 0, apertura (BAR prefetchable) **256 MiB** | sin `sriov_totalvfs`: **no tiene SR-IOV** |
| kernel | `6.17.2-1-pve`, cmdline `ro quiet` (nada añadido) | `CONFIG_INTEL_IOMMU_DEFAULT_ON=y`: **IOMMU ya activa** (`dmar0`, `dmar1`, 9 grupos) sin tocar la cmdline |
| GVT-g | `CONFIG_DRM_I915_GVT=y`, `kvmgt.ko` y `vfio_mdev` como módulos, `i915.enable_gvt=N` | habría que activarlo y reiniciar (§2.6) |
| `/dev/dri` | `card1` (grupo video) y `renderD128` con **GID 104** (en el host se llama `postfix`; el grupo `render` es el 993) | dentro de un CT hay que dar permiso por GID, no por nombre |
| `/dev/udmabuf` | existe (`CONFIG_UDMABUF=y`, 10:259) | venus y los blobs lo necesitan; **no** está pasado a ningún CT |
| QEMU | `pve-qemu-kvm 10.1.2-3`, `libvirglrenderer1 1.1.0-2` | virgl ya enlazado; venus existe desde QEMU 9.2 |
| Mesa del host | 25.0.7 (`iris`, `crocus`, `libgl1-mesa-dri`), **sin** `mesa-vulkan-drivers` ni `/usr/share/vulkan/icd.d` | sin ANV no hay Vulkan en el host: venus no funciona hasta instalarlo (en el host o en el CT que corra el VMM) |
| crosvm, cloud-hypervisor | no instalados | se compilan (no hay paquete de Debian de crosvm) |
| THP | `madvise` | no molesta; KSM no fusiona páginas enormes |
| KSM | `ksmtuned` activo, `run=0` ahora (arranca solo bajo presión) | puede devolver parte de lo que se pierde sin dorado (§4.4) |
| CT 106 `phones` | ya tiene `/dev/kvm` y `/dev/dri` pasados (`c 226:* rwm`) | **la fase 0 con virgl no necesita tocar ni el host ni el CT** |

## 2. Opciones comparadas

Base medida (UHD 630, un teléfono 720×1280, `gpu.md`): contenedor con
`gpu_mode=host` → UI a 60 fps con 0,13–0,15 núcleos y WebGL a 60 fps; SwiftShader
→ UI a 60 fps con 0,73 núcleos y WebGL a 16,5 fps con 3,72 núcleos. El techo de
la iGPU en el WebGL de prueba es ~155 fps en total. En UI la GPU no manda (5
teléfonos la ocupan al 8,6 %): manda la RAM.

### 2.1 Tabla

| | aislamiento | teléfonos acelerados / host | fps y CPU esperados por teléfono | dorados (MAP_PRIVATE) y restore rápido | trabajo en kindling | riesgo principal |
|---|---|---|---|---|---|---|
| **A. Redroid en contenedor + `renderD128` compartido** | **ninguna frontera de VM**: kernel del host, contenedor privilegiado, `binder_linux` en el host, el i915 del host expuesto a Android | **~20 en UI** con el host libre (~580 MiB cada uno, medido), ~10–12 con lo que corre hoy | **medido**: UI 60 fps / 0,14 núcleos; WebGL 60 fps (1) y ~31 fps con 5 a la vez | **no**: cada teléfono arranca en frío (10,4–11,3 s); CRIU no sabe congelar un proceso con el i915 abierto | fuera del VMM: un "backend de contenedor" nuevo (Docker/LXC, sin netns propia de kindling, sin MMDS, sin agente) | un fallo del kernel o del i915 desde Android compromete el host; no apto para APKs de terceros |
| **B. crosvm + virtio-gpu (virgl)** | VM (KVM); el dispositivo GPU de crosvm en su propio proceso encerrado con minijail | **~5–9** (estimado: 1,2–1,6 GiB por teléfono sin compartir; ~5 con la carga de hoy, ~9 con el host dedicado) | estimado: UI 60 fps con **0,2–0,4 núcleos** (virgl traduce TGSI→GLSL en el host y compila dos veces); 3D pesado al 50–70 % del contenedor | **no**: snapshot de virtio-gpu solo en 2D | ayudante `kling-cvm` (API de Firecracker → crosvm, §3), MMDS propio, capacidades en `GET /kling/info`, núcleo sabiendo que "este backend no congela" | virglrenderer corre DENTRO del proceso del dispositivo y tiene historial de CVE (lectura/escritura fuera de límites desde el invitado); lo mitiga el sandbox de crosvm |
| **C. crosvm + virtio-gpu (venus / Vulkan)** | VM; el renderizador de venus en un **proceso aislado** del VMM (el único tipo de contexto de virglrenderer que lo hace) | igual que B | estimado: cerca del nativo en Vulkan; GLES por ANGLE sobre venus dentro de Android; sincronización invitado↔host como límite | **no** | lo de B + ANGLE/venus en la imagen, `mesa-vulkan-drivers` (ANV, Gen9 vale) y `/dev/udmabuf` donde corra crosvm | ruta menos probada con Android que virgl o gfxstream; ANV en Gen9 es el mínimo que Mesa da por bueno (21.1+) |
| **D. crosvm + gfxstream** | VM; gfxstream reenvía GL/Vulkan casi tal cual al host | igual que B | el mejor rendimiento de los tres en Cuttlefish | **no** (Cuttlefish: snapshot solo con SwiftShader) | lo de B + imagen con los *guest* de gfxstream (no los trae Redroid: hay que construirlos, tipo Cuttlefish) | superficie grande (API casi completa reenviada), pensado para el emulador; **no** recomendado para cargas no confiables |
| **E. QEMU con virtio-gl/virgl** (Proxmox `vga: virtio-gl`, o QEMU como backend) | VM | **~5–7** (estimado; QEMU pesa algo más que crosvm por VM) | como B (misma virglrenderer 1.1.0) | **no**: `virgl is not yet migratable` bloquea `savevm`; con blobs pasa lo mismo. La plantilla de QEMU (`memory-backend-file` + `x-ignore-shared`, lo que usa Kata) sí daría MAP_PRIVATE, pero solo **sin** virgl | o VMs de Proxmox gestionadas por `qm` (fuera de kindling: sin dorados, sin MMDS, sin exec), o un backend QMP nuevo (más trabajo que `kling-cvm`: QMP, `-netdev tap` en el netns, globo por QMP) | lo de virgl; en Proxmox, venus solo por `args:` a mano |
| **F. cloud-hypervisor** | VM | 0 con virtio-gpu (no hay upstream; solo el parche de Spectrum con el dispositivo de crosvm por vhost-user); 1 con vfio de la GPU entera | — | snapshot sí, pero no con vfio | backend nuevo sin ganar nada frente a crosvm | parche externo sin mantener |
| **G. GVT-g** (vGPU mediada de Intel, mdev) | VM, GPU partida por el i915 del host | ~2 con la apertura de 256 MiB de hoy; ~7 con 1 GiB en la BIOS (gpu.md) | casi nativo por vGPU | **no**: vfio fija la memoria del invitado (sin globo ni sobresuscripción); sin snapshot | backend con vfio (crosvm o QEMU) + gestión de mdev | **Intel lo abandonó** (repo archivado en 10/2024) y hay informes de que dejó de funcionar hacia el kernel 6.8; pve va en 6.17. Reinicio del host y BIOS |
| **H. SR-IOV** (VF por teléfono) | VM, partición en hardware | **0 aquí**: la UHD 630 no tiene SR-IOV; en Gen12+ (Xe) hasta 7 VF en iGPU | casi nativo | **no** (vfio: memoria fijada, sin snapshot) | backend con vfio | driver fuera de árbol en iGPU (i915-sriov-dkms) o `xe` según plataforma |
| **I. Firecracker (hoy)** | microVM | **0 acelerados**; ~21 por CPU en un CT de 6 GiB (medido) | UI 60 fps con ~0,73 núcleos; WebGL 16,5 fps | **sí**: ~165–187 MiB por clon, restore → adb en 0,1 s | nada | no hay virtio-gpu ni vfio; el trabajo de PCIe/GPU de Firecracker está **en pausa** desde 02/2025 y su primer hito sería vfio de GPU entera, sin snapshots |

(1) 60 fps es el vsync: el contenedor no llega al techo con un teléfono.

### 2.2 Cómo sale la estimación de densidad sin dorado

- En Firecracker un clon cuesta ~187 MiB porque comparte el `mem.file` del
  dorado; un teléfono en frío toca casi toda su RAM de invitado (en `vz`, que no
  comparte: 1584 MiB de footprint para 1536 MiB, `gpu.md`). Con `SLIM=1`,
  `DATA_MODE=overlay` y 1024–1536 MiB, lo razonable es **1,0–1,6 GiB por teléfono**
  más lo que virglrenderer/crosvm reserve en el host para búferes (3 búferes de
  720×1280×4 ≈ 11 MiB por pantalla, más texturas; en iGPU es RAM del sistema).
- Host dedicado: 15,8 GiB − ~3 GiB para Proxmox y el daemon → ~12 GiB → **7–9**.
  Con lo que corre hoy (8,7 GiB disponibles) → **~5**.
- KSM puede fusionar lo idéntico entre teléfonos (framework, zygote, fuentes);
  no se ha medido, y la fase 0 lo mide (§4.4).
- La GPU no es el límite en UI; con 3D continuo lo es: ~155 fps de techo que con
  virgl bajarían (estimado) a 80–110 a repartir.

### 2.3 Qué pide cada una al VMM, frente a lo que kindling usa de Firecracker

| pieza que usa kindling | Firecracker | crosvm | QEMU | cloud-hypervisor |
|---|---|---|---|---|
| API de control | HTTP en `--api-sock` | CLI + socket de control (`crosvm suspend/resume/balloon/stop`) | QMP | HTTP propia |
| snapshot / restore | sí, `mem.file` mapeado MAP_PRIVATE | experimental ("highly experimental"), GPU 3D no, memoria serializada (no mapeada) | sí, pero no con virgl | sí, no con vfio |
| globo con estadísticas | sí | sí (`balloon_stats`), y *free page reporting* | sí (QMP) | sí |
| red: tap en el netns de kindling | sí | sí (`--net tap-name=` o `tap-fd=`) | sí | sí |
| MMDS (169.254.169.254) | integrado | **no** | no | no |
| vsock | sí | sí | sí | sí |
| virtio-rng / VMGenID | sí / sí | sí / no | sí / sí | sí / no |
| arranque de `vmlinux` ELF sin PCI | sí | usa virtio-**PCI** en x86 | PCI | PCI |
| sandbox | jailer | minijail por dispositivo (propio) | ninguno propio (Proxmox no lo añade) | seccomp |

## 3. Recomendación

### Este host (i7-8700T, UHD 630, 15 GiB)

1. **El teléfono por defecto sigue en Firecracker**: es lo que da densidad (~21
   en 6 GiB) y restaurar en 0,1 s. La GPU no mejora la UI en fps (ya va a 60),
   solo la CPU (0,73 → 0,14–0,4 núcleos) y el 3D/WebGL/vídeo.
2. **Teléfono `-gpu` con frontera de VM**: crosvm + virtio-gpu, detrás de
   `kling-cvm`. Empezar por **virgl** porque no pide nada al host (virglrenderer
   va dentro del CT; CT 106 ya ve `/dev/dri`) y la imagen de Redroid ya trae
   `virtio_gpu_dri`. Pasar a **venus** en cuanto se acepte instalar ANV en el CT
   y pasarle `/dev/udmabuf`: es la opción con el renderizador fuera del VMM, la
   que conviene para APKs de terceros.
3. **Redroid en contenedor** solo para cargas propias y cuando haga falta el
   máximo de teléfonos acelerados (~20): es lo medido y lo más barato, pero sin
   frontera de VM. Si se usa, fuera del núcleo (un script, como hoy).
4. **Descartado aquí**: GVT-g (abandonado, roto según informes con 6.8+,
   reinicio y BIOS, 2–7 vGPU con memoria fijada), SR-IOV (no existe en la
   UHD 630), cloud-hypervisor (sin virtio-gpu), gfxstream (superficie y trabajo de
   imagen), QEMU/Proxmox (mismo virgl que crosvm y más trabajo para integrarlo
   en kindling; útil solo como comprobación rápida con `qm`).

### Un host más grande (GPU dedicada)

- **AMD (RADV) o Intel Arc con venus en crosvm**, y cuando madure el *DRM native
  context* (vDRM: el invitado habla el UAPI del driver real; en crosvm parcial,
  amdgpu y msm en virglrenderer) pasar a él: es el que promete rendimiento
  nativo con muchos invitados sobre una GPU. La densidad la siguen poniendo la
  RAM (sin dorado) y la VRAM.
- **SR-IOV** (Intel Xe/Flex, AMD MxGPU, NVIDIA vGPU con licencia) solo si hace
  falta rendimiento casi nativo por teléfono y se acepta perder globo,
  sobresuscripción y snapshots (vfio fija la memoria). Pocas VF por GPU.
- NVIDIA con venus: driver 570.86+ y kernel 6.16+ según Mesa.

### Qué haría primero

La **fase 0** de §5: crosvm a mano en el CT 106 con el kernel y la imagen de
Android actuales más `DRM_VIRTIO_GPU`, un teléfono con virgl, y medir fps, CPU,
RAM por teléfono y cuántos caben. Si un teléfono con virgl no baja de 0,4 núcleos
en UI o no pasa de ~5 por host, el backend no compensa frente al contenedor y
Firecracker, y se para ahí.

## 4. Diseño del backend en kindling (crosvm)

### 4.1 Cómo encaja: un ayudante que habla la API de Firecracker

El núcleo ya tiene el patrón: en macOS lanza `kling-vz --api-sock <ruta>`, que
imita la API de Firecracker (`docs/backend-vz.md`). En Linux el daemon lanza
`m.fcBin --api-sock sock` **dentro del netns** de la máquina y en su cgroup
(`internal/machine/manager.go`, `spawn`), y `KLING_VMM` ya acepta la ruta de
cualquier binario (`internal/machine/vmm.go`, `resolverVMM`). Así que:

```
kling daemon ─ netns + tap0 + iptables + cgroup (internal/net, igual que con FC)
   └─ kling-cvm --api-sock machines/<id>/fc.sock      (Go, sin cgo, en el netns)
        ├─ API compatible con Firecracker (subconjunto, §4.2) + /kling/info
        ├─ MMDS v2 en 169.254.169.254:80 (pkg/guest/mmds.go ya define la semántica)
        └─ crosvm run --gpu ... --net tap-name=tap0 --block ... vmlinux
             └─ proceso del dispositivo GPU (minijail) ── virglrenderer / render_server de venus
                  └─ /dev/dri/renderD128 (i915 del host)
```

Todo lo que en `plataforma_fc.go` hace el host alrededor del VMM (netns, veth,
tap, iptables, egress, proxy de credenciales, grafos, cgroups, `/proc`,
reflink) **se reutiliza sin cambios**: crosvm usa el mismo tap. Por eso
`kling-cvm` vive en Linux junto a Firecracker y no necesita etiqueta de
compilación nueva; va como `cmd/kling-cvm` (o módulo aparte como `vz/` si
arrastra dependencias).

Cambios en el núcleo, pequeños y en orden:

| dónde | cambio |
|---|---|
| `pkg/config` | `VMMCrosvm = "crosvm"`; `ValidateVMM` lo acepta solo en `linux` con `/dev/kvm`; `daemon.vmm: crosvm` |
| `internal/machine/vmm.go` | `BackendCrosvm`, `NombreVMM` → `kling-cvm`, `resolverVMM` lo busca junto a `kling` |
| `internal/machine` | hoy `restaurarComparteMemoria`, `jailerPosible` y compañía son **constantes por plataforma**; pasan a ser capacidades del backend leídas de `GET /kling/info` (`{"backend":"crosvm","snapshot":false,"mem_shared":false,"balloon_stats":true,"gpu":["virgl","venus"]}`). Con `snapshot:false`: `kling save`/`freeze` contestan 409 con mensaje claro, el scheduler no congela por `idle_freeze` (solo pausa) y la admisión cuenta la memoria entera del invitado |
| jailer | no aplica (es de Firecracker). `kling-cvm` corre como el usuario sin privilegios (`privilegiosPlataforma`) y deja a crosvm su minijail; el `seccomp` de crosvm viene con él |
| red | igual; lo único nuevo es MMDS: Firecracker lo sirve dentro de su tap y no cruza iptables; `kling-cvm` lo sirve en el netns, así que `internal/net` añade `169.254.169.254/32` al `lo` del netns y un ACCEPT de tap0 a ese puerto **solo para este backend** (el `DROP` de egress `none` sigue igual para todo lo demás) |
| API `POST /machines` | campo `vmm` por máquina (vacío = `daemon.vmm`) y `gpu` (`""`, `virgl`, `venus`). Hasta que el `Manager` sepa llevar dos backends a la vez, la fase 1 usa **un daemon privado** con `daemon.vmm: crosvm` (dos daemons ya conviven en el CT 106, `proxmox.md`, arreglo #97) |

`kling-cvm` traduce (subconjunto de `internal/fc/client.go`):

| petición | a crosvm |
|---|---|
| `PUT /boot-source` | kernel y `boot_args`; quita `pci=off` (crosvm usa virtio-PCI) |
| `PUT /machine-config` | `--cpus`, `--mem` |
| `PUT /drives/{id}` | `--block path=...,ro=` en el orden de llegada (`vda`, `vdb`…) |
| `PUT /network-interfaces/{id}` | `--net tap-name=tap0,mac=...` |
| `PUT /balloon`, `PATCH /balloon`, `GET /balloon/statistics` | `--balloon`, `crosvm balloon`, `crosvm balloon_stats` |
| `PUT /entropy` | virtio-rng (crosvm lo trae) |
| `PUT /mmds/config`, `PUT /mmds`, `PATCH /mmds` | servidor MMDS propio |
| `PUT /actions InstanceStart` | `crosvm run ... --gpu backend=virglrenderer,context-types=virgl:virgl2,...` (o `venus`) |
| `PATCH /vm Paused/Resumed` | `crosvm suspend` / `crosvm resume` |
| `PUT /snapshot/create`, `/snapshot/load`, `PATCH /drives` | **400** "kling-cvm: snapshots are not supported with a 3D GPU" |
| `GET /kling/info` | capacidades (arriba) |
| `GET /kling/stats` | RSS de crosvm y de sus procesos de dispositivo (el GPU incluido); más `gpu_busy_pct` del `fdinfo` del i915 de esos procesos, para `kling top` |

### 4.2 La imagen Android con virtio-gpu

**Kernel**: un perfil nuevo `ANDROID_PROFILE=gpu-crosvm` sobre
`config-android-x86_64`, con el mismo mecanismo que el perfil `gpu` del Mac
(`kernel/config-android-gpu` y `check-android-gpu-config.sh`, que ya saben
quitar DRM del `.config` que ve el comprobador del núcleo):

- `DRM`, `DRM_VIRTIO_GPU`, `SYNC_FILE`, `DMABUF_HEAPS` (si no los trae ya
  `config-android`); sin `FB`/fbcon (Android compone por KMS o en pantalla
  virtual);
- `PCI`, `VIRTIO_PCI` (crosvm pone los virtio en PCI); ACPI: probar con y sin
  (en Firecracker 1.17 hubo que quitarlo, `proxmox.md` §2) — prueba 0.2;
- venus pide kernel ≥ 5.16 o los parámetros de virtio-gpu retroportados: el 6.1
  del prototipo los tiene.

**Espacio de usuario** (en la capa, `build-image.sh` con `GPU=virgl|venus`):

| modo | argumentos de `/init` (`phoned/stage2_linux.go`, `image/android-launch.sh`) | qué pinta |
|---|---|---|
| hoy | `androidboot.redroid_gpu_mode=guest` | ANGLE sobre SwiftShader |
| **virgl** | `androidboot.redroid_gpu_mode=host` + `androidboot.redroid_gpu_node=/dev/dri/renderD128` (el virtio-gpu **del invitado**) | Mesa `virtio_gpu_dri` (GLES por virgl), gralloc gbm sobre ese nodo |
| **venus** | `gpu_mode=host` + `ro.hardware.vulkan=virtio` y GLES por ANGLE sobre Vulkan (`ro.hardware.egl=angle`) | Vulkan por venus, GLES por ANGLE; hay que comprobar que el gralloc de Redroid asigna con blobs |

El modo pasa de la receta a `stage2` como hoy (`ANDROID_EXTRA_ARGS`), con una
etiqueta de receta `kling.gpu` que `kling-phoned` lea para su `/v1/health`
(`"gpu": "virgl"` y el `GL_RENDERER` que diga SurfaceFlinger), y que falle la
sonda de listo si se pidió GPU y SurfaceFlinger cayó a SwiftShader.

Riesgo conocido: un usuario de Redroid dentro de QEMU (aarch64) no consiguió que
`gpu_mode=host` funcionara sobre virtio-gpu (redroid-doc #705, cerrado sin
arreglo). Aquí la fase 0 lo dice en un día.

### 4.3 Dorados y restore

- **Hoy no hay dorado con GPU 3D en ningún VMM** (§0). El teléfono `-gpu` arranca
  en frío desde la imagen: ~11–14 s hasta `boot_completed` (el contenedor con
  GPU tarda 10,4–11,3 s; Firecracker con verity 12,5–13,8 s).
- La identidad del clon (`kling-phoned identity`, gancho `post-restore.d`) tiene
  que correr también en el **primer arranque**: `phoned` la aplica si MMDS trae
  identidad y aún no se aplicó. Es el mismo código, otro disparador.
- `freeze` no existe para estas máquinas; `pause` sí (`crosvm suspend`: la RAM
  sigue ocupada, la GPU queda quieta). El scheduler solo pausa.
- Experimento de la fase 0, con pocas esperanzas: dorado **antes** de crear
  contextos 3D (Android parado antes de SurfaceFlinger, dispositivo virtio-gpu
  presente pero sin contextos) y snapshot de crosvm. Si crosvm lo acepta y
  restaura, el clon ahorra kernel e init (~2–4 s), no zygote ni system_server.
  Si no lo acepta, se descarta sin más.
- Cuando crosvm (rutabaga) o QEMU sepan guardar el estado 3D, `kling-cvm`
  anuncia `snapshot:true` y el núcleo no cambia; la memoria compartida seguiría
  pendiente porque el snapshot de crosvm serializa la RAM en vez de mapearla.

### 4.4 Densidad

- Admisión: con `mem_shared:false` el núcleo cuenta `mem_mib` entero por máquina
  (como en el Mac).
- Globo con *free page reporting* de crosvm para devolver lo que Android libera;
  `kling machine squeeze` sí tiene sentido aquí (no hay dorado que desbaratar,
  al contrario que en `proxmox.md` §4).
- KSM: medir en la fase 0 con 3–5 teléfonos iguales (`/sys/kernel/mm/ksm/pages_sharing`
  con `run=1` forzado solo durante la prueba, que es lo que haría `ksmtuned` bajo
  presión). Si recupera >20 %, documentarlo como ajuste del host.
- Límite de GPU: el mismo reparto que mide `gpu.md` (`drm-engine-render` del
  `fdinfo`); `kling top` enseña `gpu_busy_pct` por máquina.

### 4.5 Cómo se elige por teléfono

```sh
kling phone up tel-1              # Firecracker, desde el dorado, CPU
kling phone up tel-2 -gpu         # crosvm + virgl (o venus si el host lo tiene), en frío
kling phone up tel-3 -gpu venus   # exige venus; error claro si el host no lo ofrece
```

Hoy no existe `kling phone` (es `phone.sh`); la bandera va primero en
`phone.sh up -gpu` y pasa al CLI cuando exista. Por debajo: `POST /machines`
con `vmm: crosvm, gpu: virgl` y la imagen `android13gpu`; el daemon comprueba
con `GET /kling/info` de un `kling-cvm --probe` que el host tiene
`renderD128` (y `udmabuf` + ANV para venus) y contesta 400 con qué falta. `kling
ps` enseña la columna `GPU`.

## 5. Plan por fases

### Fase 0 — prototipo a mano, sin código en kindling (2–3 días)

En el **CT 106** (ya tiene `/dev/kvm` y `/dev/dri`) o en un CT desechable como el
190 de `gpu.md`. Nada en el host.

| # | prueba | criterio de éxito |
|---|---|---|
| 0.1 | compilar crosvm (`--features gpu,virgl_renderer`) en un CT o en Lima; instalar `libvirglrenderer1` en el CT | `crosvm run` arranca la imagen `min` con virtio-gpu 2D |
| 0.2 | kernel `gpu-crosvm` + imagen Android `GPU=virgl`, a mano con `crosvm run` | `boot_completed=1`; `dumpsys SurfaceFlinger` dice `virgl` / `Mesa Intel UHD 630`; crosvm abre `renderD128` |
| 0.3 | un teléfono: arrastre 30 s en Ajustes y el WebGL de `gpu.md` | UI ≥ 58 fps con **≤ 0,4 núcleos** (cgroup de crosvm); WebGL **≥ 30 fps** |
| 0.4 | RAM por teléfono (PSS de crosvm y sus procesos, `MemAvailable` del CT) con 1, 3 y 5 | **≤ 1,6 GiB** por teléfono; con KSM forzado, cuánto baja |
| 0.5 | 5 teléfonos a la vez, 1 h de UI continua | los 5 a ≥ 58 fps, sin "GPU HANG" en el journal del host (lo mira el usuario), ningún crosvm muerto |
| 0.6 | snapshot de crosvm con GPU 3D (y el dorado "antes de SurfaceFlinger" de §4.3) | se anota si funciona; que falle no para el plan |
| 0.7 | (si el usuario acepta §6.1) venus con ANV en el CT | Vulkan `virtio` en `dumpsys`; mismos criterios que 0.3–0.5 |

**Salida**: una sección nueva en este documento con las cifras. **Se para si**
0.3 o 0.4 no se cumplen: el contenedor (A) y Firecracker (I) cubren entonces los
dos extremos mejor que una VM con GPU.

### Fase 1 — `kling-cvm` con un daemon privado (1–2 semanas)

| prueba | criterio |
|---|---|
| `KLING_VMM=/usr/local/bin/kling-cvm kling daemon -root /var/lib/kindling-gpu -socket /run/kling-gpu.sock` con `daemon.vmm` sin tocar el daemon del sistema | `kling run/ps/exec/logs/stop/rm` y `machine ready` funcionan con la imagen `min` y con la Android |
| MMDS | `kling machine secret` y la identidad del teléfono llegan (3 teléfonos con `android_id` distintos, que no aparecen en logs ni `state.json`, como en `proxmox.md`) |
| egress `none` / `allowlist` | `curl` a Internet falla con `none`; con `allowlist` solo sale a lo listado; MMDS responde en los dos |
| cgroup | `cpu.max` del teléfono se respeta (`-cpu-pct 200` no pasa de 2 núcleos) |
| `kling save` / `freeze` | 409 con mensaje claro; ningún fichero a medias |
| 20 ciclos `run`/`rm` | sin netns, tap, procesos de crosvm ni memoria de GPU huérfanos (`fdinfo` del i915 sin clientes de más) |
| tests | `go test ./cmd/kling-cvm/...` con un crosvm falso (como los de `kling-vz`) para la traducción de la API |

### Fase 2 — integración en el núcleo (1–2 semanas)

- `VMMCrosvm` en `pkg/config`, capacidades por backend en `internal/machine`,
  campo `vmm`/`gpu` por máquina y el `Manager` con dos backends a la vez.
- `phone.sh up -gpu` (y `kling phone up -gpu` si existe), imagen `android13gpu`
  construida por `build-image.sh GPU=virgl`, `kling-phoned` con `gpu` en
  `/v1/health` y la sonda que falla si cae a SwiftShader.
- `test-phone-gpu.sh` e2e en el CT: un teléfono Firecracker desde el dorado y
  uno `-gpu` en el mismo daemon; los dos pasan `fase0`-equivalente, el `-gpu` con
  `GL_RENDERER` de virgl y UI ≤ 0,4 núcleos.
- Criterio: `verificador-kindling` APTO; ningún cambio de comportamiento para
  máquinas sin `gpu` (los e2e de siempre pasan igual).

### Fase 3 — producción

- **venus por defecto** para `-gpu` (renderizador aislado); virgl solo si el host
  no tiene ANV/udmabuf, con aviso.
- Seguridad: crosvm con su sandbox activado (nunca `--disable-sandbox`),
  `kling-cvm` sin privilegios, perfil seccomp revisado; entrada en `SECURITY.md`
  sobre la superficie de virglrenderer; fuzzing no, pero fijar versiones de
  crosvm y virglrenderer por sha256 como Firecracker.
- Recuperación: si la GPU se cuelga (reset del i915) los teléfonos `-gpu` se
  reinician solos (`phoned` ya relanza Android; crosvm muerto → la máquina pasa a
  `failed` y el scheduler la recrea).
- Observabilidad: `kling top` con `gpu_busy_pct`, admisión que rechace un
  teléfono `-gpu` más si la GPU va > 80 % de media en 1 min.
- Criterio: 24 h con N teléfonos `-gpu` (N de la fase 0) y 20 Firecracker a la
  vez en el host, sin muertes ni fugas; doc de operación en este fichero.

## 6. Cambios en el host `pve` — **decisión del usuario**

Nada de esto se ha aplicado. Por orden de cuánto toca:

| # | cambio | para qué | alcance | reversible |
|---|---|---|---|---|
| 6.1 | instalar `mesa-vulkan-drivers` **dentro del CT** que corra crosvm, y pasarle `/dev/udmabuf` (`lxc.cgroup2.devices.allow: c 10:259 rwm` + `lxc.mount.entry: /dev/udmabuf ...`) | venus (C) | config del CT, reinicio del CT | sí |
| 6.2 | nada | virgl (B) en el CT 106: ya tiene `/dev/kvm` y `/dev/dri` | — | — |
| 6.3 | `binder_linux` persistente (`/etc/modules-load.d/`) | solo para el contenedor (A); hoy está cargado en caliente hasta el próximo reinicio | host | sí |
| 6.4 | ajustar KSM (`ksmtuned.conf`) | si la fase 0 muestra que recupera memoria | host | sí |
| 6.5 | GVT-g: `i915.enable_gvt=1` en la cmdline, módulos `kvmgt vfio_mdev`, apertura mayor en la BIOS, reinicio (la IOMMU ya está activa por defecto: no hace falta `intel_iommu=on`) | G | **host entero, reinicio y BIOS** | sí, con otro reinicio |
| 6.6 | instalar `mesa-vulkan-drivers` en el **host** | venus con QEMU de Proxmox (E) | host | sí |

**No recomendado**: 6.5 (abandonado y con informes de rotura en 6.8+) y 6.6 (no
hace falta si crosvm corre en un CT).

## 7. Fuentes (consultadas el 2026-09-29)

- crosvm, libro: GPU y Wayland (compilar con `--features gpu`, modos por
  `context-types`), <https://crosvm.dev/book/devices/wayland.html>; snapshot
  "highly experimental", <https://crosvm.dev/book/architecture/snapshotting.html>.
- crosvm, código de virtio-gpu (snapshot solo en 2D, "3D is WIP"),
  <https://crosvm.dev/doc/src/devices/virtio/gpu/virtio_gpu.rs.html>; capsets
  `VIRGL2`, `VENUS`, `GFXSTREAM`, `CROSS_DOMAIN`,
  <https://github.com/google/crosvm/blob/main/devices/src/virtio/gpu/protocol.rs>.
- Cuttlefish: modos de GPU (`gfxstream`, `drm_virgl`, SwiftShader),
  <https://source.android.com/docs/devices/cuttlefish/gpu>; snapshot solo con
  `guest_swiftshader`, <https://source.android.com/docs/devices/cuttlefish/snapshot-restore>.
- Mesa, venus (Vulkan 1.1, extensiones, kernel ≥ 5.16, ANV 21.1+, RADV, NVIDIA
  570.86+), <https://docs.mesa3d.org/drivers/venus.html>.
- Collabora, estado de virgl, venus y DRM native context (venus en proceso
  aislado), 2025-01-15,
  <https://www.collabora.com/news-and-blog/blog/2025/01/15/the-state-of-gfx-virtualization-using-virglrenderer/>;
  ChromeOS sobre venus, <https://chromeos.dev/en/posts/improving-vulkan-availability-with-venus>.
- QEMU virtio-gpu (virgl, venus con `blob=on,hostmem=,venus=on` desde 9.2,
  rutabaga/gfxstream), <https://www.qemu.org/docs/master/system/devices/virtio/virtio-gpu.html>;
  bloqueo de migración con virgl,
  <https://patchwork.ozlabs.org/project/qemu-devel/patch/1469123413-20809-55-git-send-email-mst@redhat.com/>;
  Proxmox, suspensión y snapshots con `virtio-gl`,
  <https://forum.proxmox.com/threads/display-type-virgl-gpu-virtio-gl-issues-w-suspend-snapshots.136976/>;
  configuración de VMs, <https://pve.proxmox.com/wiki/Manual:_qm.conf>.
- Firecracker: GPU y PCIe (en pausa desde 02/2025, primer hito vfio sin
  snapshots, memoria preasignada), <https://github.com/firecracker-microvm/firecracker/discussions/4845>;
  <https://github.com/firecracker-microvm/firecracker/issues/849>.
- cloud-hypervisor: virtio-gpu solo con el parche de Spectrum,
  <https://spectrum-os.org/software/cloud-hypervisor/>,
  <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/3212>.
- GVT-g: ArchWiki (Gen5–Gen10, proyecto abandonado),
  <https://wiki.archlinux.org/title/Intel_GVT-g>; Proxmox, estado en kernels
  6.8+ (abril de 2026), <https://forum.proxmox.com/threads/status-of-mediated-devices-gvt-g.182937/>.
- Redroid: parámetros (`redroid_gpu_mode`, `redroid_gpu_node`),
  <https://github.com/remote-android/redroid-doc>; `gpu_mode=host` sobre
  virtio-gpu en QEMU sin funcionar, <https://github.com/remote-android/redroid-doc/issues/705>.
- Propias: [`gpu.md`](gpu.md), [`proxmox.md`](proxmox.md),
  [`phoned.md`](phoned.md), [`densidad.md`](densidad.md),
  [`docs/backend-vz.md`](../../../docs/backend-vz.md).
