# dm-verity sobre la capa de Android

Respuesta a la mitigación 1 de [`sigill.md`](sigill.md): con el Mac bajo
presión, bloques sueltos de 4 KiB de la capa de Android (vdc) llegaban a la
caché del invitado enteros a ceros, sin error de virtio_blk, y un `.so` o un
`.odex` leído así daba SIGILL. Con la capa detrás de dm-verity, un bloque que
no cuadra con su hash no entra en la caché como bueno.

Hecho en el MacBook Air M4 de 16 GiB (macOS 26), con el kernel de la fase 0
más `DM_VERITY` y la capa `android13` de la fase 0 (la misma de `sigill.md`).

## Cómo va

```
vdc (android13.layer.ext4) = | ext4 (2559 MiB) | árbol sha256 (20 MiB) | FEC RS(255,253) (20 MiB) |
                                   │
init de la base (/sbin/overlay-init) ── dmsetup create android-layer (tabla en /etc/kindling-android/layer.verity)
                                   │
/dev/mapper/android-layer ── ext4 ro ── lower del overlay ── /android
```

- **Construcción** (`image/verity.sh`, o `VERITY=1 build-image.sh`, en Lima):
  `veritysetup format --no-superblock` con el árbol y el FEC pegados detrás del
  ext4 en el MISMO fichero (kindling engancha la capa como un solo disco, y el
  ext4 ignora lo que hay tras su último bloque: la misma capa arranca igual con
  una base sin verity). La tabla sale de un `veritysetup open` real; los
  argumentos de FEC se añaden a mano porque el kernel de Ubuntu 24.04 de Lima
  no trae `DM_VERITY_FEC` ("Invalid number of feature args").
- **Raíz del hash**: en la tabla, dentro de la base (`/etc/kindling-android/layer.verity`),
  y en la receta (`spec.verity_root_hash`). La base no va verificada: el kernel
  la monta directamente como raíz (`root=/dev/vda`, línea de comandos fija del
  núcleo), y dm-verity sobre un disco ya montado no se puede poner después
  (los dos lo abren en exclusiva). Hacerlo exigiría `dm-mod.create=` en la
  línea de comandos, es decir, tocar el núcleo. Queda fuera.
- **Montaje**: el init de la base (el `minimal-init` de kindling) crea
  `/dev/mapper/android-layer` con `dmsetup` (instalado en la base, 1 MiB) y
  monta la capa desde ahí antes del overlay. Si `dmsetup` falla, el init se
  para: montar la capa sin verificar sería volver al fallo de siempre. El
  resto (overlay, `/entrypoint`, el lanzador de Android) no cambia.
- **Por qué dm-verity y no fs-verity**: la capa es de solo lectura entera, y
  dm-verity cubre también los metadatos del ext4 (directorios, inodos,
  extents), que fs-verity no protege; además no hay que activarlo fichero a
  fichero (`fsverity enable` en cada fichero de la capa, con el ext4 montado
  en escritura y `-O verity`), y trae la relectura y el FEC hechos.
- **Kernel** (`config-android`): `MD`, `BLK_DEV_DM`, `DM_VERITY`,
  `DM_VERITY_FEC`, `CRYPTO_SHA256` y `CRYPTO_SHA2_ARM64_CE` (dmesg:
  `sha256 using implementation "sha256-ce"`).

## ¿Detecta, reintenta, corrige?

Modo de error: el de por defecto, EIO. Ni `ignore_corruption` ni
`restart/panic_on_corruption`.

Leído en el árbol 6.1.140 (`drivers/md/dm-verity-target.c`), un bloque de
datos que no cuadra pasa por tres escalones antes de dar EIO:

1. **`verity_recheck`**: lo vuelve a leer del disco, a un búfer aparte
   (`dm_io`, sin caché), y lo vuelve a hashear. Si ahora cuadra, copia lo
   bueno a la página de la petición y sigue. Es exactamente "EIO + relectura",
   pero dentro de dm-verity y sin que el de arriba vea el error. Está en 6.1
   por un backport de estable (upstream 6.8). **En silencio**: sin el parche
   de abajo no deja rastro.
