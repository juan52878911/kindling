# Densidad: cuántos teléfonos caben

Cómo bajar la RAM por teléfono Android (Redroid 13 en una microVM de kindling)
sin romper lo que importa: arranque, `boot_completed`, abrir una app arm64
(Termux), `uiautomator dump`, `screencap` y el acceso por `android-sh`.

Medido el 2026-09-28 en un MacBook Air M4 de 16 GiB (macOS 26), backend vz,
`-cpus 2 -cpu-pct 200 -egress none`, con el Mac compartido con otros cinco
agentes (swap de 7–9 GiB casi todo el rato: los tiempos llevan ruido).

## La receta

```sh
# en la VM Lima (arm64), como el README, más SLIM=1 y pantalla pequeña
sudo SLIM=1 WIDTH=480 HEIGHT=800 DPI=240 FPS=10 \
     DATA_MODE=overlay IMAGE_NAME=android13slim \
     prototypes/android/image/build-image.sh

# en el Mac
kling run -image android13slim -mem 768 -cpus 2 -cpu-pct 200 -egress none -allow-exec
```

`SLIM=1` llama a [`image/slim/apply.sh`](../image/slim/apply.sh) sobre el rootfs
antes de montar la capa. Cada cosa que toca está en su propio fichero:

| Fichero | Qué hace |
|---|---|
| `slim/services.txt` | comenta 17 servicios de init: Bluetooth, radio, wifi, cámara, trazas (perfetto, incidentd), mdnsd, DRM/CAS y VNC |
| `slim/features.xml` | declara ausentes Bluetooth, cámara, micrófono, wifi, impresión y los fondos animados: system_server deja de lanzar sus servicios |
| `slim/apps.txt` | quita 33 apps del sistema (navegador, cámara, galería, reloj, contactos, calendario, música, impresión, NFC, fondos...) |
| `slim/slim.prop` | `ro.config.low_ram=true`, montículos de ART como en Android Go, sin perfiles de JIT, zygote sin precarga de GL, lmkd más agresivo, 5 tombstones como mucho |

**`DATA_MODE=overlay`, no `tmpfs`, en cuanto se instalen apps.** Con tmpfs,
`/data` está en RAM y no se puede liberar. Termux por sí solo deja 231 MiB en
`/data`: 62 MiB de la app y 77 MiB del bootstrap que descomprime al abrirse por
primera vez. Con 768 MiB y tmpfs, el dorado se quedó con 18 MiB disponibles
(Shmem 166 MiB) y el dump dejó de responder. Con overlay, `/data` está en el
disco de la máquina y ocupa caché de páginas, que sí se puede liberar: con
Termux abierto quedan 150 MiB disponibles (Shmem 13 MiB), el dump funciona
(26 nodos) y la huella es la misma (823 MiB). El precio es el techo de 512 MiB
del disco escribible (README); Android recién arrancado más Termux caben de
sobra.

`apply.sh` también vale sobre `/android` dentro de una VM ya arrancada. Con
`SLIM_PARTS` y `SLIM_PROP` se aplican las partes una a una para medirlas.
`android-launch.sh` no cambia: ninguna palanca del lanzador salió a cuenta.

## Palanca a palanca (-mem 1536)

Dentro del invitado se mide después de 45 s de reposo. "Usada" es la `Used RAM`
de `dumpsys meminfo` (PSS de los procesos más el kernel sin cachés); "anónima"
es `AnonPages`. La huella es `phys_footprint` de la VM en el Mac (`kling top`).
Cada palanca se midió sola, con una VM nueva a la que se le aplica la parte y
se reinicia Android una vez.

