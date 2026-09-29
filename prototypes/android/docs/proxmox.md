# Teléfonos en x86_64 con Firecracker: el CT 106 "phones" de Proxmox

El prototipo (Redroid 13 en una microVM de kindling), llevado de arm64/`vz`
(Mac) a **x86_64/Firecracker** en un contenedor LXC dedicado. La pregunta
principal es la **densidad**: en Firecracker los clones de un dorado
comparten las páginas de su `mem.file` (MAP_PRIVATE), cosa que en `vz` no pasa.

Medido el 2026-09-28 en el CT 106 (`phones`, 192.168.2.20): LXC privilegiado,
Debian 12, kernel del host 6.17.2-1-pve, Intel i7-8700T (8 hilos), **6 GiB de
RAM + 1 GiB de swap** en el CT, `/dev/kvm` directo (sin virtualización
anidada). Firecracker **v1.17.0** (sha256 del tgz verificado contra su
`.sha256.txt`) y kindling v0.16.0-28 (esta rama), daemon del sistema.

## Resultado

- **Arranca**: `sys.boot_completed=1` en frío en **10,4–10,8 s** (2 vCPU,
  1536 MiB, `-cpu-pct 200`). Hicieron falta dos cambios en el kernel (abajo) y
  uno en `android-sh`.
- **24 teléfonos a la vez** de 1536 MiB en 6 GiB, los 24 sanos (adb,
  `boot_completed`, system_server vivo, `logcat -b crash` limpio). Paró el
  control de admisión de kindling, no Android ni el OOM.
- **Cada clon extra cuesta ~165 MiB con `-mem 1536` y ~290 MiB con `-mem
  1024`** (PSS de su firecracker; MemAvailable baja lo mismo), frente a
  ~2,1 GiB por teléfono en el Mac. Menos RAM de invitado sale **más cara**.
- **Sin páginas a ceros en Firecracker**: 2 clones del mismo dorado con otros 4
  vivos, 124 pasadas de lectura de 1,4 GB cada una contra referencia, y
  `--verify-cache` de 8 clones con el CT al límite: 0 diferencias. Lo del Mac
  (`docs/sigill.md`) no se reproduce aquí.
- **Termux x86_64 se instala y abre** en un clon (0,7 s instalar, 1,1 s abrir).
- **`kling machine squeeze` empeora las cosas aquí** y, con el CT lleno, lo
  dejó sin memoria y colgado (sección 4). No usarlo con clones de un dorado.

### Tiempos

Reloj de pared en el CT (CLI incluido), salvo "desde el Mac".

| | x86_64 Firecracker (CT 106) | arm64 `vz` (Mac M4, README) |
|---|---|---|
| kernel K1 + Android (8 hilos) | 2 min 15 s (make 118 s) | 226 s cruzado / 447 s Lima |
| capa `android13` (base y descarga en caché) | 54 s | — |
| `boot_completed` en frío | **10,4–10,8 s** | 3,6 s |
| `android-sh --verify-cache` (1711 ficheros) | 13,4–14,0 s | 9,1 s (1623) |
| `kling save` del dorado | 3,5–5,7 s | 1,5–1,9 s |
| `kling run -from` (restaurar) | **0,08 s** (el primero tras guardar: 1,44 s) | 0,65–0,94 s |
| restaurar → `adb shell true` | **0,17–0,21 s** | 0,73–0,99 s |
| identidad por clon (MMDS + `settings`) | 0,17–0,28 s | 0,08 s |
| `kling thaw` de un pausado → adb | 0,01 s → 0,07 s | 0,01 s → 0,07 s |
| thaw → `screencap` por adb | 0,47–0,50 s | 0,36 s |
| `adb exec-out screencap -p` (en el CT) | 0,48–0,64 s | 0,34–0,41 s |
| `kling exec … android-sh screencap -p` | 0,54 s | 0,27 s |
| `uidump` (servidor residente) | 27–38 ms (139 ms el primero) | ~20 ms |
| adb desde el Mac por `ssh -L`: `shell true` / `screencap` | 24 ms / 0,46 s | — |
| `adb install` Termux (35 MB) / `am start -W` | 0,69 s / 1,06 s | — |