2. **FEC**: si la relectura tampoco cuadra, Reed-Solomon. Lee de nuevo del
   disco los 253 bloques del mismo código (incluido el malo) y corrige hasta
   1 bloque malo por código con 2 raíces (un bloque entero a ceros es un
   borrado de 1 byte en cada uno de sus 4096 códigos intercalados).
3. **EIO** (`verity_handle_err`, "data block N is corrupted"). Encima, la
   caché de páginas lo reintenta una vez (`filemap_read_folio` en
   `filemap_fault` y `filemap_update_page`: "Try to re-read it _once_").

Los **bloques de hashes** no tenían el primer escalón: se leen por `dm-bufio`,
que los guarda en caché. Uno leído a ceros quedaba en bufio sin verificar y
fallaba cada vez que se consultaba, para los 128 bloques de datos (512 KiB)
que cuelgan de él, hasta que bufio lo soltara. El parche
[`kernel/patches/dm-verity-reread.patch`](../kernel/patches/dm-verity-reread.patch)
(`kernel/build.sh` lo aplica a la copia del builder; `NO_PATCHES=1` lo salta):

- bloque de hashes que no cuadra → `dm_bufio_forget` y otra lectura, una vez,
  antes de FEC; deja "metadata block N is corrupted, re-reading it";
- relectura de datos que arregla algo → "data block N was corrupted on read,
  re-read ok". Sin esto no se podía contar.

Probado en el invitado (kernel con el parche) con una imagen de 24 MiB con
verity + FEC y dos copias estropeadas a mano: un bloque de datos entero a
ceros y el bloque de hashes de arriba a ceros (el fallo "persistente"; el de
verdad es pasajero, que es lo que arregla el primer escalón):

| imagen | sin FEC en la tabla | con FEC |
|---|---|---|
| buena | md5 bueno, estado `V` | md5 bueno, `V` |
| bloque de datos a ceros | **EIO** (`md5sum: Input/output error`), "data block 2920 is corrupted", estado `C` | **md5 bueno**, "FEC 81920: corrected 4086 errors" |
| bloque de hashes a ceros | "metadata block 6144 is corrupted, re-reading it" → corrupted → EIO, el ext4 no monta | "re-reading it" → FEC lo corrige, **md5 bueno** |

Así que con este kernel: un cero pasajero lo arregla la relectura (1), uno
que se repite lo arregla el FEC (2), y solo si los dos fallan sale EIO (3),
que la caché de páginas reintenta una vez más. En ningún caso entra en la
caché una página mala: o es buena, o es EIO.

## Coste

VMs de `-mem 1536 -cpus 2 -cpu-pct 200`, una a la vez, 3 repeticiones de
cada, Mac con `memorystatus_level` 40–80 y otras VMs corriendo (hay ruido de
±1,5 s en el frío):

| | sin verity | con verity |
|---|---|---|
| `boot_completed` en frío | 4,8 / 6,1 / 6,4 s | 5,4 / 7,6 / 7,9 s |
| `kling save` | 1,59 s | 1,34 s |
| `kling run -from` (2.ª y 3.ª; la 1.ª lee el estado del disco) | 0,69 / 0,63 s | 0,68 / 0,65 s |
| restaurar → proceso nuevo de Android | 0,75 / 0,68 s | 0,72 / 0,69 s |
| primer `uiautomator dump` tras restaurar | 1,96–1,99 s | 1,95–2,01 s |
| `uiautomator dump` | 1,93–1,94 s | 1,93–1,94 s |
| memoria usada en el invitado (`free`, reposo) | 704–736 MiB | 704–722 MiB |
| caché de hashes (`dm_bufio` `current_allocated_bytes`) | — | 2,0–3,1 MiB |
| una pasada de lectura en frío de 2362 ficheros (1,3 GB, `md5sum -P16` tras `drop_caches`) | 2,5 s | 2,8 s (+11 %) |
| disco | capa 2559 MiB | +41 MiB (árbol 20 MiB + FEC 20 MiB), base +1 MiB (dmsetup) |

Restaurar, `dump` y la memoria no cambian: tras restaurar, lo que Android usa
ya está en la caché del invitado (verificado cuando se leyó). El frío paga el
hash de lo que lee al arrancar: ~1 s en las medias, dentro del ruido del Mac
compartido. El único coste claro es leer mucho en frío: +11 %.

## Bajo presión: ¿desaparece el código corrupto?