| Palanca | Usada | Anónima | Servicios · procesos | Huella en el Mac | dump | screencap |
|---|---|---|---|---|---|---|
| línea base | 840 MiB | 452 MiB | 46 · 83 | 1571 MiB | 1,97 s | 0,38 s |
| servicios + features | **774** (−66) | 411 | 36 · 68 | 1571 | 1,95 s | 0,39 s |
| `ro.config.low_ram` sola | 824 (−16) | 436 | 46 · 84 | 1570 | 1,95 s | ✓ |
| montículos/JIT/GL/lmkd | 825 (−15) | 440 | 46 · 83 | 1570 | ✓ | 0,41 s |
| pantalla 480×800 @240, 10 fps | 802 (−38) | 431 | 46 · 83 | 1569 | 1,98 s | **0,30 s** (PNG de 330 KB, no de 654 KB) |
| quitar las apps | no se pudo medir sola: las dos pasadas cayeron en la corrupción del Mac (abajo) | | | | | |
| **todo junto** (imagen `SLIM=1`, en frío) | **749** (−91) | 350 (−102) | 36 · 58 | 1486 | 1,99 s | 0,39 s |

Con todo junto también desaparecen los tombstones: en la base, la HAL simulada de
Bluetooth y `com.android.bluetooth` mueren en bucle (22–24 tombstones en el
primer minuto, más de 5 MiB de `/data`, que está en RAM). `/data` baja de 27 a
16 MiB.

**Con 1,5 GiB la huella casi no cambia (1571 → 1486 MiB)**, porque en vz la
VM paga en el Mac todas las páginas que el invitado ha tocado alguna vez, y
Android llena de caché de páginas toda la RAM que se le dé. Lo que ahorra
SLIM=1 en el invitado solo llega al Mac bajando `-mem`: es lo que permite
darle menos RAM sin que se rompa.

## Bajando `-mem` (arranque en frío, sin reinicios)

La imagen SLIM de estas filas usa `DATA_MODE=tmpfs` y no tiene apps instaladas,
salvo la fila marcada como overlay.

| Imagen | `-mem` | Huella en el Mac | Disponible dentro | `boot_completed` | dump | screencap | ¿Va? |
|---|---|---|---|---|---|---|---|
| SLIM | 1536 | 1486 MiB | 985 MiB | 9,0 s | 1,99 s | 0,39 s | ✓ |
| SLIM | 1024 | 1059 MiB | 489 MiB | 7,7 s | 1,96 s | 0,29 s | ✓ |
| **SLIM** | **768** | **803 MiB** | 242 MiB | 6,5 s | **2,0 s** | 0,33 s | **✓ recomendada** |
| SLIM + overlay, con Termux abierto | 768 | 823 MiB | 150 MiB | 7,8 s | 3,0 s con Termux delante | 0,55 s | ✓ |
| SLIM | 640 | 675 MiB | 123 MiB | 7,9 s | 2,7 s | 0,66 s | ✓ pero lento (vuelve a leer caché de disco) |
| SLIM | 512 | 546 MiB | 15 MiB | 9,6 s | ✗ (se agota el tiempo de 60 s) | 1,8 s | ✗ |
| base | 768 | 815 MiB | 91 MiB | 7,4 s | 2,9–3,6 s | 0,75 s | a duras penas: lmkd mató 10 procesos, 30 tombstones |

El suelo es la memoria anónima (~345 MiB: system_server, SystemUI, Launcher3,
surfaceflinger) más ~110 MiB del kernel. Por debajo de ~640 MiB no queda sitio
para la caché de las bibliotecas y cada dump las vuelve a leer del disco.

### Lo que NO se puede quitar (medido)

- **La HAL térmica** (`vendor.thermal-hal-2-0-mock`): sin ella system_server se
  queda para siempre en `StartHardwarePropertiesManagerService` y
  `boot_completed` no llega.
- **storaged y statsd**: StorageManagerService y otros los esperan en bucle
  (`storaged not found; trying again`).
- **audioserver / media**: zygote los relanza (`onrestart restart audioserver`) y
  AudioService arranca con system_server. No se probaron quitados.
- servicemanager, hwservicemanager, surfaceflinger, zygote, netd, vold, installd,
  keystore2, logd, lmkd, gralloc/hwcomposer, adbd y gatekeeper son el esqueleto.

### Lo que no sale a cuenta

- **`ro.config.low_ram`** y los montículos de Android Go: −15 MiB cada uno. Con
  poca RAM ayudan a que lmkd mate antes lo que está en caché. Se quedan porque no
  rompen nada.