El frío es ~3× más lento que en el M4 (un i7-8700T de 2018 a 2,4 GHz frente a
un M4, y render por software); restaurar es ~8× más rápido porque Firecracker
no copia la RAM: mapea el `mem.file` y fallan las páginas bajo demanda.

## Actualización 2026-09-29: listo, ganchos, verity y el núcleo nuevo

El daemon del sistema del CT pasó de v0.16.0-28 a esta rama (copia de los
binarios viejos en `/root/backup-kindling-v0.16.0-28/`), con kernel e imagen
nuevos: `ARCH=amd64 DATA_MODE=tmpfs DATA_SIZE=1G UIDUMP=1 VERITY=1
IMAGE_NAME=android13v BASE_NAME=android-basev build-image.sh`. Otro nombre
porque la imagen `android13` la usaban el dorado y el `phone-1` de entonces;
`/usr/local/bin/phone` exporta `PHONE_IMAGE=android13v`.

**Qué falló y cómo se arregló:**

| Fallo | Causa | Arreglo |
|---|---|---|
| `verity.sh` se paraba en el CT: `Cannot initialize device-mapper` | en el LXC `/dev/mapper/control` existe pero da `EPERM`, y el host no tiene `dm_verity` cargado | sin device-mapper, la tabla se escribe a mano (la de la versión 1 de dm-verity, 13 campos + FEC) y el árbol y el FEC se comprueban con `veritysetup verify` en espacio de usuario (8 s); el arranque la valida: `dmsetup status` en el invitado da `verity V` |
| Clones sin adb (`No route to host`, y adbd sin escuchar en 5555) | desde 10fd5d2 el núcleo arranca con `ipv6.disable=1`: adbd solo abre `[::]:5555`, y el IpClient del `eth0` de Android falla sin `/proc/sys/net/ipv6` y le borra la IPv4 en bucle (14 279 errores en <3 min, CPU gastada en el dorado) | receta con `guest_ipv6_stack: true` (arreglo del Mac, integrado): `ipv6.disable_ipv6=1`, módulo cargado y sin direcciones v6 en la VM; la barrera del host no cambia. Aquí se añadió `Snapshot.guest_ipv6_stack` para que `kling template ls` no lo marque como dorado viejo |
| `android-sh --identity` escribía los 4 primeros hex del `android_id` | con los ganchos esa salida acaba en `kling logs` | ya no escribe ni un trozo |
| Actualizar el daemon con máquinas vivas dejaba su namespace sin marca en `<root>/net/` | la marca solo se ponía al montar la red | reconcile marca la red de las máquinas propias que readopta |

**Cifras** (dorado 1536 MiB, 2 vCPU, egress none):

| | antes (09-28) | ahora |
|---|---|---|
| kernel K1 + Android + DM_VERITY + parche de relectura | 2 min 15 s | ~3 min (sha256 `ee238326…`) |
| imagen con base y Redroid en caché | 54 s | 1 min 48 s (48 s de `veritysetup format`, 8 s de `verify`); capa 2982 → 3030 MiB |
| `boot_completed` en frío | 10,4–10,8 s | 12,5–13,8 s (verity + pila IPv6) |
| `--verify-cache` del dorado (1711) | 13,4–14,0 s | 17,8–20,5 s, 0 diferencias (4 dorados) |
| `kling save` | 3,5–5,7 s | 3,5–4,1 s; `run` + `save` inmediato: 16,3 s (esperó a la sonda; el clon sale con `boot_completed=1`) |
| `run -from` → adb | 0,17–0,21 s | 0,09–0,10 s sin esperar; con `-wait-ready` (ganchos y sonda) 0,50 s |
| identidad por clon | 0,17–0,28 s (MMDS + `android-sh`) | 0,20–0,28 s (MMDS + `secret -hooks`) |
| `freeze` / `thaw` → adb | no se podía (secretos) | 1,9–2,9 s / 0,26–0,30 s → 0,9–1,0 s |
| `uidump` | 27–38 ms | 31–40 ms (271 ms el primero) |
| `adb exec-out screencap -p` en el CT / `kling exec … screencap` | 0,48–0,64 / 0,54 s | 0,46–0,70 / 0,51 s |
| desde el Mac (`phone-mac.sh`): `shell true` / `screencap` | 24 ms / 0,46 s | 36 ms / 0,71 s |