`stress-restore.sh -modes pause,reads,freeze,reads -read-passes 10 -heavy 4
-hostspike N -keep-going` sobre un clon de un dorado de 1536 MiB con Android en
marcha: cada `reads` son 10 pasadas de `drop_caches` + md5 de 2362 ficheros
(1,3 GB de `/android/system` en vdc y `/usr` en vda) contra la referencia; cada
4 ciclos, `am start` + `uiautomator dump`; en todos, procesos nuevos, SIGILL en
`logcat -b crash` y dmesg, y system_server.

| tanda | picos | ciclos | pasadas de lectura | md5 malos | SIGILL | líneas de verity |
|---|---|---|---|---|---|---|
| verity | 3 GiB | 60 | 300 | 0 | 0 | 0 |
| sin verity | 3 GiB | 60 | 300 | 0 | 0 | — |
| sin verity, `uncached` | 3 GiB | 60 | 300 | 0 | 0 | — |
| verity | 4 GiB | 60 | 300 | 0 | 0 | 0 |
| sin verity | 4 GiB | 55 | 270 | 0 | 0 | — |
| sin verity, `uncached` | 4 GiB | 15 (parada: disco a 2,4 GiB) | 70 | 0 | 0 | — |

**El fallo no se reprodujo en ninguna tanda, tampoco sin verity**, así que
estas cifras no demuestran que verity lo quite: demuestran que no estorba
(0 falsos positivos en 600 pasadas y 60 pausas/congelaciones) y lo que cuesta.
La prueba de que detecta y corrige es la de la sección anterior (bloques a
ceros de verdad). Por qué no salió:

- **El Mac estaba libre**: `memorystatus_level` 57–82 fuera de los picos (en
  `sigill.md` fallaba con 22–40, la swap llena y 5–6 VMs de otros agentes).
  Con un Mac libre, `sigill.md` ya vio 10 de 10 limpios.
- **No se pudo meter más presión sin arriesgar el Mac**: la swap estaba llena
  (5,2–6,0 de 6–7 GiB) y el disco al 99 % (2–8 GiB libres, moviéndose por
  otros agentes). Un pico de 7 GiB hizo crecer la swap y bajó el disco de 8 a
  2 GiB en un ciclo; uno de 6 GiB, a 2,5 GiB. Se pararon (y `pico` ahora se
  corta solo con menos de 3 GiB libres). A 4 GiB, un bajón del disco a 2,3 GiB
  (no solo nuestro) paró la tanda sin verity en el ciclo 55.
- En `sigill.md` la tasa era de 1 bloque malo en ~730 pasadas en frío; 300 por
  tanda no llegan.

Queda por hacer, en un Mac con disco libre (o cuando vuelva a haber presión):
la misma tanda de 730+ pasadas con picos de 5–6 GiB, verity y sin verity.
Si verity lo coge, `verity.txt` tendrá "data block N was corrupted on read,
re-read ok" (o "FEC ... corrected"), y los md5 seguirán a 0.

## ¿Basta `uncached`?

Sin respuesta todavía, por lo mismo: con la caché de disco por defecto
tampoco falló, así que `uncached` limpio no distingue nada (370
pasadas con `uncached`, más las 150 de `sigill.md`, todas limpias; la variable
llega a kling-vz: `KLING_VZ_DISK_CACHING=uncached` en su entorno). Coste:
ninguno medible en lecturas en frío (2,53 s por pasada contra 2,51 s).

## Lo que verity no cubre

- **La base (vda)**: sin verificar (arriba, por qué). Sus bloques pueden
  seguir llegando a ceros: en `sigill.md` pasó en `/usr`.
