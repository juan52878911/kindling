# El SIGILL tras restaurar: no es PAC, son páginas a ceros

Investigación del fallo abierto en el README ("Resultado en el Mac"): en un clon
restaurado y pausado varias veces, los procesos nuevos de Android morían con
`Fatal signal 4 (SIGILL), code 1 (ILL_ILLOPC)`, siempre en el mismo
desplazamiento de apexd. Se hizo en el MacBook Air M4 de 16 GiB (macOS 26.5.1),
con otros cinco agentes usando VMs a la vez y el Mac casi siempre en
`kern.memorystatus_level` 32–40, con el swap lleno (8–9 de 9,2 GB) y el disco al
97–98 %. VMs de `-mem 1024 -cpus 2 -cpu-pct 200 -egress none -allow-exec`.
Kernel y paquete: los de la fase 0 (`vmlinux-6.1.140-kindling-arm64-android`,
`android-fase0-20260927-233645.tar`).

## Conclusión

1. **No es PAC.** En arm64 Linux 6.1 un fallo de autenticación con FPAC llega
   como `SIGILL` con `ILL_ILLOPN` (`do_el0_fpac`). `ILL_ILLOPC` es una
   instrucción indefinida (o BTI). Comprobado en el invitado con dos ELF
   mínimos de 4 bytes de código: la palabra `0x00000000` da
   `unhandled exception: undefined instruction`, y `autiasp` con LR=0 da
   `unhandled exception: FPAC, ESR 0x0000000072000000`. Una página de código a
   ceros es justo lo que produce `ILL_ILLOPC` "siempre en el mismo sitio": la
   palabra 0 es `udf #0`.