**Pruebas:**

- **Listo**: `kling ps` enseña `READY` (`ready` en dorado frío y clones; `-` en los
  congelados y en máquinas de agentes viejos); `kling machine ready` lista la sonda
  y los ganchos.
- **Identidad con los ganchos**: 3 clones con `android_id` distintos; ninguno aparece
  en `kling ps -json`, `kling logs` de los tres, el journal del daemon, `state.json`,
  los json del dorado ni su `mem.file`. `has_secrets` se levanta tras `{}`, los tres
  se congelan y al descongelar conservan su `android_id`. `phone-1` rehecho con su
  `android_id` de antes (`PHONE_ANDROID_ID`).
- **`cpu_pct`**: dorado en frío sin `-cpu-pct` → 200 (receta `cpu_pct_per_vcpu: 100`),
  y todos los clones 200.
- **`squeeze`** en una copia recién restaurada: 409 con el mensaje de memoria
  compartida; tras `freeze`/`thaw` (`mem_shared` apagado) se permite (~0 MiB); con
  `-force` en otra copia fresca, también. Dos máquinas.
- **Dos daemons**: uno privado (`-root /var/lib/kindling-priv -socket
  /run/kling-priv.sock`) arranca, crea y borra máquinas y se para sin tocar el
  namespace ni el veth de `phone-1`; el del sistema, al reiniciar, deja el del
  privado ("leaving namespace … alone"). **Choque de subredes medido antes del
  arreglo #97**: el privado avanzó su cursor hasta el índice de `phone-1` (97) y
  montó la misma 172.30.1.132/30: dos rutas iguales en dos veth y `kling machine
  ready` de la máquina del privado **contestó el agente de `phone-1`** (sonda de
  Android en una imagen `min`). Con el arreglo integrado, 110 máquinas del privado
  saltan el índice de `phone-1` (101) y siguen por 102.
- **Densidad** (`density.sh run`, prefijo propio, con `phone-1` vivo): **21 + 1
  teléfonos**; paró la guarda de swap (+37 MiB). ~187 MiB de PSS y ~176 MiB de
  MemAvailable por clon extra (antes ~162/~165). Los 21 sanos.

  | teléfonos | Σ PSS | Σ Private_Dirty | MemAvailable | swap usada |
  |---|---|---|---|---|
  | 1 | 575 | 156 | 5743 | 33 |
  | 4 | 1273 | 655 | 5219 | 33 |
  | 8 | 2009 | 1321 | 4517 | 33 |
  | 12 | 2716 | 1995 | 3810 | 33 |
  | 16 | 3424 | 2684 | 3090 | 33 |
  | 20 | 4122 | 3366 | 2390 | 46 |
  | 21 | 4308 | 3547 | 2219 | 69 |

- **`--verify-cache` de 4 en 4** con 8 clones: 4 × 1711 ficheros, 0 diferencias,
  pero cada clon verificado sube de ~120 a ~585 MiB de Private_Dirty (+460; el 09-28
  eran +150 con el dorado de 1024) y la tanda metió 534 MiB en swap con
  MemAvailable en 3,3 GiB: la guarda paró la segunda tanda. Con verity y este CT,
  `--verify-cache` de a 2 como mucho.
- **Muro** (`wall/wall-mac.sh`): `/api/phones` con 5 teléfonos sanos, `/shot/v-2`
  PNG 720×1280 en 0,8 s, y `POST /tap/v-2?x=102&y=1120` abrió Contactos.

## 1. Preparar el CT (hecho en el 106)