- **El globo en vz** (`kling machine squeeze`): sin estadísticas del invitado
  aprieta hasta `mem/2`. Android se queda con 4 MiB disponibles, el dump deja de
  responder y solo devolvió 74–78 MiB. No sirve para Android.
- **`DATA_SIZE`**: es un techo. El tmpfs solo ocupa lo que se escribe (16 MiB
  tras arrancar). Lo que sí cuesta es instalar un APK: lo que se copia a
  `/data/local/tmp` y a `/tmp` de la VM (tmpfs) se queda en RAM hasta que se
  borra (Shmem subió a 188 MiB con Termux). Hay que borrar las copias después de
  `pm install`.
- **zram**: el kernel no lo lleva (`# CONFIG_ZRAM is not set`, y tampoco
  ZSMALLOC). Sería la siguiente palanca: con él, los ~345 MiB anónimos, que
  comprimen bien, dejarían bajar a 512. Fragmento propuesto para
  `kernel/config-android` (no probado):

  ```
  CONFIG_ZSMALLOC=y
  CONFIG_ZRAM=y
  CONFIG_CRYPTO_LZ4=y
  CONFIG_ZRAM_DEF_COMP_LZ4=y
  ```

  Más `swapon` de un `/dev/block/zram0` de ~256 MiB (el `fstab` de Redroid no
  lo trae; lo haría el lanzador).

## Restaurar desde un dorado en vz: lo que cuesta de verdad

| | En frío (imagen) | Restaurado de un dorado |
|---|---|---|
| hasta poder usarlo | 6,5–9 s (`boot_completed`) | 1,0–1,4 s (`run -from`), primer dump a los 3,6 s |
| huella SLIM con 1024 MiB | 1059–1074 MiB | 1490–1641 MiB (1784 tras abrir Termux) |
| huella SLIM con 768 MiB | 803 MiB | 1150–1600 MiB (1504, 1153–1336, 1389–1602 en tres tandas) |

**En vz, un clon restaurado cuesta 450–700 MiB más que su RAM**: es el proceso de
Apple (`com.apple.Virtualization.VirtualMachine`, el mismo exceso que el README
midió con 1,5 GiB). No se va con el tiempo, ni con pause/thaw (1490 → 1494), ni
con el globo. En el Mac caben por eso casi el doble de teléfonos arrancados en
frío que restaurados; restaurar solo compensa si el segundo y medio importa más
que la densidad.

Una huella de 830–880 MiB en un clon restaurado (visto dos veces) no quiere
decir que se haya ahorrado memoria. Es macOS mandando al swap parte de la VM
(`phys_footprint` no cuenta lo que está en swap).

## Cuántos caben

Presupuesto: el 70 % de 16 GiB, unos 11,2 GiB, como en el README.

| Configuración | Por teléfono | Teléfonos en el Mac |
|---|---|---|
| base, 1536 MiB, restaurado (README) | ~2,1 GiB | 5 |
| SLIM, 1024 MiB, restaurado | ~1,6 GiB | 7 |
| SLIM, 768 MiB, restaurado | ~1,3–1,5 GiB | 7–8 |
| SLIM, 1024 MiB, en frío | ~1,05 GiB | 10 |
| **SLIM, 768 MiB, en frío** | **~0,8 GiB** | **14** (visto: 4 a la vez con Termux abierto = 3284 MiB en total) |
| SLIM, 640 MiB, en frío (dump más lento) | ~0,67 GiB | 16 |

