# El constructor `android`: la imagen sin Lima, debootstrap ni e2fsprogs

Issue #90. `build-image.sh` necesita un Linux de la arquitectura de destino
(en el Mac, una VM Lima), root, `debootstrap`, `mkfs.ext4`/`resize2fs`/
`debugfs`, `veritysetup` y montar loops. El constructor `android` del núcleo
hace la misma imagen **en Go, sin nada de eso**: corre en el daemon de macOS
(como el usuario, sin Lima) y en el de Linux (como root, como los demás
constructores).

```sh
prototypes/android/image/spec.sh > spec.json          # los ficheros de este checkout
kling image build android13 -builder android -spec spec.json
kling run -image android13 -cpus 2 -mem 1536 -label kling.ports=5555,8091 -wait-ready
```

Deja `android13.layer.ext4` (Redroid + agente + lanzador, con el árbol de
dm-verity y el FEC pegados detrás), su base `android13-base.ext4` (glibc, con
el init que monta la capa por dm-verity y la tabla) y la receta con
`cpu_pct_per_vcpu: 100`, `guest_ipv6_stack: true`, la base y, en `built`, los
digests de lo descargado y la raíz, la sal y la tabla de verity.

## Qué hace, y con qué

| Paso | Antes (`build-image.sh` + `verity.sh`) | Ahora (Go, `internal/...`) |
|---|---|---|
| Redroid | `curl` + `python3` o Docker; sha256 de cada pieza | `oci`: manifiesto por digest, configuración y capas con sha256 y tamaño, token anónimo del registro, caché de blobs (manifiestos incluidos: reconstruir no necesita red) |
| capa | `mkfs.ext4` + overlay sobre la base montada + `tar -x` + `cp -a` + `resize2fs -M` | `ext4`: el tar se lee dos veces (estructura, luego datos directos a sus bloques), sin extraer nada al disco del host |
| dm-verity + FEC | `veritysetup format` + `veritysetup open` para sacar la tabla | `verity`: árbol sha256 y RS(255,253) bit a bit iguales a los de `veritysetup` (prueba con el resultado de cryptsetup 2.7) |
| base | `debootstrap` trixie + `apt-get install` en chroot | `debian:trixie-slim` por digest (`oci`) + los `.deb` fijados en `debian_lock.go` (`deb`, `xz`) |
| init de la base | `python3` que parchea `/sbin/overlay-init` en la base montada | el mismo bloque sobre `scripts/minimal-init.sh` (embebido) |
| receta | escrita a mano por el script | el daemon, con lo que el constructor deja en `recipe.json` (`api.BuildRecipeHints`) |

**El ext4** (`internal/ext4`) es un escritor propio: bloques de 4 KiB,
extents (con árbol cuando un fichero pasa de 4), `flex_bg`, sin journal, sin
`metadata_csum` y directorios lineales; dueños numéricos, modos con
setuid/setgid, horas con nanosegundos, dispositivos, enlaces duros y
xattrs en el inodo o en bloque (el `security.capability` de `run-as` y
`simpleperf_app_runner`). Un bloque de 4 KiB todo a ceros queda como hueco
(el `data.ext4` de 1 GiB no ocupa). `e2fsck -fn` limpio (1.47.0 y 1.47.4),
montable por el kernel, y `ext4.Read` lo relee (también lo que escribe
`mkfs.ext4`). Se descartó una biblioteca: el núcleo no tiene dependencias
fuera de la estándar, y la única escritora mantenida (el `tar2ext4` de
hcsshim) arrastra el módulo entero de hcsshim.

**La base.** `min` (Alpine) no sirve: el lanzador necesita `nsenter` e
`iptables` con glibc, y `min-glibc` sale de `debootstrap`. Se parte de
`debian:trixie-slim` (la imagen oficial, que ya trae util-linux: `nsenter`,
`unshare`, `mount`, `pivot_root`) y se le ponen encima 28 `.deb` fijados por
sha256: `iptables procps iproute2 dmsetup ca-certificates` y sus dependencias
(`go run ./internal/android/lockgen` las resuelve contra el `dpkg/status` de
la imagen y reescribe `debian_lock.go`). Si `deb.debian.org` ya no tiene un
paquete, se busca en `snapshot.debian.org` con la marca del día en que se
fijó.