```sh
apt-get install -y nftables iptables ipset jq e2fsprogs squashfs-tools curl build-essential \
  flex bison bc libelf-dev libssl-dev xz-utils gnupg debootstrap debian-archive-keyring \
  python3 perl dwarves rsync file openjdk-17-jdk-headless unzip adb bsdextrautils
for c in nft iptables ipset; do ln -sf /usr/sbin/$c /usr/local/bin/$c; done  # el daemon no mira /usr/sbin
useradd --system kindling && usermod -aG kvm kindling
# Firecracker v1.17.0 (la del laboratorio): tgz + .sha256.txt de su release, sha256sum -c
```

kindling como `make deploy` pero a mano (desde el Mac: `make daemon guest
chispa-guest GOARCH=amd64` y `scp` a `phones`): `kling` en `/usr/local/bin`,
`kling-guest`, los scripts de construcción y `kling.service`. Kernel del CI
(`30-fetch-artifacts.sh`) y base `min` (`70-build-minimal-image.sh`) para el
`kling up -check`, que sale limpio. Después `images/vmlinux` pasa a ser el
kernel Android (el CT es solo para teléfonos; el del CI queda en
`images/vmlinux.k1-ci`).

## 2. Kernel x86_64

```sh
TARGET_ARCH=amd64 KERNEL_SHA256=5779f9caca77f7bfe3c3923b4d760041318db0303de93b2d4691d60e4d41eb74 \
  OUT=/root/android-kernel prototypes/android/kernel/build.sh
```

K1 (`config-common` + `config-amd64`) + `config-android` +
`kernel/config-android-x86_64`. Sale `vmlinux-6.1.140-kindling-amd64-android`,
sha256 `0e280d90…` en la primera compilación (la de ahora, sin ACPI, cambia).
Lo que x86 pidió además de lo de arm64:

| Opción | Por qué |
|---|---|
| `IA32_EMULATION=y` | en x86 es lo que da `COMPAT` (no tiene pregunta propia); sin él el init de Android 13 aborta al escribir `mmap_rnd_compat_bits`, igual que en arm64 |
| `CPU_MITIGATIONS=y` | **deriva del K1 amd64 con 6.1.140**: el menú se llama así (retroportado); `config-amd64` pide `SPECULATION_MITIGATIONS` y allnoconfig dejaba PTI, RETPOLINE, RETHUNK, SRSO… en "n" y el comprobador del núcleo fallaba |
| `# CONFIG_ACPI is not set` | **Firecracker 1.17 describe un PCI en su DSDT (y MCFG)**. El K1 amd64 va sin PCI y ACPICA no carga el DSDT (`AE_BAD_PARAMETER, During Region initialization`); la IRQ del virtio-mmio queda sin enrutar, `virtio_blk: probe … failed with error -22` y pánico al montar la raíz. Sin ACPI el kernel usa la tabla MP y los `virtio_mmio.device=` de la línea de comandos. El kernel del CI de Firecracker (con PCI) arranca bien |

Las dos últimas son **hallazgos para el núcleo**: el K1 amd64 de
`scripts/builders/kernel` con 6.1.140 no arranca en Firecracker 1.17 tal cual
(se probó a mano con `firecracker --config-file` y la imagen `min`). Arreglarlo
es de `config-amd64` (renombrar la opción; y o ACPI fuera, o PCI permitido),
no de este prototipo.

Siguen sin quedar como se piden (igual que en arm64): `BPF_JIT` y
`BPF_JIT_ALWAYS_ON` (dependen de `MODULES`) y `VMGENID`. Android arranca sin JIT
de BPF.

## 3. Imagen x86_64

```sh
ARCH=amd64 DATA_MODE=tmpfs DATA_SIZE=1G UIDUMP=1 prototypes/android/image/build-image.sh
```

Redroid `13.0.0_64only-240527`, **amd64**, del mismo índice que el arm64
(`sha256:5a42a569…`), fijado por digest:

