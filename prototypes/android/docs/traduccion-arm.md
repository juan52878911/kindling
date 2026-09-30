# Apps solo-ARM: traducción en x86_64 o un host ARM

Issue #93. Muchas apps de Android solo traen código nativo `arm64-v8a`. En un
teléfono de kindling hay dos maneras de correrlas:

- **(a) Host x86_64 con traducción ARM**: `libndk_translation` (de Google)
  traduce en el invitado el código arm64 a x86_64. Opcional al construir y
  **apagada por defecto**, por la licencia (abajo).
- **(b) Host ARM (arm64 nativo)**: el Mac con vz, o un Linux arm64 con
  Firecracker (AWS Graviton o Ampere en bare metal). La app corre
  tal cual. **Es el camino recomendado** para apps solo-ARM.

## Cuándo usar cada una

| | x86_64 sin traducción (`none`, por defecto) | x86_64 + `libndk` | arm64 nativo (Mac vz, Graviton/Ampere) |
|---|---|---|---|
| Apps x86_64 o solo Java/Kotlin | ✓ | ✓ | ✓ (las que traigan arm64 o ninguna nativa) |
| Apps solo-ARM (`arm64-v8a`) | no: `INSTALL_FAILED_NO_MATCHING_ABIS` al instalar | ✓ (medido: Termux entero) | ✓ |
| Apps solo-32 bits (`armeabi-v7a`) | no | no (la imagen es 64only) | no en Apple Silicon (sin AArch32); sí en algunos Ampere/Graviton con Redroid no-64only (no probado) |
| CPU del código nativo ARM | — | **4–5× más lento** en bucles, **~30×** al lanzar procesos (~150 ms por `exec`) | nativo |
| Memoria | — | +13 MiB de PSS en Termux (186 frente a 173 MiB), ~85 MiB menos de MemAvailable en el invitado | nativo |
| Arranque del teléfono | 14,6–15,7 s | 13,9–15,4 s (igual) | 8,1 s (M4, vz) |
| Capa (el ext4, sin el árbol de verity) | 1 546 MiB | 1 576 MiB (+30 MiB) | 1 400 MiB (otra imagen) |
| Licencia | nada de Google | **blob propietario de Google** (abajo) | nada de Google |
| Cuándo | granjas x86_64 con apps x86_64/Java; lo seguro por defecto | probar una app ARM concreta en un host x86_64 que ya tienes, en privado | apps solo-ARM en serio, CI de apps ARM, densidad con código nativo |

Resumen: **si la app es solo-ARM y va a correr en serio, host ARM**. La
traducción sirve para que una app ARM funcione en un x86_64 que ya tienes,
aceptando el coste de CPU y la licencia.

## (a) La traducción en x86_64

### Lo que ya trae Redroid, y por qué el defecto es quitarlo

Redroid 13 amd64 (`13.0.0_64only-240527`, la fijada) **ya trae** un
`libndk_translation` (de enero de 2023) con `libnb.so` como puente y
`ro.dalvik.vm.native.bridge=libnb.so` en `vendor/build.prop`. Lo que decía
proxmox.md ("anuncia arm64 pero no trae traducción") era falso: medido aquí,
con esa imagen tal cual Termux arm64 se instala y su biblioteca JNI corre
traducida, pero el bootstrap falla (dos problemas, abajo). Redroid no dice de
dónde sale ese binario ni con qué licencia lo redistribuye. Por eso el
constructor, **por defecto en amd64 (`none`), lo quita**: fuera `libnb.so`,
`libndk_translation*.so`, `lib64/arm64`, `bin/arm64`, el lanzador de
binfmt_misc, `ndk_translation.rc` y `ld.config.arm64.txt`, y las ABIs en todas
las `build.prop` (system, vendor, **odm**) pasan a `x86_64`. Una app solo-ARM
falla al instalarse, con un error claro, en vez de instalarse y fallar después.

### Los modos

| `arm_translation` (spec) / `ARM_TRANSLATION` (build-image.sh) | Qué hace |
|---|---|
| `none` (por defecto en amd64) | quita el puente de Redroid; ABIs `x86_64` |
| `libndk` | quita el de Redroid y pone el de la imagen del emulador de Google (abajo), bajado y comprobado por el constructor; ABIs `x86_64,arm64-v8a` |
| `redroid` | deja el de Redroid tal cual (sin el arreglo de argv[0]: el bootstrap de Termux no termina) |
| en arm64 | `""`/`none`: nada que hacer; `libndk`/`redroid` son un error |