- **Páginas ya en la RAM del invitado.** verity comprueba al LEER del disco.
  Si una página buena se pone a ceros después, en RAM (la otra hipótesis de
  `sigill.md`), no lo ve. Tampoco lo que trae un dorado: su caché de páginas se
  restaura tal cual, sin pasar por verity. Si un fallo aparece con verity
  puesta y sin líneas de verity en dmesg, apunta a la RAM y no al disco: eso
  es lo que esta prueba sirve para separar. **Separado el 29-09 (#87):** 47
  lecturas malas de la capa con verity puesta y 0 líneas de verity; es la
  RAM (páginas de 16 KiB del host a ceros, también en memoria anónima). Ver
  `sigill.md`, "La causa". verity sigue sirviendo para eso, para distinguir,
  pero no protege del fallo de verdad.
- **El `.odex`/`.art` que Android genera en `/data`** (tmpfs o overlay): no es
  la capa.

## Varios clones del mismo dorado a la vez

El agente de densidad vio corrupción SIN presión en el Mac (nivel 60–64) con
4 clones del mismo dorado a la vez (3 de 4 rotos a los 20 s: `libc++.so`
"`.dynamic section header was not found`", surfaceflinger con md5 malo,
"FIPS integrity test failed"). Hipótesis: los clones comparten los ficheros de
la capa y la base abiertos a la vez en vz. Se probó aquí (`multi.sh`, fuera
del repo): 4 `kling run -from` seguidos, `am start` + `uiautomator dump` en
cada uno, y a los 20 s y 60 s, en los 4, caché vs `O_DIRECT` de 1623–1829
ficheros (`system/{lib64,bin,framework,apex,fonts}`, `vendor`), system_server,
`logcat -b crash`, SIGILL y FIPS en dmesg, y otro dump.

| variante | dorado | repeticiones × clones | roto |
|---|---|---|---|
| (a) por defecto, misma capa y base | 1024 MiB, caché vaciada antes de guardar | 3 × 4 | 0 |
| (c) cada clon con su capa y base (clon APFS, ficheros distintos para vz) | 4 dorados de 1024 MiB | 3 × 4 | 0 |
| (d) dm-verity | 1024 MiB | 3 × 4 | 0 |
| (a) con la caché caliente en el dorado (sin `drop_caches`) | 1024 MiB | 1 × 4 | 0 |
| (a) | 768 MiB, caliente | 1 × 4 | 0 |
| (a) con la capa `android13slimov` del agente de densidad (`/data` en disco) y Termux instalado y abierto en el dorado, como su `clon.sh` | 768 MiB | 1 × 4 con el kling-vz de esta rama, 1 × 4 con el instalado (`~/.local/bin`) | 0 |
| (a) 4 clones a la vez leyendo 15 pasadas de 1,3 GB tras `drop_caches` (los mismos bloques de los mismos ficheros a la vez) | 1024 MiB | 1 × 4 (60 pasadas) | 0 |
| lo mismo con escrituras con `fsync` a la vez en el disco propio (vdb) | 1024 MiB | 1 × 4 (60 pasadas) | 0 |

**No se reprodujo** en 60 clones ni en 120 pasadas de lectura concurrentes.
Con el Mac a nivel 60–82 y la swap sin crecer. (b) `uncached` no se corrió en
esta forma: con (a) limpio no habría dicho nada. Lo que queda distinto de la
prueba del agente de densidad: el momento (el Mac a esa hora tenía 5–6 VMs
de otros agentes y el disco al 98–99 %), su kernel (el de la fase 0, sin
dm) y su orden exacto (instalar Termux en frío en los 4, `cuatro-frio.sh`).
Así que esto no descarta la hipótesis; solo dice que compartir el fichero no
basta por sí solo para romperlo.

## Reproducirlo

```sh
# en Lima (root): kernel con DM_VERITY y el parche, y la capa con verity
KERNEL_SHA256=5779f9ca... OUT=/var/tmp/android-kernel-verity prototypes/android/kernel/build.sh
sudo VERITY=1 DATA_MODE=tmpfs prototypes/android/image/build-image.sh
# o sobre imágenes ya hechas, dejando la original al lado (la capa es un enlace duro)
sudo prototypes/android/image/verity.sh IMAGES android13 android-base android13v android-basev

# en el Mac: que la capa va por verity, y contarlo bajo carga
kling exec <m> -- dmsetup status android-layer     # "... verity V" (C = hubo corrupción)
KLING_HOST=... prototypes/android/stress-restore.sh -from <dorado> -cycles 60 \
  -modes pause,reads,freeze,reads -read-passes 10 -hostspike 3 -keep-going
```

`stress-restore.sh` apunta en `ciclos.csv` (columna note) las líneas nuevas de
`device-mapper: verity` de cada ciclo y las guarda en `verity.txt`; `reads`
avisa de los ficheros que dan EIO (`READ_EIO`); `-keep-going` apunta una
lectura mala y sigue, para contar en N pasadas.