| Pieza | Digest | Verificado |
|---|---|---|
| manifiesto amd64 | `sha256:36d6d21bcf7e92d78eabaa6f1748e5cf0e9fb15176c0091794168be012c5c22e` | leído del índice en Docker Hub; sha256 del manifiesto pedido al registro coincide |
| configuración | `sha256:8bf1395b61afc74d86b13cd776cee36d0ca8ac64ee2eb2e342487cd561eee6db` | sha256 y `architecture=amd64`, ENTRYPOINT `/init qemu=1 androidboot.hardware=redroid` |
| capa (única) | `sha256:2824b019a4a8a038e79392f80302a3ddca69fdfdd6acb8c37b16e1461b2a6168` (682 925 585 bytes) | sha256 al bajarla (`build-image.sh`, `EXPECT_LAYER`) |

Lo que dice la imagen: Android 13, `ro.product.model=redroid13_x86_64_only`,
ABIs `x86_64,arm64-v8a` (anuncia arm64 pero **no** trae traducción; apps arm64
quedan fuera de alcance), APEX aplanados, binderfs lo monta su `init.rc`.
`kling-guest` linux/amd64 estático. `android-launch.sh` usa el cargador de glibc
x86_64 tras el `pivot_root`.

**Termux x86_64** (para la prueba de APK): `termux-app_v0.118.3+github-debug_x86_64.apk`
de la release oficial, sha256
`3550e61f4d9eb49b712fd1bd9519dc37085a4d8eb597c57a340f0a64859b7144`, que
coincide con el `…_sha256sums` de la misma release (y ese fichero da para el
arm64 el `72fdb596…a4e` que ya usaba el prototipo). Está en `/root/apk/` del
CT. Instalado con `adb install` en un clon de 1024 MiB: `primaryCpuAbi=x86_64`,
`versionName=0.118.3`, `am start -W` en 1,06 s, `TermuxActivity` arriba y la
pantalla de bienvenida con el prompt (el bootstrap nativo se extrajo sin red:
va dentro del APK).

## 4. Densidad

`x86_64/density.sh run` crea teléfonos de uno en uno desde el dorado
(`phone.sh up -n 1`), espera 20 s y suma `smaps_rollup` de cada proceso
firecracker. Solo cuenta los teléfonos de su prefijo (`PHONE_PREFIX`). Dorado
de 1536 MiB, 2 vCPU:

| teléfonos | Σ RSS | **Σ PSS** | Σ Private_Dirty | MemAvailable del CT | swap usada |
|---|---|---|---|---|---|
| 0 | — | — | — | 6109 MiB | 0 |
| 1 | 802 | 802 | 158 | 5948 | 0 |
| 4 | 3345 | 1372 | 672 | 5411 | 0 |
| 8 | 6763 | 2103 | 1389 | 4663 | 5 |
| 12 | 10150 | 2802 | 2072 | 3945 | 24 |
| 16 | 13563 | 3522 | 2786 | 3203 | 42 |
| 20 | 16809 | 4084 | 3332 | 2623 | 232 |
| **24** | 19943 | **4533** | 3770 | 2152 | 556 |

- **Coste de un clon extra: ~162 MiB de PSS** ((4533 − 802) / 23), ~157 MiB de
  Private_Dirty, y MemAvailable baja ~165 MiB por teléfono. Es lo que el
  invitado escribe (su `/data` en tmpfs, montones de Java, SurfaceFlinger);
  el resto (~600 MiB por clon de `Shared_Clean`) es el `mem.file` del dorado,
  una sola vez en la caché del CT.
- **RSS engaña**: 24 teléfonos suman 19,5 GiB de RSS en un CT de 6 GiB.
- **Paró la admisión de kindling** (`internal/machine/memory.go`,
  `checkHostMemory`): descuenta de MemAvailable la caché "caliente" de los
  `mem.file` vivos, y con 2152 MiB disponibles solo le quedaban 612 útiles para
  los 384 que pide un restaurado + 384 de reserva. Es correcto: esa caché es la
  RAM compartida de los 24.