**Entradas.** El constructor no sabe qué lanzador lleva la imagen: el spec
trae los ficheros (`files`: `src` en el host del daemon, o `content`), cuál
arranca el entrypoint (`service`) y `android.conf` (`conf`). `spec.sh` los
saca del checkout: con `PHONED=1` (por defecto) `kling-phoned` como servicio,
`kling-phoned ready` como sonda y su gancho de identidad; con `PHONED=0` el
lanzador de bash. uidump entra como un fichero más (`UIDUMP_DEX=`). Como
root, el constructor solo lee `src` de `/usr/local/lib/kindling` o de
`KLING_ANDROID_INPUTS` (en el entorno del daemon): si no, cualquiera con el
socket metería `/etc/shadow` del host en una imagen. Genera él: el agente de
invitado, `/entrypoint`, `entrypoint.args` (de ENTRYPOINT+CMD de la imagen
OCI), `android.conf`, `IMAGE.txt`, el `data.ext4` vacío (`data_ext4_mib`) y
lo de `slim` (props, servicios comentados, apps fuera, `features.xml`).

**Reproducible.** Con las mismas entradas y `SOURCE_DATE_EPOCH`, la capa y la
base salen iguales bit a bit (sal de verity y UUID derivados de las
entradas): comprobado construyendo dos veces (sha256 de las dos imágenes
iguales) y en `TestBuild`.

## Traducción ARM (`arm_translation`)

Issue #93, [traduccion-arm.md](traduccion-arm.md). Solo en amd64:

| `arm_translation` | Qué deja en la capa |
|---|---|
| `none` (por defecto) | **quita** el `libndk_translation` que ya trae Redroid 13 amd64 (con `libnb.so`) y deja las ABIs en `x86_64` en todas las `build.prop` (system, vendor y odm): una app solo-ARM da `INSTALL_FAILED_NO_MATCHING_ABIS` |
| `libndk` | quita el de Redroid y pone el de la imagen x86_64 del **emulador de Android 14** de Google (`x86_64-34_r14.zip`, fijada por URL, tamaño y sha256 en `pins.go`), más las propiedades del puente y un intérprete de binfmt_misc que arregla `argv[0]` |
| `redroid` | el de Redroid tal cual |

En arm64 no hay nada que traducir (`native` en la receta); `libndk` y `redroid`
son un error. Con `libndk` el constructor baja el zip (1,4 GiB) a
`cache/android/`, comprueba tamaño y sha256, lee el `system.img` en streaming
(GPT → `super` → ext4 `system`, sin escribir los 4 GiB), guarda las 90
entradas de la traducción en un tar de 29 MiB en la caché y borra el zip:
~35 s la primera vez, nada después. La receta (`built.layer.arm_translation`),
`IMAGE.txt` y `/v1/health` de kling-phoned dicen el modo, el puente y las
ABIs. El kernel necesita `CONFIG_BINFMT_MISC=y` para los ejecutables arm64.

**Licencia, con franqueza.** `libndk_translation` es un binario propietario
de Google sin licencia de redistribución. La imagen del emulador va bajo el
*Android SDK License Agreement*, que la da solo para desarrollar apps para
implementaciones compatibles de Android (Redroid no lo es) y prohíbe copiarla,
modificarla o redistribuirla. Por eso va **apagada por defecto**, el
repositorio no lleva ni un byte de ella (solo URL y sha256), la baja quien
construye y **una imagen con `libndk` no se publica** ni se copia a otro
daemon. La copia que trae Redroid tiene un origen aún menos claro: por eso el
defecto la quita. Houdini (Intel) no se toca. Los detalles, las cifras y
cuándo conviene un host ARM en su lugar: traduccion-arm.md.

## Igual que `build-image.sh`

`go run ./internal/ext4/ext4cmp -sub /upper/android -mtime REF.ext4 NUEVA.ext4`
compara fichero a fichero (tipo, modo con setuid, dueño, tamaño, sha256 del
contenido, destino de enlaces, dispositivos, xattrs y hora):

| Referencia (hecha por `build-image.sh`) | Resultado |
|---|---|
| arm64, `android13` (27-09, tmpfs, isolated) | 4 135 de 4 136 idénticos; difiere solo la hora de `/android` (la de extracción) |
| arm64, `android13slim` (`SLIM=1`) | 3 923 de 3 923 idénticos (contenido, modos, dueños, xattrs) |
| amd64, `android13` (28-09, en `phones`) | 4 235 de 4 240; difieren el `.dex` de uidump (otra versión) y horas de directorios donde se instaló |

