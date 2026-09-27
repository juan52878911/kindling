# android — fase 0: Android (Redroid 13) en una microVM de kindling sobre `vz`

Prototipo de la **fase 0** de la propuesta "un celular Android eficiente sobre
kindling" (opción c: Redroid dentro de una microVM Linux de kindling, §1.1, §4 y
§5 de la propuesta). El anfitrión elegido es **el Mac Apple Silicon con el
backend `vz`** ([`docs/mac.md`](../../docs/mac.md)): los invitados son arm64, así
que las apps Android arm64 corren **nativas**, sin traducción.

**Pregunta.** ¿Arranca Android 13 (Redroid, 64 bits) dentro de una microVM de
kindling en `vz`, sin Docker, con `kling-guest` como PID 1? ¿Cuánto tarda en
frío, cuánto en volver desde un dorado, cuánto cuestan `uiautomator dump` y
`screencap`, cuánta memoria come cada clon, abre un APK arm64 y funciona
`sandbox fork`?

**Nada del núcleo cambia.** Todo vive en `prototypes/android/`. Lo que el
prototipo necesitó y el núcleo no tiene está en
[Hallazgos para el núcleo](#hallazgos-para-el-núcleo-encontrados-al-construir-esto)
y en [Qué necesitaría el núcleo si la fase 0 sale bien](#qué-necesitaría-el-núcleo-si-la-fase-0-sale-bien).

## Estado: qué está probado y qué no

**No se ha ejecutado en un Mac.** Se escribió en un contenedor Linux amd64 sin
KVM, sin macOS y sin binder. Lo que sí se comprobó ahí:

| Qué | Cómo | Resultado |
|---|---|---|
| Sintaxis y estilo de los 7 scripts | `bash -n` + `shellcheck` (`test-local.sh`) | limpio |
| Fragmento `config-android` sobre K1 | el builder del núcleo **sin modificar** (vía `kernel/build.sh`), `CONFIG_ONLY=1` y después compilación completa cruzada (gcc 13 aarch64) sobre el árbol v6.1.140 | aplica sin deriva propia; los dos comprobadores pasan; sale un `Image` arm64 de 9,0 MiB (magia `ARMd`) en 226 s con 4 núcleos. **No se ha arrancado** |
| Que cada símbolo del fragmento existe en 6.1.140 | `KSRC=... test-local.sh` | todos existen; `ANDROID`, `ASHMEM` e `ION` no (por eso no están) |
| Comprobador `check-android-config.sh` | configuraciones sintéticas buenas y malas, stdin | falla y pasa cuando toca |
| Digests de Redroid fijados | manifiesto pedido al registro por digest y su sha256 recalculado; la configuración y la capa (640 MiB) descargadas y verificadas | coinciden (`c815ac1b…`, `e4450fd6…`, `7ae0e911…`) |
| Contenido real de la imagen | la capa de arriba, inspeccionada | `/init` → `/system/bin/init` (enlace absoluto), ENTRYPOINT `/init qemu=1 androidboot.hardware=redroid`, APEX **aplanados**, sin bibliotecas de 32 bits, 1,35 GiB descomprimida, su `init.rc` monta binderfs y crea los tres binder él mismo (`binder_alloc`) |
| `build-image.sh` (montaje) | ejecutado en amd64 con un gancho de prueba (`KLING_ANDROID_TEST_ANYARCH=1`), la capa REAL de Redroid (`FETCH=local`), una base falsa (sin debootstrap: Debian no era alcanzable) y un `kling-guest` arm64 real | capa ext4 sana (`e2fsck`), receta, paquete y `SHA256SUMS` correctos; setuid, dueños numéricos y `security.capability` se conservan |
| `android-launch.sh` + `android-sh` | con un "Android" falso (busybox + un `init` de pega) en espacios de nombres reales | init es PID 1 de su espacio, recibe los argumentos, ve cgroup2 y devpts propios, red aislada (solo `lo`), nada se filtra a los montajes de la VM; `android-sh` encuentra init y zygote, hereda su entorno, propaga el código de salida, pasa binario intacto y `--push` funciona; `--kill-child` mata a Android si muere unshare; sin binderfs se para con `failed: ...` |
| `fase0.sh` | `--self-test` (informe, medianas, veredictos) y los lectores de JSON contra ejemplos con la forma de `api.ProcStats`/`api.ForkResult` | bien. **El resto solo corre en macOS** y no se ha ejecutado; tampoco con el bash 3.2 de macOS (se evitó a propósito todo lo de bash 4) |

Lo que **no** se ha podido comprobar y es la razón de ser de la fase 0: que el
kernel arranque en vz con este fragmento, que Android 13 llegue a
`boot_completed` sin Docker, que netd/bpfloader acepten este kernel, cualquier
tiempo o cifra de memoria. Lista completa de supuestos al final.

## Cómo encaja

```
Mac ── kling daemon PRIVADO (raíz ~/.kindling-android-fase0, backend vz)
        └─ kling-vz ── Virtualization.framework ── microVM arm64
             kernel: K1 + config-android (images/vmlinux de ESA raíz)
             vda: base android-base (Debian trixie, 71-build-glibc-base.sh)
             vdc: capa android13 (81-base-image.sh) · vdb: overlay de 512 MiB
             PID 1: kling-guest (:8080 → exec, cp, resync, save)
               └─ android-launch.sh  (SERVICE del entrypoint, como llama-server en VON)
                    unshare -m -p -i -u -n ── chroot /android ── /init  (Redroid 13 64only)

kling exec <m> -- android-sh <cmd>   → nsenter en los espacios de Android
                                        → getprop, uiautomator, screencap, pm, am
```

- **Sin Docker dentro.** El lanzador hace lo que haría `docker run --privileged`:
  `/proc`, `/sys` en escritura, cgroup2, un `/dev` en tmpfs con los nodos del
  invitado, devpts propio, `/dev/shm`, mqueue, y `chroot` a `/init` con los
  argumentos de la imagen más los nuestros.
- **La red de Android va aislada** (su propio espacio de red, solo `lo`). Si netd
  tocara `eth0`, las rutas o iptables de la VM, `kling-guest` dejaría de ser
  alcanzable y con él exec, save y los reenvíos. Con `egress none` no se pierde
  nada. `ANDROID_NET=shared` al construir lo cambia.
- **El acceso es `kling exec`** + `android-sh` (hace de `adb shell` sin adb ni
  puertos): `POST /machines/{ref}/exec` ya existe, lleva la salida en base64
  (binario intacto) y necesita `-allow-exec`, que viaja en el dorado.
- **Daemon privado.** El kernel es uno por daemon (`images/vmlinux`,
  `internal/machine/manager.go` `KernelPath`) y un dorado se niega a restaurar
  sobre otro kernel (K2, `comprobarKernel`). Cambiar el del daemon de siempre
  invalidaría sus dorados, así que `fase0.sh` arranca otro con su raíz y su
  socket. Es seguro: el barrido de huérfanos y el de enlaces cortos filtran por
  raíz (`internal/machine/procesos_vz.go`, `internal/fc/dial.go`).

## Ficheros

| Fichero | Dónde corre | Qué hace |
|---|---|---|
| `kernel/config-android` | — | fragmento sobre `config-common` + `config-arm64`: binder + binderfs (`binder,hwbinder,vndbinder`), memfd, DMA-BUF heaps, sync_file, PSI, evdev/uinput, KEYS, lo que netd da por hecho (netfilter/iptables, tablas de políticas, tc+BPF, SOCK_DESTROY), loop. Cada bloque dice de dónde sale |
| `kernel/build.sh` | Linux (arm64, o amd64 cruzado) | compone config-common + config-arm64 + config-android **ejecutando una copia literal del builder K1** en un árbol de sombra (el builder no acepta un fragmento extra) y deja `vmlinux-6.1.140-kindling-arm64-android` |
| `kernel/check-android-config.sh` | Linux o dentro del invitado | obligatorias (falla) y recomendadas (avisa) para Android; el `build.sh` de aquí lo encadena al comprobador del núcleo; el lanzador lo corre contra `/proc/config.gz` |
| `image/build-image.sh` | Linux arm64, root (la VM Lima vale; **no** hace falta virtualización anidada ni daemon) | baja Redroid 13 64only **por digest** y verifica cada pieza, construye la base glibc, monta la capa con `81-base-image.sh` (ROOTFS_DIR + SERVICE, como el constructor `llm`), escribe la receta y empaqueta un `.tar` con `SHA256SUMS` |
| `image/android-launch.sh` | dentro de la microVM | el lanzador (arriba) |
| `image/android-sh` | dentro de la microVM | ejecutar dentro de Android; `--push`, `--pid`, `--state` |
| `fase0.sh` | el Mac | la prueba completa y el informe (`results/<fecha>/resultados.md`) |
| `test-local.sh` | cualquier Linux | lo que se puede validar sin Mac |

## Paso a paso

Tiempo total estimado: ~1 h la primera vez (kernel ~5–30 min según dónde;
imagen ~10 min; fase 0 ~15–30 min).

### 0. Requisitos

- **Mac**: Apple Silicon, macOS 14+, kindling instalado con el backend `vz`
  (`make install`, `make vz`, `brew install e2fsprogs`; `kling up -check` sin
  errores, ver [`docs/mac.md`](../../docs/mac.md)). ~12 GiB libres en disco.
  Cierra lo que puedas: cada VM restaurada ocupa **toda su RAM** (abajo).
- **Una VM Linux arm64** para construir. Sirve la de
  [`docs/mac-arm64.md`](../../docs/mac-arm64.md) (`kindling-arm`), pero **sin**
  `nestedVirtualization` también vale (no se usa KVM ni el daemon): funciona en
  M1/M2. Ojo: la receta de `docs/mac-arm64.md` trae `nestedVirtualization: true`,
  que solo funciona en M3+ con macOS 15; en M1/M2 **borra esa línea** del yaml o
  `limactl start` falla. ~15 GiB libres dentro. Su home del Mac va montado en solo lectura:
  las salidas van a `/var/tmp` y se sacan con `limactl copy`.

```sh
limactl shell kindling-arm
sudo apt-get update
sudo apt-get install -y build-essential flex bison bc libelf-dev xz-utils curl gnupg \
                        debootstrap debian-archive-keyring e2fsprogs python3
```

### 1. El kernel (dentro de Lima)

`kernel.pin` fija 6.1.140 con el sha256 **vacío** a propósito: el builder no
compila sin él. Sácalo de la lista **firmada** de kernel.org:

```sh
cd /Users/<tú>/<ruta>/kindling                     # el checkout del Mac, montado
curl -fsSLO https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc
gpg --locate-keys autosigner@kernel.org            # comprueba la huella en https://www.kernel.org/signature.html
gpg --verify sha256sums.asc                        # tiene que decir "Good signature"
grep ' linux-6.1.140.tar.xz$' sha256sums.asc       # ← este es el valor

KERNEL_SHA256=<valor> OUT=/var/tmp/android-kernel prototypes/android/kernel/build.sh
```

`KERNEL_SHA256=` en el entorno no toca el repo; si prefieres dejarlo fijado,
escríbelo en `scripts/builders/kernel/kernel.pin` (es el paso previsto por K1).
Deja `/var/tmp/android-kernel/vmlinux-6.1.140-kindling-arm64-android` y su
`.sha256`. Busca en la salida la línea `android kernel config ...: ok`. La
deriva que imprime el builder (`ARM64_BTI_KERNEL`, `BPF_JIT`...) es del K1 de
siempre, no de este fragmento (ver hallazgos).

(También compila cruzado en un Linux amd64 con `gcc-aarch64-linux-gnu`; así se
validó aquí.)

### 2. El agente de invitado, arm64 (en el Mac)

```sh
cd <kindling> && make guest GOARCH=arm64     # deja ./kling-guest (linux/arm64, estático)
```

`build-image.sh` lo encuentra ahí (el checkout se ve desde Lima) o en
`/usr/local/lib/kindling/kling-guest`; si no, `KLING_GUEST=/ruta`.

### 3. La imagen (dentro de Lima)

```sh
cd /Users/<tú>/<ruta>/kindling
sudo DATA_MODE=tmpfs DATA_SIZE=1G prototypes/android/image/build-image.sh
```

**Recomendado: `DATA_MODE=tmpfs`.** El disco escribible de cada máquina está
fijo en 512 MiB (`defaultOverlayMiB`), y el primer arranque de Android más el APK
de prueba y su extracción se acercan a ese límite
(`INSTALL_FAILED_INSUFFICIENT_STORAGE`). Con tmpfs, `/data` vive en la RAM del
invitado y se congela con el snapshot.

Descarga 640 MiB de Docker Hub (verificados por sha256), construye la base
Debian con `debootstrap` (unos minutos, solo la primera vez) y deja
`/var/tmp/kindling-android/out/android-fase0-<fecha>.tar` (+ `.sha256`).

- Si Docker Hub contesta **429** (límite sin cuenta): espera una hora, o
  `docker login` y `sudo FETCH=docker ...`.
- Variables útiles: `DATA_MODE=tmpfs DATA_SIZE=2G` (ver "Problemas"),
  `WIDTH/HEIGHT/DPI/FPS`, `EXTRA_ARGS="ro.xxx=..."`, `ANDROID_NET=shared`.

### 4. Llevarlo al Mac

```sh
limactl copy kindling-arm:/var/tmp/android-kernel/vmlinux-6.1.140-kindling-arm64-android ~/Downloads/
limactl copy kindling-arm:/var/tmp/android-kernel/vmlinux-6.1.140-kindling-arm64-android.sha256 ~/Downloads/
limactl copy kindling-arm:/var/tmp/kindling-android/out/android-fase0-<fecha>.tar ~/Downloads/
limactl copy kindling-arm:/var/tmp/kindling-android/out/android-fase0-<fecha>.tar.sha256 ~/Downloads/
```

No es `kling image copy` a propósito: `copy` lleva siempre el kernel del daemon
de origen y necesita un daemon de kindling corriendo en Lima (ver hallazgos). `fase0.sh`
comprueba los dos sha256 antes de usar nada.

### 5. La fase 0 (en el Mac)

```sh
cd <kindling>/prototypes/android
./fase0.sh -kernel ~/Downloads/vmlinux-6.1.140-kindling-arm64-android \
           -bundle ~/Downloads/android-fase0-<fecha>.tar
```

Opciones: `-apk mi.apk` (por defecto descarga **Termux 0.118.3 arm64-v8a** de su
release de GitHub y verifica sha256 `72fdb596…a4e`: trae `libtermux.so` y
`libtermux-bootstrap.so` nativas), `-clones N` (5), `-mem 3G`, `-cpus 2`,
`-keep` (no borra nada al acabar, deja el daemon privado vivo), `-force` (no
recorta clones por memoria). Para mirar a mano mientras corre o con `-keep`:

```sh
export KLING_HOST="unix://$HOME/.kindling-android-fase0/kling.sock"
kling ps
kling exec a0-clone-1 -- android-sh getprop sys.boot_completed
kling exec a0-clone-1 -- android-sh screencap -p > pantalla.png
kling exec a0-clone-1 -- tail -f /var/log/service.log      # el lanzador
```

### 6. Devolver los resultados

Manda `results/<fecha>/resultados.md`. Si algo falló, también
`results/<fecha>/raw/diag-*` y `fase0.log` (un `tar czf` de la carpeta sin los
PNG pesa poco).

## Criterios go/no-go (propuesta §4, ajustados al Mac)

> **Lo que la fase 0 en el Mac NO decide: la densidad.** En `vz`, restaurar copia
> toda la RAM del invitado (`vz/internal/vzvm/vzvm_darwin.go`,
> `docs/vz-mac-prototipo.md`): cada clon de 3 GiB cuesta ~3,2 GiB de footprint,
> así que en 16 GiB caben 2–3 teléfonos, 5–6 en 32 GiB. El criterio 3 es solo
> informativo aquí; la puerta de densidad de la propuesta (páginas compartidas
> entre clones) sigue necesitando Linux/Firecracker. Para meter más teléfonos en
> el Mac: `-mem 2G`, `squeeze` tras restaurar y teléfonos pausados en vez de
> restauraciones.

| # | Criterio | Umbral | Por qué así en el Mac |
|---|---|---|---|
| 1 | `sys.boot_completed=1` en frío | ≤ 120 s | igual que la propuesta; aquí sin virtualización anidada debería sobrar |
| 2 | `run -from` → primer `uiautomator dump` correcto | ≤ 3 s (p50) | igual; incluye restaurar 3 GiB (en vz se copian a memoria) |
| 3 | memoria por clon extra | **informativo** | el ~350 MiB/VM conocido es de una VM de 256 MiB: en vz restaurar copia la RAM entera (`docs/vz-mac-prototipo.md`: 128 MiB → 267, 256 → 397). Para 3 GiB lo esperable es ~3 GiB + ~100 MiB por clon, menos lo que comprima macOS. El informe dice cuántos teléfonos caben en el 70 % de la RAM de tu Mac y lo que deja `kling machine squeeze` |
| 4 | `uiautomator dump` y `screencap -p` | ≤ 2 s cada uno (p50) | medido de extremo a extremo desde el Mac (CLI + exec + nsenter + la herramienta), que es lo que pagaría una extensión `kling phone` |
| 5 | un APK arm64 nativo abre | su actividad en primer plano | en vz es nativo; las apps solo-32 bits no pueden correr en Apple Silicon |
| 6 | `kling sandbox fork` | la copia contesta a dump | fork = commit de la VM viva + restaurar (`internal/machine/fork.go`) |

PSS no existe en macOS (no hay `/proc/<pid>/smaps`). Se mide `phys_footprint`
(memoria sucia + comprimida atribuida a un proceso), que es lo que da
`kling top -json` en `vz` sumando `kling-vz` y el proceso auxiliar
`com.apple.Virtualization.VirtualMachine` que aloja la VM
([`docs/backend-vz.md`](../../docs/backend-vz.md) §3). En vz una restauración no
comparte páginas con el dorado, así que PSS y footprint dirían lo mismo. Aparte
se guardan `ps` (RSS), `vm_stat`, `sysctl vm.swapusage kern.memorystatus_level`
y, si existe, `footprint`.

## Problemas esperables (síntoma → causa)

- **`android-sh --state` dice `failed: kernel without binderfs`**: la VM arrancó
  con otro kernel. Mira `images/vmlinux` en la raíz privada y su sha256.
- **Nunca llega `boot_completed`**: `raw/diag-a0-cold/` tiene consola,
  `service.log`, `dmesg`, `logcat`, `getprop` y `ps -A` de Android. Lo típico:
  - `servicemanager`/`hwservicemanager` en bucle, "Binder driver could not be
    opened" → binder/binderfs;
  - `netd` "Unable to start" o `bpfloader` con `reboot,bpfloader-failed` → falta
    algo de netfilter/BPF/tc; subir la recomendada que diga el log a obligatoria;
  - avisos de lmkd sobre PSI → `CONFIG_PSI`;
  - `surfaceflinger`/`composer` muertos → render por software (Redroid "guest":
    ANGLE sobre el Vulkan por software "pastel"); `dumpsys SurfaceFlinger`.
- **`screencap` negro o `uiautomator` "could not get idle state"**: la pantalla
  duerme o hay animaciones; `fase0.sh` ya pone `svc power stayon`, quita el
  bloqueo y las animaciones.
- **Render por CPU**: 15 FPS por defecto sin GPU. Vale para dumps y capturas, no
  para vídeo o juegos. `vz` no expone GPU al invitado.
- **`/data` lleno** ("No space left" al instalar): `/data` va al overlay de la
  VM, que en kindling mide **512 MiB fijos** (`defaultOverlayMiB`). Reconstruye
  con `DATA_MODE=tmpfs DATA_SIZE=2G` (cuenta contra la RAM del invitado).
- **Memoria**: 5 clones de 3 GiB son ~15 GiB en el peor caso. `fase0.sh` mira
  lo disponible y recorta clones (dice cuántos en el informe); con `-force`
  insiste y macOS puede matar VMs (jetsam: "Internal Virtualization error"). El
  daemon también se niega (507) por debajo del 15 % libre
  (`KLING_MIN_MEM_LEVEL`).
- **Arranques simultáneos**: vz tiene compuerta de 4 (`KLING_MAX_PARALLEL_BOOT`)
  y el prototipo vio fallos con ~20 restauraciones a la vez. Los clones se
  restauran en serie.
- **Google Play / GMS y Play Integrity**: no vienen, y no se deben añadir a mano
  (licencia de GMS por dispositivo; Play Integrity no da
  `MEETS_DEVICE_INTEGRITY` en un dispositivo no certificado). Apps de banca y de
  operadoras pueden negarse a funcionar. Propuesta §5.
- **Apps solo-32 bits**: `INSTALL_FAILED_NO_MATCHING_ABIS`. Apple Silicon no
  ejecuta AArch32 y la imagen es 64only.
- **Identidad de los clones**: todos despiertan con el mismo `android_id`,
  claves y semillas (`internal/machine/fork.go`). El informe cuenta cuántos
  `android_id` distintos hubo (se espera 1). Es para la fase 1 (hook post-thaw).
- **Sin SELinux**: el kernel no lo trae; Android corre sin él (como Redroid en
  anfitriones con AppArmor). La frontera de seguridad es la VM, no Android.
- **Reloj**: tras restaurar, kindling resincroniza reloj y entropía del
  invitado (`POST /resync`); Android ve un salto de hora.

## Qué responde la fase 0

1. ¿Arranca Redroid 13 sin Docker en una microVM `vz` con K1 + un fragmento, y
   qué opciones de kernel resultan imprescindibles de verdad (las "recomendadas"
   que hagan falta pasan a obligatorias)?
2. ¿Cuánto tarda en frío en el hierro del Mac, y cuánto desde un dorado?
3. ¿Cuánto cuesta en memoria cada teléfono en `vz` y cuántos caben en tu Mac?
   ¿Cuánto recupera `squeeze` sin romper Android?
4. ¿Son `uiautomator dump` y `screencap` lo bastante rápidos para percibir la
   pantalla (JEV) sin un servicio residente dentro de Android?
5. ¿Abre un APK arm64 nativo? ¿Funciona `sandbox fork` con un invitado de 3 GiB?
6. ¿Cuánto ocupa `/data` tras el arranque y una instalación (¿basta el overlay de
   512 MiB?)?

## Qué necesitaría el núcleo si la fase 0 sale bien

1. **Kernel por imagen, no por daemon.** Hoy hay un `images/vmlinux` por daemon;
   un teléfono obliga a elegir entre invalidar los dorados de siempre (K2) o un
   daemon aparte. La receta podría llevar `kernel` (K2 ya guarda el sha256 en el
   sello) y `copy` moverlo con la imagen.
2. **`config-android` en el builder K1** como variante opcional
   (`EXTRA_FRAGMENTS=` o `FLAVOR=android` en `scripts/builders/kernel/build.sh`)
   y un perfil en `scripts/check-kernel-config.sh` (`--profile android`), en vez
   del árbol de sombra de aquí. Separado del K1 normal: los dorados de MCP/VON
   siguen con el kernel mínimo.
3. **Constructor `android`** en `scripts/builders/` (o que el constructor `base`
   acepte `rootfs`/`service` en su `BaseSpec`), con la descarga OCI por digest en
   Go y verificada, como `fetchVerified` del constructor `llm`.
4. **`kling image copy` sin kernel** (o con el kernel de la imagen), y un camino
   documentado para construir imágenes sin un daemon Linux completo (la receta
   de `docs/mac-arm64.md` asume M3+ con virtualización anidada).
5. **Tamaño del overlay por máquina** (`defaultOverlayMiB` = 512 fijo) o un
   `/data` por clon: Android escribe mucho más que un servidor MCP.
6. **Listo = `boot_completed`, no puerto 8080.** `kling save` y `sandbox fork`
   esperan al agente; para un teléfono haría falta una sonda por etiqueta
   (`kling.ready-cmd=...`) para que el dorado no se congele a medio arrancar.
7. **Hook post-restauración** (p. ej. etiqueta `kling.post-thaw=...` que el daemon
   ejecute tras `/resync`) para la identidad de cada clon: `android_id`, serie,
   claves de ADB.
8. **Extensión `ext/phone`** (propuesta §1.1.4): `create/screen/dump/tap/swipe/
   type/install/open/fork` sobre `POST /machines/{ref}/exec`, con `android-sh`
   dentro de la imagen.
9. **Pantalla de verdad en `vz`**: hoy `kling-vz` solo engancha consola, disco,
   red, globo y entropía (`vz/internal/vzvm/vzvm_darwin.go`). Dos caminos: un
   `VZVirtioGraphicsDeviceConfiguration` + entrada virtio en `kling-vz` (y DRM en
   el kernel, hoy prohibido por `check-kernel-config.sh`), o sin tocar vz el VNC
   que Redroid ya trae (`ro.boot.use_redroid_stream=1`, `use_redroid_vnc=1`,
   `vendor/etc/init/vncserver.rc`) expuesto con `kling.ports` y red compartida.
10. **Memoria en `vz`**: con restauraciones que copian 3 GiB, la densidad pide la
   política del Mac ya descrita en `docs/vz-mac-prototipo.md` (pausadas en RAM,
   globo mantenido tras restaurar, `mem-max`) o un anfitrión Linux/Firecracker
   para muchos teléfonos.

## Hallazgos para el núcleo (encontrados al construir esto)

- **El constructor `base` no admite `ROOTFS_DIR` ni `SERVICE` por la API**
  (`cmd/kling/builder.go`, `BaseSpec` solo tiene `packages` y `env`); solo el
  constructor `llm` los usa por dentro. Por eso `build-image.sh` llama a
  `81-base-image.sh` directamente, con los mismos parámetros que el `llm`.
- **`kling image copy` lleva siempre el kernel del origen**
  (`pkg/api/blobs.go`, `CopyImage` → `planCopia`) y pisaría un kernel distinto en
  el destino (si no hay máquinas vivas).
- **Un kernel por daemon** (`KernelPath`), con K2 negándose a restaurar sobre
  otro: correcto, pero obliga a un daemon aparte para un invitado con otro kernel.
- **Construir con `kling image build` pide un daemon en el Linux arm64**, y la
  receta de Lima (`docs/mac-arm64.md`) lo monta para M3+ con KVM anidado. No
  comprobé si el daemon arranca sin `/dev/kvm` (`config.ValidateVMM` solo mira el
  sistema operativo; `/dev/kvm` se consulta al informar y al bajar privilegios),
  así que no sé si en M1/M2 bastaría para construir. Este prototipo lo evita.
- **Overlay de 512 MiB fijo** (`internal/machine/manager.go`, `defaultOverlayMiB`).
- **`kling exec -i` admite 1 MiB de stdin** (`api.ExecMaxStdin`): un APK entra
  con `kling cp` y `android-sh --push`.
- **Deriva del K1 arm64 que ya existía**: con gcc 13 sobre 6.1.140, el builder del
  núcleo (sin este fragmento) avisa de `ARM64_BTI_KERNEL`, `ARM64_ERRATUM_1418040`,
  `ARM64_ERRATUM_2645198`, `BPF_JIT`, `BPF_JIT_ALWAYS_ON` (pedidas "y", salen "n")
  y `POWER_SUPPLY` (pedida "n", sale "y"). El fragmento Android no añade ninguna
  (78 opciones nuevas, ninguna quitada). Observado con un tarball rehecho desde
  la etiqueta v6.1.140 de git (kernel.org no era alcanzable desde aquí).

## Supuestos y lo que no se pudo verificar

- Que Redroid 13 arranque **sin Docker** con este lanzador. Se imitó lo que da
  `docker run --privileged`; Redroid solo se documenta con Docker/podman/k8s/LXC.
- Que el init de Redroid funcione sin SELinux en el kernel y sin debugfs,
  `/sys/power/state` ni `/proc/vmallocinfo` (su `init.redroid.rc` hace bind
  mounts sobre los dos últimos; deberían fallar sin parar el arranque).
- El bloque de netfilter/BPF/tc del fragmento sale de `android-base.config` de
  AOSP **de memoria**, no del fichero; no se sabe cuáles son imprescindibles.
- Que con `CONFIG_ANDROID_BINDER_DEVICES="binder,hwbinder,vndbinder"` (lo que
  pide redroid-doc) el `binder_alloc` del `init.rc` de Redroid, que crea esos
  mismos nodos, solo avise de que ya existen.
- `KEYS`, `UDMABUF`, `TUN`, `BLK_DEV_LOOP`: defensivas, sin prueba de que hagan falta.
- Que `uiautomator`, `am` y `pm` funcionen con el entorno copiado de zygote por
  `nsenter` (en la prueba con un Android falso el entorno llega; con el real no
  se ha visto).
- Los digests de Redroid vienen de la API de Docker Hub y se verificaron contra
  el registro (manifiesto, configuración y capa); la etiqueta es móvil, el
  digest no.
- El sha256 de Termux se calculó aquí sobre el fichero de GitHub; no hay una
  firma aparte que lo respalde. F-Droid no era alcanzable desde aquí.
- La clave con que kernel.org firma `sha256sums.asc` (`autosigner@kernel.org`) y
  el comando `gpg --locate-keys`: de memoria; comprueba la huella en
  kernel.org/signature.html.
- Que `footprint <pid>` exista en tu macOS (se usa solo si está) y que
  `kling top -json` en `vz` dé el footprint en `pss_mib` (así lo dice
  `docs/mac.md`; no se ha visto en un Mac desde aquí).
- `fase0.sh` no se ha ejecutado con el bash 3.2 de macOS.
- Tiempos de construcción: medidos solo en este contenedor amd64 (kernel
  cruzado 226 s con 4 núcleos; montaje de la capa real 64 s).

## Fuentes

Propuesta (secciones 1.1, 4 y 5) · [redroid-doc](https://github.com/remote-android/redroid-doc)
(README "Configuration", `deploy/README.md`, `deploy/centos.md`,
`deploy/debian.md`) · `device_redroid` rama `redroid-13.0.0` (`redroid.mk`,
`redroid.prop`) · la propia imagen `redroid/redroid@sha256:c815ac1b…` ·
[`docs/mac.md`](../../docs/mac.md), [`docs/backend-vz.md`](../../docs/backend-vz.md),
[`docs/vz-mac-prototipo.md`](../../docs/vz-mac-prototipo.md),
[`docs/mac-arm64.md`](../../docs/mac-arm64.md), [`docs/von.md`](../../docs/von.md) ·
`scripts/builders/kernel/`, `scripts/check-kernel-config.sh`,
`scripts/71-build-glibc-base.sh`, `scripts/81-base-image.sh`,
`scripts/minimal-init.sh`, `cmd/kling/builder*.go`, `pkg/api/blobs.go`,
`internal/machine/{manager,layer,snapshot,fork,procesos_vz}.go`, `pkg/guest`.