- **CPU**: carga media ~1 con 24 teléfonos quietos en 8 hilos; no es el límite.
- **Sanos los 24** tras llenarlo (`density.sh health`: adb, `boot_completed`,
  system_server, `logcat -b crash`).

### Con `-mem 1024`

Dorado aparte (`PHONE_MEM=1024 PHONE_GOLDEN=phone-golden-1024
PHONE_PREFIX=d1024`, boot_completed 11,1 s, `--verify-cache` 16,6 s, save
3,3 s), CT vacío al empezar, con las guardas de abajo:

| teléfonos | Σ RSS | **Σ PSS** | Σ Private_Dirty | MemAvailable | swap usada |
|---|---|---|---|---|---|
| 0 | — | — | — | 6089 MiB | 1 |
| 1 | 803 | 803 | 248 | 5841 | 1 |
| 4 | 3321 | 1701 | 1096 | 4977 | 1 |
| 8 | 6714 | 2886 | 2263 | 3785 | 1 |
| 12 | 10059 | 4016 | 3371 | 2653 | 1 |
| **14** | 11754 | **4590** | 3957 | 2061 | 2 |

- **~291 MiB por clon extra** (PSS y MemAvailable), casi el doble que con
  1536. Por clon, Private_Dirty ~285 MiB frente a ~157. La explicación más
  probable (no demostrada): con 1024 MiB el invitado va justo, su kernel
  reclama caché de páginas y la vuelve a leer, y cada página releída es una
  página **privada** nueva en el anfitrión, en vez de la compartida del
  `mem.file`. Es el mismo mecanismo que hace dañino a `squeeze`.
- Paró en 14 la comprobación propia de `phone.sh` (MemAvailable < 35 % del
  CT); con solo el suelo de 1 GiB habrían sido ~17. Con 1536 caben más: ~24
  (lo para la admisión de kindling), unos 31 por el suelo de 1 GiB.
- **Para densidad, `-mem 1536` es mejor que 1024** en este prototipo.

### Integridad con el CT lleno (páginas a ceros)

La pregunta: ¿pasa aquí lo del Mac (`docs/sigill.md`: bloques de 4 KiB de la
caché del invitado a ceros con varios clones del mismo dorado)? **En
Firecracker no se ha visto ni una vez**:

| Prueba | Resultado |
|---|---|
| `--verify-cache` antes de guardar cada dorado (1536 dos veces, 1024 una) | 1711 ficheros, 0 diferencias |
| `density.sh verify` de 8 clones del dorado de 1024 (2 a la vez), con el CT al límite de las guardas (14 → 8 teléfonos, MemAvailable 3,9 → 2,9 GiB) | 8 × 1711 ficheros, 0 diferencias. Cada comprobación sube ~150 MiB la privada del clon |
| `stress-restore.sh -modes reads -cycles 20 -read-passes 3` en **2 clones a la vez** del dorado de 1024, con otros 4 clones del mismo dorado vivos | 20 + 20 ciclos ok: 2 × (2 de referencia + 60) pasadas de `drop_caches` + md5 de 2359 ficheros (1,4 GB) de `/android/system` y `/usr`, **0 lecturas malas, 0 páginas a ceros**; ~30 s por ciclo |
| `density.sh health` tras cada tanda (adb, `boot_completed`, system_server, `logcat -b crash`) | todos sanos |

Una primera tanda de `reads` con 8 clones vivos la cortó `watchdog.sh` al
minuto: la swap del CT subió 86 MiB (sin diferencias en lo leído hasta
entonces). Con 6 teléfonos no subió.

En el Mac el fallo era 1 bloque en ~730 pasadas con picos de memoria del host;
aquí son 124 pasadas + 13 verificaciones sin presión real (el CT sin swap y
la presión de su cgroup a 0), así que la comparación es honesta solo hasta ahí:
no se ha forzado presión en el host (ni se debe: tiene VMs del usuario).

### `squeeze` es contraproducente con clones compartidos