Fuera de `/android`: los ficheros de entrada cambian con el checkout (lo
esperado), `IMAGE.txt` y `android.conf` tienen otro formato, y los
directorios que en la referencia se copiaron hacia arriba desde la base
(`/etc`, `/usr/local`...) llevaban `trusted.overlay.origin`/`impure`, que
aquí no se ponen: son del overlay que usó el script para construir, y sin
`opaque` el directorio se mezcla con el de la base igual.

La capa amd64 pasa `veritysetup verify` con el FEC (4,9 s en `phones`), y las
dos arrancan con la tabla del constructor: `/v1/health` de kling-phoned da
`"verity": "verified"` (estado `V` de device-mapper), `"net": "veth"`
(iptables de la base) y `"uidump": true`.

## Cifras (29-09-2026)

| | Mac M4 (vz, arm64) | `phones` (x86_64, Firecracker, amd64) |
|---|---|---|
| construir, caché caliente | 15–21 s (capa ~12 s, verity 1,4 s, base 1,7–2,9 s) | 57 s (con 28 `.deb` bajados y reintentos de TLS) |
| construir, caché fría | ~1,5 min (610 MiB de Redroid, ~25 s) | — (blob enlazado del de `build-image.sh`) |
| capa (PHONED=1, /data en overlay) | 1 419 MiB: 1 397 de ext4 + 22 de árbol y FEC | 1 591 MiB |
| base | 157 MiB (94 MiB reales) | 129 MiB (97 MiB reales) |
| arranque en frío hasta listo (`-wait-ready`) | 7,3–9,1 s | 16,3 s |

(Referencia: la base de `build-image.sh` son 186 MiB, la capa 2 560 MiB de
fichero con 1,4 GiB usados.)

## Lo que no es Go (y por qué)

- **El `.dex` de uidump**: lo compila `uidump/build.sh` con javac y d8. Es
  Java; entra al constructor como un fichero.
- **Los scripts de mantenimiento de los `.deb`** no se ejecutan (no hay
  chroot). Lo que hacen y hace falta se hace a mano: `iptables` apunta a
  `iptables-legacy` (el kernel no trae nf_tables), `ca-certificates.crt` y
  `ca-certificates.conf`, y el paquete queda apuntado en `dpkg/status` con su
  lista, md5sums y conffiles (apt en una capa derivada lo ve instalado). No se
  regenera `/etc/ld.so.cache`: ld.so busca igual en `/usr/lib/<triplete>`,
  donde están todas las bibliotecas añadidas.
- **El índice de Debian** que usa `lockgen` se baja por HTTPS sin comprobar la
  firma de `InRelease` (OpenPGP no está en la biblioteca estándar): lo que se
  revisa y queda fijado es el sha256 de cada `.deb`.
- **iptables** sigue en la base: lo usa kling-phoned para MASQUERADE y dos
  DROP (docs/phoned.md, "Por qué queda iptables").
- **El daemon de macOS** sigue usando e2fsprogs de Homebrew para sus
  overlays; el constructor no.
- **El kernel** se compila en CI (abajo): compilar Linux no tiene sentido en Go.

## Kernels

`.github/workflows/android-kernels.yml` compila en cada release publicada
(o a mano con `workflow_dispatch`) el kernel Android de arm64 (vz) y de amd64
(Firecracker) con `kernel/build.sh`, tras comprobar el tarball de kernel.org
contra `sha256sums.asc` **firmado** (claves de Linus, Greg y el autosigner,
por WKD), y sube a la release `vmlinux-<ver>-kindling-<arch>-android` con su
`.sha256`, `.config` y `.buildinfo`, más una atestación de procedencia
firmada con Sigstore. Para comprobar uno:

```sh
gh release download v0.18.0 -R juan52878911/kindling -p 'vmlinux-*-arm64-android*'
sha256sum -c vmlinux-*-arm64-android.sha256            # (shasum -a 256 -c en macOS)
gh attestation verify vmlinux-*-arm64-android -R juan52878911/kindling
```

La atestación dice qué commit y qué workflow lo produjeron; el `.buildinfo`,
con qué fragmentos, parches y compilador. El kernel sigue siendo uno por
daemon (`images/vmlinux`): el kernel por imagen queda para otro issue.

## Pendiente

- `kling phone golden inspect` (ext/phone, PR #120) debería enseñar
  `built.layer.arm_translation` y `abilist` de la receta.

- Kernel por imagen (hoy, un daemon aparte para Android).
- Publicar también la base como imagen OCI para no depender de
  `deb.debian.org`/`snapshot.debian.org` al construir.