**Linux con Firecracker (estimado, sin medir aquí).** Allí los clones mapean
el `mem.file` del dorado con MAP_PRIVATE: la página que nadie escribe es una
sola en la caché del host para todos. Cada clon paga lo que ensucia después de
restaurar, más unos pocos MiB del VMM.
Intenté medirlo con Firecracker anidado en la VM Lima del Mac, pero no sirve:
la carga pasa de 11 y el watchdog de system_server lo reinicia. Tampoco se puede
medir desde dentro del invitado, porque el kernel no trae
`CONFIG_MEM_SOFT_DIRTY`, `IDLE_PAGE_TRACKING` ni `/proc/kcore`. Una cota: tras
restaurar, 3 dumps + 3 screencaps + abrir Termux sumaron 677 MiB de
`pgalloc` en el invitado. Es una cota superior muy holgada, porque cuenta cada
vez que se reutiliza una misma página. Sin haberlo medido, lo esperable es
**150–350 MiB por clon** para un uso de automatización y el dorado (≤768 MiB)
una sola vez. En un host de 64 GiB, dejando 4 GiB al sistema, saldrían
**~170–390 teléfonos por memoria**; antes se acabarán las CPU. Para medirlo en
el Proxmox x86 hay un script:
[`slim/mide-fc.sh`](../image/slim/mide-fc.sh) (Private_Dirty del proceso de
firecracker de cada clon: recién restaurado, a los 30 s y tras el trabajo).

Las cifras de 1 VM sola son limpias: pasaron la comprobación de integridad. Las
filas de "Cuántos caben" con varias VMs son una multiplicación y **no son
fiables mientras no se arregle la corrupción con varias VMs a la vez** (abajo).

## Prueba con varios teléfonos a la vez (no fiable por la corrupción)

- **4 en frío** (`android13slimov`, 768 MiB, arrancados uno tras otro con
  memorystatus ≥ 45): los 4 llegan a `boot_completed` en 7,5–9,0 s, y en los 4
  `pm install` de Termux y `am start` van bien (TotalTime 0,8–0,9 s). Unos 20 s
  después, en 3 de ellos Termux está en primer plano, el dump da 26 nodos
  (2,9–3,2 s) y screencap funciona (0,55–0,65 s); huella de 819–822 MiB cada
  uno, 3284 MiB los 4. En el cuarto, el `init` de Android murió y el lanzador lo
  relanzó (sin OOM: 282 MiB disponibles).
- **4 restaurados del mismo dorado** (`run -from`, 768 MiB, Termux instalado):
  los 4 restauran en 0,37–0,92 s y dan el primer dump a los 3,3–4,1 s. 20 s
  después, con los 4 en marcha, 3 están rotos: `surfaceflinger` corrupto en la
  caché (md5 distinto al de la capa) en dos, `CANNOT LINK ...libc++.so: .dynamic
  section header was not found` en uno y system_server muerto en dos. En otra
  tanda, 2 de 4 funcionaban y uno daba `FIPS integrity test failed` (el
  autotest de boringssl). Huella: 1494–1602 MiB cada uno.

Con el Mac libre (memorystatus 60–64, swap de 2,9 GiB) y **una sola** VM no lo
vi. Con varias a la vez pasa enseguida, restauradas o en frío. El agente de
verity está aislando la causa: disco uncached, copia propia de capa y base por
VM, y dm-verity.

## Lo que se rompe y dónde

- Por debajo de 640 MiB (512): el dump agota el tiempo.
- Quitando la HAL térmica, storaged o statsd: no arranca o se queda en bucle.
- `kling machine squeeze` en vz: el invitado se queda sin RAM y el dump muere.
- **Aparte de SLIM, y lo peor:** con el Mac bajo presión (swap de 7–9 GiB), la
  RAM del invitado se corrompe. Páginas de 4 KiB de la caché de páginas se
  quedan **enteras a cero**: `boot-framework.art`, `surfaceflinger` (SIGILL en
  `FrameTracker::FrameTracker()+0`), fuentes (`invalid font data`) y
  `runtime-permissions.xml` escrito con basura. Una vez incluso `/proc/filesystems`
  (el lanzador dijo que no había binderfs). Tras `drop_caches` los ficheros se
  releen bien del disco. Pasa sin globo, sin regulador de CPU y sin restaurar.
  Con una sola VM y el Mac bajo presión invalidó 4 de unas 20 ejecuciones; las cifras de arriba son de ejecuciones
  que pasaron una comprobación de integridad (md5 desde la caché de 5 ficheros
  calientes contra la capa). Se lo pasé al agente que investiga el SIGILL.