`kling machine squeeze` a los 24 (34 s en total) "devolvió" 86–247 MiB cada
uno según el CLI, pero **Σ PSS subió de 4533 a 5072 MiB**, Σ Private_Dirty de
3770 a 4560, MemAvailable bajó a 1052 MiB y **la swap del CT se llenó**
(SwapFree 2 MiB). Al inflar el globo el invitado suelta su caché de páginas,
que en un clon eran páginas **compartidas y limpias** del `mem.file`; en cuanto
Android vuelve a tocarlas las lee del disco virtual a páginas **privadas**. Un
teléfono solo baja (~110–120 MiB de privada tras squeeze los tres primeros),
pero el total sube.

Justo después, con el CT así y `android-sh --verify-cache` en marcha en todos
los teléfonos, **el CT dejó de contestar** (sshd no completa el saludo; TCP y
ping sí): el cgroup del CT se quedó reclamando y refaulteando caché de ficheros
sin llegar a matar nada (un bloqueo por falta de memoria, no un OOM). Sin
acceso al host no se pudo reiniciar desde aquí.

El cuelgue también cargó el host pve (load 73, 3 GiB de swap del host, PSI
de IO ~12 %) y hubo que parar y arrancar el CT.

**Conclusión**: en Firecracker con dorados compartidos, **nunca `squeeze` en
teléfonos**. `density.sh` ya no tiene `-squeeze`.

### Guardas (desde el cuelgue)

El CT comparte anfitrión con VMs del usuario. `density.sh` (`run`, `verify`,
`safe`) y `x86_64/watchdog.sh` aplican:

- MemAvailable del CT ≥ 1 GiB antes de cada teléfono o comprobación;
- la swap del CT no crece (más de 16 MiB en `density.sh`, 64 en el perro
  guardián) y la presión de memoria **del cgroup del CT**
  (`/sys/fs/cgroup/memory.pressure`; `/proc/pressure` en un LXC es la del host)
  some avg10 < 5 %;
- `--verify-cache` de a lo sumo 4 a la vez (se usó 2);
- solo se miden y borran los teléfonos del prefijo propio (`density.sh clean`,
  nunca `phone.sh rm -a`).

Ojo con la swap: este CT empieza a usarla en cuanto su RAM (caché incluida) se
llena, aunque MemAvailable diga 2–4 GiB (se vio al crear el dorado de 1024 y
al leer mucho en los clones). Con la RAM del CT llena de caché de `mem.file`, "sin
swap" es una guarda más estricta que "MemAvailable ≥ 1 GiB".

### KSM

`/sys/kernel/mm/ksm/run` = 0 en el host: KSM apagado, y encenderlo es cosa del
host. Estimación: KSM solo junta páginas **anónimas** idénticas; las
compartidas del `mem.file` ya lo están. Lo que ganaría es la parte de
Private_Dirty que coincide entre clones (código JIT, montones de zygote
escritos igual en todos tras restaurar): con ~157 MiB privados por clon, si la
mitad fuese igual serían ~80 MiB por teléfono, es decir ~+10 teléfonos en 6 GiB,
a cambio de CPU de ksmd y de que Firecracker pida `MADV_MERGEABLE` (o
`prctl(PR_SET_MEMORY_MERGE)` en el proceso, Linux 6.4+). **No medido.**
Recomendación para el usuario: probar `echo 1 > /sys/kernel/mm/ksm/run` en pve
en una ventana tranquila si se quiere exprimir más; el prototipo no lo necesita.

## 5. Uso

En el CT:

```sh
phone up -n 5          # /usr/local/bin/phone = phone.sh contra /run/kling.sock
phone ls               # NAME STATE ADB VNC  (ADB = <ip de la VM>:5555)
phone adb 3 shell      # adb dentro del CT
phone pause 3; phone resume 3
phone pool 4; phone rm -a; phone golden rebuild
```

Desde el Mac (`x86_64/phone-mac.sh`, alias ssh `phones`):