```sh
ARM_TRANSLATION=libndk prototypes/android/image/spec.sh > spec.json   # constructor Go
kling image build android13x -builder android -spec spec.json
sudo ARCH=amd64 ARM_TRANSLATION=libndk prototypes/android/image/build-image.sh   # el script
```

Hace falta el **kernel con `CONFIG_BINFMT_MISC=y`**
(`kernel/config-android-x86_64`, añadido aquí; el comprobador lo recomienda en
x86_64). Sin él, las bibliotecas JNI arm64 se traducen igual (las carga ART por
el puente), pero un **ejecutable** arm64 (el bootstrap de Termux, lo que una
app lanza con `exec`) da `ENOEXEC`.

### De dónde sale `libndk`, y cómo se verifica

**La imagen de sistema x86_64 del emulador de Android 14** (API 34, "Google
APIs", revisión 14), tal como la sirve Google:

| | |
|---|---|
| URL | `https://dl.google.com/android/repository/sys-img/google_apis/x86_64-34_r14.zip` |
| lista oficial | `https://dl.google.com/android/repository/sys-img/google_apis/sys-img2-3.xml`, paquete `system-images;android-34;google_apis;x86_64`: tamaño 1 563 721 130, sha1 `e0f6c9a0691aa27bd597d0deb1bcfdc943ac8ca7` |
| sha256 (fijado) | `783a40134baf4f3012d4464fbe1571b1612a0dbd2e7a44d14bd8328923443833`, calculado el 30-09-2026 sobre el fichero bajado; su sha1 coincide con el de la lista |
| dentro | `x86_64/system.img`: disco GPT → partición `super` (particiones dinámicas, liblp) → ext4 `system` → `/system/lib64/libndk_translation.so` (julio de 2024, `ro.ndk_translation.version=0.2.3`), las `libndk_translation_proxy_*.so`, `lib64/arm64` (58 bibliotecas arm64 que ve la app), `bin/arm64/{linker64,app_process64}`, el lanzador `ndk_translation_program_runner_binfmt_misc_arm64`, `etc/binfmt_misc/arm64_{exe,dyn}`, `etc/init/ndk_translation.rc` |

Es la fuente más verificable que hay: primera parte (Google), por HTTPS desde
`dl.google.com`, con tamaño y sha1 publicados en la lista del SDK que usa
`sdkmanager`. Las alternativas son peores: la imagen de recuperación de
ChromeOS también es de Google, pero `libndk` va dentro de la imagen del
Android de ARCVM, que habría que desempaquetar además; los prebuilts de
terceros en GitHub que usan scripts como
[redroid-script](https://github.com/ayasa520/redroid-script) no dicen de qué
imagen salen; y la copia que trae Redroid, lo mismo.

El constructor (`internal/android/libndk.go`) baja el zip (1,4 GiB) a la caché
del daemon, comprueba tamaño y sha256, lo lee **en streaming** (no escribe los
4 GiB del disco: solo la partición `system`, dispersa), la lee con el lector
ext4 de kindling, saca esas 90 entradas a un tar de 29 MiB en la caché
(`cache/android/libndk-<sha256>-v1.tar`, determinista) y borra el zip. Las
siguientes construcciones usan el tar. `build-image.sh` hace lo mismo con
`image/arm-translation.sh` (python3 + debugfs); comprobado que las 58
bibliotecas de `lib64/arm64`, `libndk_translation.so`, `arm64_exe` y la
`vendor/build.prop` salen **idénticas** por los dos caminos.

**Probadas y descartadas**: la de Android 12L (API 32, `x86_64-32_r08.zip`,
sha256 `2709bcc5…63bf1`): su `libndk_translation` de 2022 no tiene todos los
*thunks* que usa el `dpkg` de Termux (`Bad thunk call`) y el bootstrap no
termina. La `google_apis` de Android 13 (API 33) **no trae**
traducción; las de 34 y 35 sí.

### Lo que añade kindling (no es de Google)

- **Las propiedades**, en las `build.prop` que ya las tienen y el resto en un
  bloque marcado al final de `vendor/build.prop`:
  `ro.dalvik.vm.native.bridge=libndk_translation.so`,
  `ro.enable.native.bridge.exec=1`, `ro.vendor.enable.native.bridge.exec{,64}=1`,
  `ro.dalvik.vm.isa.arm64=x86_64`, `ro.ndk_translation.version=0.2.3`,
  `ro.ndk_translation.flags=accurate-sigsegv` (las del emulador) y las ABIs
  `x86_64,arm64-v8a` (`abilist64` igual; `abilist32` vacía). Sin
  `ro.dalvik.vm.isa.arm`: nada de 32 bits.
- **`/system/bin/kindling-ndk-binfmt`**, un script de `sh` que va de
  intérprete de binfmt_misc. El lanzador de Google toma el `argv[0]` ORIGINAL
  (el que conserva la bandera `P`) como ruta del programa y le hace
  `realpath`: cuando un shell ejecuta un programa del `PATH` por su nombre
  (`id`, `argv[0]="id"`), falla con `Unable to get realpath of id`. Termux lo
  documenta en su `termux-bootstrap-second-stage.sh` ("You have likely
  installed the wrong ABI"), y es lo que rompe su bootstrap en el emulador
  x86_64 y en Redroid. El script recibe del kernel la ruta de verdad y el
  `argv[0]`; si este no es una ruta, lo cambia por la ruta y llama al
  lanzador. `arm64_exe`/`arm64_dyn` apuntan a él (mismo `magic`, misma
  bandera `P`). Cuesta ~4 ms por `exec` (el lanzador, ~147 ms).

### Licencia: la franqueza

`libndk_translation` es **software propietario de Google**, sin licencia de
redistribución. La imagen del emulador se distribuye bajo el *Android
Software Development Kit License Agreement* (la lista la marca
`android-sdk-license`), que:

- la da "solely to develop applications for compatible implementations of
  Android" (3.1) — Redroid no es una implementación compatible (no pasa CTS);
- prohíbe "copy (except for backup purposes), modify, adapt, redistribute" el
  SDK o una parte (3.4) — sacar esos ficheros a otra imagen y editar el
  registro de binfmt es, como poco, discutible;
- y hay que aceptarla para usar el SDK (2.1).

Por eso:

1. **Apagada por defecto**; se pide con `arm_translation: libndk`.
2. **kindling no la redistribuye**: el repositorio no lleva ni un byte de
   ella, solo la URL y el sha256. La baja **quien construye**, en su máquina,
   y quien la pide acepta ese acuerdo con Google por su cuenta.
3. **Una imagen con `libndk` no se publica** (`kling image copy`, un
   registro, un paquete de fase 0): llevaría el blob dentro. El dorado y la
   receta lo dicen (`arm_translation`, `ndk_translation_source` en
   `IMAGE.txt`), para que se note.
4. Lo mismo vale para la copia que trae Redroid (`redroid`): su origen y su
   licencia son aún menos claros; por eso `none` la quita.

**Houdini** (la traducción de Intel, también propietaria, la de muchos
emuladores de escritorio) no se toca.

**Riesgos**: traducción incompleta (el ejemplo de API 32: un *thunk* que
falta y el programa muere con `Bad thunk call`), bibliotecas arm64 de
Android 14 debajo de un Android 13 (una app que use una API del NDK que
Android 13 no tiene, o una del 14 con otro comportamiento), apps con
detección de emulador o anti-trampas que miran `ro.dalvik.vm.native.bridge` o
`/proc/cpuinfo`, JIT y código auto-modificable (lo más lento de traducir) y
que Google cambie o retire el zip (el sha256 fijado lo detecta; habría que
fijar otro).

### Lo que dicen el dorado y la API

- **Receta** (`built.layer.arm_translation`): `mode`, `native_bridge` y, con
  `libndk`, `source`, `source_sha256`, `source_android`, `version`,
  `files_sha256` y `files`; `built.layer.abilist` son las ABIs tras la
  traducción.
- **`IMAGE.txt`**: `abilist`, `arm_translation` (`native`, `none`, `libndk`,
  `redroid`), `native_bridge` y, con `libndk`, `ndk_translation_version`,
  `ndk_translation_source` y su sha256.
- **`GET /v1/health`** de kling-phoned: `abis`, `native_bridge`,
  `arm_translation` (de `IMAGE.txt`) y `arm64_exec` (`enabled` si binfmt_misc
  tiene registrado `arm64_exe`):

```json
{"ok":true, ..., "abis":["x86_64","arm64-v8a"],"native_bridge":"libndk_translation.so","arm_translation":"libndk","arm64_exec":"enabled"}
{"ok":true, ..., "abis":["x86_64"],"arm_translation":"none"}
{"ok":true, ..., "abis":["arm64-v8a"],"arm_translation":"native"}
```

(`kling phone golden inspect` vive en `ext/phone`, PR #120: cuando llegue,
que lea estos campos de la receta.)

## (b) Host ARM nativo

Nada que construir: la imagen arm64 de Redroid 13 (64only) anuncia
`arm64-v8a`, sin puente, y el constructor no toca nada (`arm_translation:
native` en la receta). Funciona igual en:

- **Mac Apple Silicon (vz)**: lo que se ha medido siempre (README, fase 0;
  docs/densidad.md). Sin AArch32: nada de `armeabi-v7a`.
- **Linux arm64 con Firecracker** (Graviton en instancias EC2 **metal**, Ampere en
  bare metal...: Firecracker necesita KVM): el mismo
  kernel Android arm64 (`kernel/build.sh`, el del CI) y la misma imagen; el
  camino de Firecracker es el de proxmox.md con `ARCH=arm64`. **No probado aquí
  en un Graviton/Ampere** (no hay uno en el laboratorio): lo arm64 del
  constructor y del kernel es lo del Mac, y lo de Firecracker, lo del CT de
  x86_64.

## Cifras (30-09-2026)

`phones` (CT x86_64, Firecracker, 2 vCPU, 1536 MiB por teléfono), daemon
privado, las tres imágenes hechas con el constructor Go desde la misma
entrada; Termux 0.118.3 de su release oficial (sha256 contra el
`sha256sums` de la release: arm64 `72fdb596…a4e`, x86_64 `3550e61f…7144`).

| | `none` | `libndk` |
|---|---|---|
| `ro.product.cpu.abilist` | `x86_64` | `x86_64,arm64-v8a` |
| Termux arm64-v8a | `INSTALL_FAILED_NO_MATCHING_ABIS` (HTTP 422 de `/v1/install`) | se instala (`primaryCpuAbi=arm64-v8a`), bootstrap en 2,9–3,4 s, **prompt** (captura), `bash` con `e_machine b7` (aarch64) ejecuta `uname -m` → `x86_64` |
| construir, primera vez | 27–29 s | +24,5–28,9 s de bajada (1 491 MiB) y 6,0 s de extracción |
| construir, con caché | 27–29 s | 28–29 s |
| capa (fichero con verity) | 1 647 603 712 B | 1 678 839 808 B (+30 MiB; `redroid`: 1 671 536 640) |
| arranque en frío hasta listo | 14,6–15,7 s | 13,9–15,4 s (`redroid`: 14,2 s) |

La misma prueba de CPU con los binarios de Termux (el `bash` y el
`sha256sum` del bootstrap, mismo paquete en las dos arquitecturas), como root
en Android, tres rondas:

| | x86_64 nativo (`none` + Termux x86_64) | arm64 traducido (`libndk` + Termux arm64) | factor | arm64 nativo (M4, vz; otro hardware) |
|---|---|---|---|---|
| bucle de `bash`, 300 000 vueltas | 1,72–1,80 s | 9,41–9,74 s | ×5,4 | 0,74–0,80 s |
| `sha256sum` de 256 MiB | 1,20–1,22 s | 5,16–5,41 s | ×4,3 | 0,55–0,59 s |
| 200 × `exec true` | 0,94–0,95 s | 29,9–30,1 s | ×31 | 0,30–0,37 s |
| bootstrap de Termux (2.ª fase) | 0,12 s | 2,9–3,4 s | ×25 | 0,07 s |
| PSS de Termux tras el bootstrap | 173 MiB | 186 MiB | +8 % | 169 MiB |

El `exec` es lo caro: cada proceso arm64 arranca el traductor (~147 ms el
lanzador solo; el script de argv[0] añade ~4 ms). Una app que vive en un
proceso (JNI) paga el factor de CPU; una que lanza muchos procesos (shells,
Termux, herramientas de línea de órdenes) paga mucho más.

Con la imagen de Redroid tal cual (`redroid`) y el kernel con binfmt_misc:
Termux se instala y extrae (JNI traducido), y el bootstrap muere en
`Unable to get realpath of id`; con el script de argv[0] puesto a mano,
termina y da el prompt. Con el kernel anterior (sin `BINFMT_MISC`) ningún
ejecutable arm64 arranca.

## Límites

- Solo arm64: la imagen es 64only, no hay traducción de 32 bits.
- La traducción es la del emulador de Android 14 sobre un Android 13 (no hay
  una de 13: la imagen de API 33 no la trae).
- El kernel necesita `CONFIG_BINFMT_MISC=y` (en este cambio; el kernel del CI
  lo lleva en la siguiente release).
- No probado en Graviton/Ampere (b), ni apps con detección de emulador.
- Una imagen con `libndk` no debe salir de la máquina donde se construyó.