2. **La causa son páginas de la caché de ficheros del invitado que se quedan a
   ceros.** Se reprodujo de tres formas (abajo). **Corregido el 29-09 (#87):**
   no es que virtio-blk devuelva un bloque a ceros, sino que la RAM del
   invitado pierde páginas enteras del host (16 KiB) ya leídas y verificadas;
   ver [La causa](#la-causa-la-ram-del-invitado-no-la-lectura-issue-87). Un binario o
   `.odex` leído así se ejecuta con una página de ceros. Android no vuelve
   porque init, zygote y cada proceso nuevo mapean esa misma página de la caché.
   Los binarios de Debian seguían bien porque la página mala era de un fichero
   de Android.
3. **Condición:** el Mac bajo presión de memoria de verdad (swap lleno, picos
   de reserva de varios GiB, o un `fork` que restaura 1 GiB de golpe). Con el Mac
   libre (`memorystatus_level` 65–70), 10 de 10 forks con carga salieron limpios.
   Con presión y picos es raro pero real: 1 bloque malo en ~730 pasadas de
   lectura de 1,3 GB, y 3 de 5 forks con carga rompieron el original.
4. **Mitigación de PAC: no se aplica.** `arm64.nopauth` o
   `CONFIG_ARM64_PTR_AUTH=n` quitarían una protección sin tocar la causa.

## La causa: la RAM del invitado, no la lectura (issue #87)

Tanda del 29-09-2026 en el mismo M4 (macOS 26.5.1, 25F80), con un daemon
privado y kling-vz de `main` (ea3a16e), kernel 6.1.140 de la fase 0 con
`DM_VERITY`/`DM_VERITY_FEC` y el parche de relectura, y la capa `android13`
con verity + FEC (`android13v`). Clones de un dorado de `-mem 1536 -cpus 2`.
Disco del Mac con **26–46 GiB libres** y la swap creciendo sin tope (5–19 GiB).
Carga: `ceros/lecturas-canario.sh` (10 pasadas de `drop_caches` + md5 de
1,3 GB por ciclo, y un **canario** de 256 MiB anónimos con un patrón no nulo
por página, comprobado cada 2–5 s, que dice el PFN de cada página mala y el
de sus vecinas de la misma página de 16 KiB del host).

**Resultado: el Mac pierde páginas de 16 KiB de la RAM de la VM, y vuelven
llenas de ceros.** No es la lectura de virtio-blk:

1. **verity no ve nada.** 47 lecturas malas de ficheros de la capa (vdc, la
   que va por dm-verity; y 1 de la base, vda) en las tandas con verity, y **0** líneas
   `device-mapper: verity` (ni relecturas ni FEC ni EIO). El bloque entró en
   la caché verificado y bueno, y se quedó a ceros después.
2. **El canario también cae**, y no lo toca ninguna E/S: memoria anónima,
   reservada al principio y nunca liberada. En una ráfaga (e4, un instante):
   40 páginas de 4 KiB a ceros enteras, en 25 grupos de 16 KiB (PFN & ~3), y
   en todos los grupos con más de un canario, **todos** los canarios del grupo
   a ceros (0 contraejemplos). Es la granularidad de la página del host, no la
   de un bloque de disco. Las páginas de fichero sueltas de 4 KiB que se veían
   antes son lo mismo: las otras 3 páginas de su grupo eran de otra cosa.
3. **El kernel del invitado se rompe igual**: pánicos con estructuras del
   kernel a cero (`__mod_lruvec_page_state` con NULL+8, `_binder_inner_proc_lock`
   con NULL+0x238, `free_pgd_range`, "Attempted to kill init"). La caché de
   páginas solo es lo más visible porque es casi toda la RAM.
4. **A veces vuelve sola.** Páginas de fichero mapeadas (no se pueden soltar
   con `drop_caches`) leídas a ceros en varias pasadas seguidas y bien en la
   siguiente, con el mismo PFN; y lecturas malas que al releer al instante ya
   están bien (`bytes=0`). El invitado no reescribe esas páginas: lo que cambia
   es lo que el host le enseña en esa dirección física.

### Qué hace falta para que salga

| Tanda | VMs | tope CPU | presión | ciclos (×10 pasadas) | md5 malos | canario | pánicos |
|---|---|---|---|---|---|---|---|
| e1, sin verity | 1 | 200 (sin freno) | picos de 5 GiB, nivel 31–69 | 5 | 1 (2 págs. de SystemUI.apk) | — | — (system_server murió) |
| e2, verity | 1 | 200 | picos de 5 GiB | 14 | 5 (vdc), verity 0 | — | 2 |
| e3, verity | 1 | 200 | **sin picos**, nivel 32–40 | 11 | 0 | 0 antes del pánico | 1 + racha de SIGILL (libutils.so), invitado colgado |
| e4, verity | 1 | 200 | sin picos, nivel 35–60 | 13 | 0 | **40** (una ráfaga) | 1 |
| e5, verity | **2** | 200 | sin picos, nivel 33–50 | 3 + 3 | 0 | **131** + 1 | 1 |
| m1–m2 (default, uncached, cached ×2) | 1 | 200 | sin picos, nivel 37–50 | 6 × 8 | 0 | 0 | 0 |
| d1 default / uncached / cached | 2 | 200 | sin picos, nivel 35–50 | 2 × 8 cada uno | 3 / 0 / 0 | 0 / 0 / 0 | 0 |
| r1 (freno de señales) | 2 | **50** | sin picos, nivel 34–39 | 2 × 8 | **38** | **125** | **3** |
| d1 freno de señales | 2 | 50 | sin picos, nivel 47–53 | 2 × 8 | 2 | 0 | 0 |
| d1 freno de pausa (prueba: kling-vz con la pausa del framework en vez de SIGSTOP) | 2 | 50 | sin picos, nivel 61–66 | 2 × 8 | 0 | 0 | 0 |
| p1 freno de señales | 2 | 50 | picos de 3 GiB, nivel 30–60 | 8 + 6 | **621** | 81 | 3 (+1 colgada: "Bad page cache") |
| p1 freno de pausa | 2 | 50 | picos de 3 GiB | 2 × 8 | 0 | **76** | 1 (y la otra con el ext4 roto, "Structure needs cleaning", a los 11 s de restaurar) |
| p1 freno de señales, **`uncached`** | 2 | 50 | picos de 3 GiB, nivel 30–67 | 2 × 4 (parada: disco a 24 GiB) | **210** | **2607** | 6 |
| c1 / c2: solo canario (restaurada / en frío), sin lecturas | 1 + 1 | 200 | sin picos | 5 min | — | 0 | 0 |
| c3: solo canario | 2 | 50 | sin picos | 8 min | — | 0 | 0 |
| c4: solo canario | 2 | 50 | picos de 4 GiB, nivel ≥ 20 | 8 min | — | 0 | 0 |

Además, sin ninguna carga mía: al hacer un dorado en frío (sin picos, nivel
49) una comprobación caché frente a `O_DIRECT` dio un `MediaProvider.apk`
distinto; y el agente de `ext/phone` vio páginas a ceros en 2 de 5 arranques
en frío con el Mac cargado, y un dorado que pasó la comprobación y guardó una
página rota de `libart.so` en todos sus clones: `kling save` congela la RAM
tal cual, así que un dorado guardado tras una pérdida la reparte.

Lo que dicen los datos:

- **Disco libre: no hace falta el disco lleno** (pregunta 1). Todas las tandas
  de arriba tenían 26–46 GiB libres y la swap podía crecer. Tampoco hace falta
  que la swap encoja: en e5 la ráfaga salió con el tamaño de la swap quieto.
  Lo que acompaña a cada ráfaga es el **compresor del Mac trabajando a
  tope** (100 000–290 000 compresiones y descompresiones de páginas cada 2 s,
  `memorystatus_level` bajando a 30–35). Con el Mac holgado todo el rato
  (nivel > 55 y sin picos) no salió en ninguna tanda.
- **Hace falta E/S de disco en el invitado.** El canario solo, sin lecturas,
  no cayó ni una vez en 42 minutos-VM (con el freno de señales y con picos de
  4 GiB en el Mac). Con las lecturas, caen también páginas que no son de E/S
  (el canario). O sea: la E/S de virtio-blk dispara la pérdida, pero lo que
  se pierde no es solo lo que se lee.
- **La caché del disco no lo cambia** (pregunta 2): con la carga que lo hace
  salir, `default` 3, `uncached` 0 y `cached` 0 lecturas malas en 160 pasadas
  cada uno; a esta tasa eso no distingue nada, y no puede explicar el canario,
  que no pasa por el disco. Y con más presión (p1), `uncached` salió peor que
  nada: 210 lecturas malas, 2607 páginas de canario y 6 pánicos en 8 ciclos.
  NVMe no se probó: el kernel no trae `BLK_DEV_NVME`, la raíz es `/dev/vda`
  fija, y lo que se pierde no pasa por el dispositivo de disco.
- **Una o dos VMs** (pregunta 3): sale con una sola (e2, e3, e4). Con dos a la
  vez sale antes (e5: a los ~60 s de crear cada una, cada una a su hora, no a
  la vez), por la presión que meten, no por compartir la capa: lo que se pierde
  es sobre todo memoria anónima.
- **El tope de CPU lo empeora; SIGSTOP o pausa, da igual** (pregunta 4). Sin
  regulación (`-cpu-pct 200` con 2 vCPU) sale. Con `-cpu-pct 50`, que para el
  auxiliar de Apple 50 veces por segundo, salió mucho más a la misma presión
  (r1, nivel 34–39: 38 lecturas, 125 páginas de canario, 3 pánicos, frente a
  3 / 0 / 0 sin freno). Se probó un kling-vz que regula con la pausa del
  framework en vez de SIGSTOP: sin picos, con el Mac más libre, limpio; con
  picos de 3 GiB, 76 páginas de canario, un pánico y un ext4 roto. No lo
  arregla, así que no se queda en el código. `kling pause` no hace falta:
  ninguna de estas tandas pausa.

### Qué es, y qué no se puede arreglar en kindling

Un fallo de macOS (Virtualization.framework o el compresor de memoria de XNU
con la memoria de una VM de Hypervisor.framework): con el compresor apretando
la memoria del auxiliar `com.apple.Virtualization.VirtualMachine` (su RSS baja
a 200–400 MiB de 1,5 GiB) y E/S de virtio-blk a la vez, algunas páginas de 16
KiB de la RAM del invitado vuelven a ceros. kindling no toca esa memoria
(`vz/internal/footprint` solo lee `phys_footprint`; no hay `madvise` ni globo
inflado: `balloon_inflate 0`), y no hay forma de fijarla desde fuera: la
reserva el auxiliar, que es un binario de la plataforma (`task_for_pid` se
niega). NVMe (`VZNVMExpressControllerDeviceConfiguration`) cambiaría el
dispositivo, pero lo que se pierde no pasa por él; además el kernel no trae
`BLK_DEV_NVME` y la raíz es `/dev/vda` fija.

**Mitigación recomendada por defecto** (no hay arreglo en kindling):

1. **No guardar dorados en un Mac con presión.** `kling save` congela la RAM
   tal cual: una página perdida antes del guardado va a todos los clones (lo
   vio el agente de `ext/phone` con `libart.so`). Hacer los dorados con el Mac
   holgado (nivel > 60, sin otras VMs), comprobar la caché contra `O_DIRECT`
   justo antes del `save`, y rehacerlos si hubo una ráfaga.
2. **Android sin tope de CPU** (`cpu_pct_per_vcpu: 100`, como ya lleva la
   receta) y no más de una VM de Android a la vez en un Mac de 16 GiB.
3. **dm-verity sigue puesto**, pero como detector de la lectura, no como
   protección: el fallo de verdad lo esquiva.
4. **Tratar como rota una VM que haya dado un SIGILL o un pánico**: borrarla y
   volver a clonar, no reiniciar Android dentro (la página mala sigue en su RAM).
5. Admisión: `KLING_MIN_MEM_LEVEL` y `KLING_MAX_SWAP_PCT` bajan el riesgo pero
   no lo quitan (salió con nivel 30–50).

### Informe para Apple (Feedback Assistant)

- **Producto**: macOS 26.5.1 (25F80), MacBook Air M4 (Mac16,1), 16 GiB.
  Virtualization.framework, `VZVirtualMachine` Linux arm64 (kernel 6.1.140,
  páginas de 4 KiB), 2 vCPU, 1,5 GiB, tres `VZVirtioBlockDeviceConfiguration`
  sobre `VZDiskImageStorageDeviceAttachment` (raw, caché automática; igual con
  `.cached` y `.uncached`), un globo tradicional sin inflar.
- **Qué pasa**: con el Mac bajo presión de memoria (`kern.memorystatus_level`
  30–50, compresor activo) y el invitado leyendo del disco sin parar, páginas
  de 16 KiB de la memoria física del invitado pasan a leerse como ceros.
  Afecta a memoria anónima del invitado que no es destino de ninguna E/S, a la
  caché de páginas ya verificada por dm-verity y a estructuras del kernel
  (pánicos). A veces el contenido correcto vuelve más tarde en la misma
  dirección física.
- **Tasa**: con 2 VMs, en 1–2 min (131 páginas en 30 s en una de ellas); con
  una, 1 ráfaga cada ~10 min con el Mac en nivel 35–50. Si la VM se para y se
  reanuda a menudo (SIGSTOP/SIGCONT del proceso, o `pause`/`resume` del
  framework, a 50 Hz), bastante más. Nunca sin E/S de disco en el invitado;
  nunca con el Mac holgado. Igual con la caché de disco automática, `.cached`
  y `.uncached`.
- **Pasos**: el invitado corre `i87probe canary 256 2` (memoria anónima con
  patrón, `prototypes/android/ceros/i87probe.c`) y en bucle `echo 3 >
  /proc/sys/vm/drop_caches; md5sum` de ~1,3 GB de ficheros del disco
  (`prototypes/android/ceros/lecturas-canario.sh`); en el Mac, otra VM igual o
  procesos que reserven 3–5 GiB durante 15 s cada 30 s. El canario escribe en
  la consola cada página a ceros con su PFN; se ven páginas alineadas a 16 KiB
  enteras a cero.
- **Esperado**: la memoria del invitado nunca cambia sin que el invitado la
  escriba.

## Cómo se reprodujo

| Prueba | Qué | Resultado |
|---|---|---|
| `stress-restore.sh -modes fork -heavy 1` (Mac en nivel 33–35, swap lleno) | `sandbox create -from` → carga (`am start`, `uiautomator dump`) → `sandbox fork` → carga en las dos | **3 de 5 forks rompieron el ORIGINAL** (la copia, sana): `services.odex` con `Check failed: num_methods != 0u` (datos a cero), systemui con `NoSuchMethodError: No virtual method ()B in class B` (nombres basura), launcher3 con `InsnsSizeInCodeUnits()=0`, `Corrupt heap detected` en system_server |
| mismo fork con el Mac libre (nivel 65–70) | 10 ciclos | 10 limpios |
| `kling save` de un clon vivo (sin copia) | commit y carga en el original | limpio |
| 10 × `run -from` con carga | | limpio |
| clon de sg-gold + picos de 4 GiB aleatorios en el host cada ~35 s, carga tras cada uno | | **ronda 7**: system_server `Scudo ERROR: corrupted chunk header` |
| lo mismo en frío | 10 rondas | limpio (una sola muestra; no descarta el frío, ver la fila siguiente) |
| **lecturas en frío y sin Android**: `drop_caches` + `md5sum -P16` de 2362 ficheros (1,3 GB de `/android/system` en vdc y `/usr` en vda) contra una referencia de dos pasadas iguales, con picos de 4 GiB en el host | 730 pasadas | **1 bloque malo**: `services.odex`, página de 4 KiB n.º 11165, entera a ceros (3431 bytes distintos, todos 0 en la versión mala), igual en dos pasadas seguidas, las dos tras `drop_caches`; `dd iflag=direct` en la misma VM leía lo correcto. Además, un md5 malo pasajero de `boot.oat`. dmesg sin errores de virtio_blk |
| **`stress-restore.sh -modes run,pause,reads,freeze -hostspike 3`**, clon de sg-gold con Android en marcha | 7 ciclos | **ciclo 7** (reads, tras un pause con el Mac en nivel 22): `/system/lib64/libcutils.so`, página 3 **entera a ceros** (3996 bytes distintos, 0 no nulos), igual en dos pasadas y tras 3 `drop_caches` más. `dd iflag=direct` da el md5 bueno. La página mala la mapean 28 procesos (PFN 0xa2c7e, `kpagecount`=28), así que `drop_caches` no puede soltarla: todo proceso nuevo que cargue libcutils ve esos ceros. Es exactamente el SIGILL del README |
| memoria anónima: 2 copias de 64–128 MiB aleatorios en tmpfs, md5 en bucle | >800 iteraciones, con y sin presión, y en cada ronda de picos | limpio |
| `shasum` de la capa (`android13.layer.ext4`) en el host, a la vez que las lecturas y los picos | 41 veces | limpio: el fichero y la caché del host están bien |

El agente de densidad vio lo mismo por su cuenta: una página de 4 KiB suelta a
ceros en la caché de ficheros (surfaceflinger, un `.ttc`). También en VMs
arrancadas en frío, y más tras reiniciar Android. El de uidump lo vio también:
`pgrep` con "unsupported version 0 of Verneed record" y un `awk` muerto con
SIGILL. Que el daño sea de 4 KiB sueltos (y no de 16 KiB alineados, la página
del host) descarta que macOS tire memoria de la VM. Apunta al camino de lectura
de disco.

### Hipótesis descartadas

- **PAC / claves mal restauradas.** Ver arriba: el código de señal no cuadra y
  el fallo aparece en VMs que nunca se restauraron.
- **RAM restaurada respaldada por el `mem.file`**, que kindling borrara o
  reescribiera. `lsof` y `vmmap` del proceso `com.apple.Virtualization.VirtualMachine`
  de un clon restaurado solo tienen abiertos `vmlinux`, la base, el overlay y
  la capa: ningún `mem.file`. Tras `RestoreMachineState` la memoria es anónima.
- **Globo.** El globo de vz no ofrece `FREE_PAGE_HINT` ni `REPORTING` (features
  del virtio 5: solo `MUST_TELL_HOST` y `DEFLATE_ON_OOM`), y no se infló.
- **Regulador de CPU.** Con `-cpu-pct 200` y 2 vCPU no pausa nada.
- **Reloj.** El `uptime` del invitado avanza como el del host (medido: ratio 1,01).

### Lo que quedaba abierto (resuelto el 29-09)

- **Dónde se pierden los datos**: en la RAM, no en la lectura. Ver "La
  causa". (El grupo de 16 KiB de libcutils, con otra página a ceros al lado, ya
  lo insinuaba.)
- **Si el modo de caché del disco lo arregla**: no; `uncached` también falla.
- **Si influye el disco del host lleno**: no hace falta; sale con 26–46 GiB
  libres.

## Mitigaciones

1. **dm-verity sobre la capa** ([`verity.md`](verity.md)): hecho. Detecta y
   corrige lecturas malas, pero el fallo de verdad es en la RAM y no lo ve.
2. **Admisión en kindling** por swap y disco: hecha (`docs/estabilidad.md`
   §9). Baja el riesgo, no lo quita.
3. **`uncached`, NVMe**: `uncached` probado, no sirve; NVMe no cambiaría lo
   que se pierde. Queda el informe a Apple (arriba) y las mitigaciones de
   "Mitigación recomendada por defecto".

## Reproducirlo

```sh
KLING_HOST=unix://$HOME/.kindling-android-X/kling.sock \
  prototypes/android/stress-restore.sh -from <dorado> -cycles 100 \
    -modes run,pause,freeze,reads,fork -heavy 5 -hostspike 3 -memlevel 25
```

`reads` es la comprobación directa (lecturas contra referencia, con la página y
si es a ceros). Los demás modos buscan las consecuencias en Android. Al primer
fallo el script se para, deja la VM viva y guarda tombstones, logcat, dmesg y
los binarios de los backtraces en `results/stress-*/fallo-N/`.
`-hostspike` mete presión en el Mac entero: úsalo con cuidado si hay más VMs.

## Cuántos ciclos se hicieron

- Operaciones reales del núcleo con comprobación en Android: 46 ciclos, entre
  `stress-restore.sh` y las pruebas a mano (run 14, pause 4, freeze 3, fork 21,
  save 1, reads 3), más 8 + 10 rondas de picos de 4 GiB con carga (clon y frío).
  Con picos de 5 GiB, un fork dio un falso positivo: `am start` no terminó bajo
  el pico; nada corrupto, y la misma carga pasó después.
- Lecturas contra referencia: 880 pasadas en frío sin Android (730 con el modo
  de caché por defecto y 150 con `uncached`), 3 × 3 con Android.
- No se llegó a los 100 ciclos seguidos. La tanda final se paró al primer fallo,
  en el ciclo 7, que es lo que se buscaba. Otra tanda con fork no pudo seguir:
  el daemon se negó a crear más máquinas porque el Mac se quedó con 5,6 GiB
  libres (`KLING_MIN_FREE_DISK_MIB`).