```sh
prototypes/android/x86_64/phone-mac.sh up -n 3
prototypes/android/x86_64/phone-mac.sh adb 2          # abre el túnel, imprime 127.0.0.1:15002
adb -s 127.0.0.1:15002 shell
prototypes/android/x86_64/phone-mac.sh view 2         # scrcpy por el túnel
prototypes/android/x86_64/phone-mac.sh unforward
```

A mano es `ssh phones -f -N -L 127.0.0.1:15002:<ip de la VM>:5555` y `adb
connect 127.0.0.1:15002` (probado: `redroid13_x86_64_only`, el `android_id` del
clon, `screencap` 0,46 s).

**Por qué un túnel y no un reenvío en la IP del CT**: adb de Redroid va sin
autenticación (`ro.adb.secure=0`) y el VNC sin contraseña. Publicarlos en
192.168.2.20 los dejaría abiertos a toda la LAN. Las IPs de las microVMs
(172.30.0.x) solo existen dentro del CT; el túnel las acerca al 127.0.0.1 del
Mac y nada más.

## 6. Qué cambió en el prototipo

| Fichero | Cambio |
|---|---|
| `kernel/build.sh` | `TARGET_ARCH=amd64` (arm64 por defecto) |
| `kernel/config-android-x86_64` | nuevo (tabla de la sección 2) |
| `kernel/check-android-config.sh` | `ARM64_4K_PAGES` solo en arm64 |
| `image/build-image.sh` | `ARCH=amd64`: digests amd64, ELF 0x3e, `--platform` |
| `image/android-launch.sh` | cargador de glibc x86_64 tras el `pivot_root` |
| `image/android-sh` | MMDS leído por `Content-Length`: el de Firecracker contesta `keep-alive` aunque se le pida `close` y `--identity` se colgaba 30 s |
| `phone.sh` | Linux: nivel de memoria desde `/proc/meminfo`, adb/VNC en la IP de la VM (no hay reenvíos a 127.0.0.1 en Firecracker), `PHONE_SOCK` para usar el daemon del sistema |
| `x86_64/density.sh` | nuevo: `run`, `measure`, `health`, `verify`, `safe`, `clean`, con guardas y prefijo |
| `x86_64/watchdog.sh` | nuevo: corta una prueba (y borra sus máquinas) si salta una guarda |
| `x86_64/phone-mac.sh` | nuevo |

`stress-restore.sh` corre en Linux salvo `-hostspike` (usa `sysctl` de macOS).

Otras notas:

- `uiautomator dump` con `uidump` en marcha muere (`Killed`, 0 bytes); con
  `uidump` no hace falta. No se probó parando `uidump`.
- `phone.sh up` ya incluye `--verify-cache` antes de guardar el dorado: en el CT
  sin presión, 1711 ficheros, 0 diferencias (dos dorados).

### No hecho

- **Redroid en contenedor con `/dev/dri`** (`gpu_mode=host`), la alternativa
  sin microVM: no se intentó. Con las guardas nuevas y el CT compartido con
  pruebas del usuario no quedaba margen de memoria para instalar docker/podman
  y otro Android.
- KSM (arriba), SELinux, apps arm64 (sin traducción).

## 7. Lo que queda en el CT

- `phone-1` (el del usuario, rehecho del dorado nuevo con su `android_id` de
  siempre) y ningún teléfono de prueba.
- Dorados `phone-golden` (imagen `android13v` con verity, 1536 MiB, 2 vCPU,
  egress none; el de uso) y `phone-golden-1024` (el de la medida, de la imagen
  `android13` vieja) en el daemon del sistema; kernel Android con DM_VERITY en
  `images/vmlinux` (el anterior en `images/vmlinux.android-20260928`).
- `/root/kindling` (scripts), `/root/android-kernel`, `/root/apk` (Termux),
  `/var/tmp/kindling-android/out/android-fase0-*.tar` (imagen empaquetada).
- Para N teléfonos: `phone up -n N` (~22 con 1536 MiB en 6 GiB, medido el
  09-29 con la imagen de verity; con las
  guardas, parar antes si MemAvailable baja de 1 GiB o el CT empieza a usar
  swap: `density.sh safe` lo dice).
