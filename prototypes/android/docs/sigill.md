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
2. **La causa son páginas de la caché de ficheros del invitado que llegan a
   ceros.** Se reprodujo de tres formas (abajo). El caso más limpio es sin
   Android y en frío: el disco virtio-blk devuelve un bloque de 4 KiB lleno de
   ceros, el invitado lo da por bueno y lo guarda en su caché. Un binario o
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

### Lo que queda abierto

- **Dónde se pierden los datos exactamente**: o virtio-blk de
  Virtualization.framework entrega ceros al leer, o una página ya en la RAM del
  invitado se queda a ceros. El fichero y la caché del host están bien, y la
  relectura directa da lo correcto. En el caso de libcutils la referencia (ciclo
  3) leyó bien esa misma página, y en el ciclo 7 estaba a ceros. Pero el
  invitado había reclamado y vuelto a leer mucha caché entre medias
  (`workingset_refault_file` 267 000), así que no distingue "se leyó mal" de "se
  borró en RAM". Su grupo de 16 KiB de RAM del invitado (PFN 0xa2c7c–f, lo que
  sería una página del host): dos páginas libres, la de libcutils a ceros y una
  anónima de mediaextractor también a ceros. Esa última es la primera página de
  una pila, que puede ser cero legítimamente: no prueba nada.
- **Si el modo de caché del disco lo arregla.** `kling-vz` acepta ahora
  `KLING_VZ_DISK_CACHING=automatic|cached|uncached` y
  `KLING_VZ_DISK_SYNC=full|fsync|none` (sin ellas, lo de siempre). Con
  `uncached`, 150 pasadas limpias a 2,9 s por pasada (con el modo por defecto,
  ~3,1 s). Con una tasa de 1 en ~730 no basta para decir nada; hace falta una
  carga que lo dispare más a menudo.
- **Si influye el disco del host lleno** (swap sin sitio para crecer). No se ha
  podido probar con disco libre.

## Mitigaciones propuestas (no aplicadas aquí)

1. **Detectar en el invitado**: dm-verity sobre la capa de Android, o fs-verity
   por fichero. Una lectura a ceros daría EIO en vez de código corrupto, y el
   kernel reintentaría la lectura (necesita `DM_VERITY` en el kernel y el árbol
   de hashes en la imagen). **Hecho** para la capa: [`verity.md`](verity.md)
   (relee, corrige con FEC o da EIO; no se pudo reproducir el fallo para
   contarlo bajo presión).
2. **Admisión en kindling**: además de `KLING_MIN_MEM_LEVEL` (15 % por
   defecto), negarse a restaurar o avisar con el swap del Mac casi lleno o el
   disco por debajo de unos GB. Ninguno de los fallos se vio por debajo del 15 %,
   así que el umbral actual no protege.
3. **Probar `uncached` y NVMe** (`VZNVMExpressControllerDeviceConfiguration`, en
   Code-Hex/vz v3.7.1) con una carga que reproduzca más a menudo, y reportar a
   Apple (Feedback) con el `readtest`.

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
