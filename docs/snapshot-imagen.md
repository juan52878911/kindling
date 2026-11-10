# Snapshots por imagen: `kling run -image` sin arranque en frío

`kling run -image X` restaura, cuando puede, un snapshot de `X` en vez de
arrancar el kernel. La primera `run` de una imagen con unos parámetros arranca
en frío como siempre; mientras tanto, el daemon guarda en segundo plano un
snapshot de esa imagen, y desde la siguiente `run` con los mismos parámetros se
restaura. No hay que pedir nada: es una caché del daemon.

```
$ kling run -image mi-base -e FOO=uno -allow-exec
3f0c9a1e2b7d  mi-base-3f0c9a  booted cold in 28 ms
$ kling run -image mi-base -e FOO=dos -allow-exec
8a71c0d4e5f2  mi-base-8a71c0  started from its image snapshot in 15 ms
```

## Cifras (kling-hv, M4)

Medido el 2026-11-05 con un daemon privado de kling-hv, una imagen Alpine
pequeña (la `min` con el agente actual y un servicio de shell), 128 MiB y
1 vCPU, una máquina a la vez (dos tandas):

| | `kling run` vuelve | hasta que contesta un `exec` |
|---|---|---|
| en frío (`-fresh`), p50 de 5 | 35-39 ms | 152-155 ms |
| desde el snapshot, p50 de 10 | 37-40 ms | 45-51 ms |

En frío, `run` vuelve en cuanto el VMM arranca, y el invitado tarda otros
~120 ms en tener el agente escuchando. Desde el snapshot, `run` vuelve con el
agente ya resincronizado y el servicio lanzado: lo que se espera de verdad baja
de ~150 a ~45 ms. La restauración por dentro (`thaw_ms`) son 15 ms: ~13 ms
lanzar kling-hv, ~11 ms reanudar y abrir los reenvíos, 2 ms el resync y 1 ms
la entrega. La primera restauración tras guardarlo cuesta más (107-114 ms,
casi todo en el resync: el invitado lee de disco las páginas que toca).

Cada copia viva ocupa 9-11 MB de `phys_footprint` (`footprint -p`): el resto de
su memoria es la del volcado, compartida por todas. Guardarlo cuesta ~2 s en
segundo plano (arrancar la máquina de preparación, dejar que su agente se
asiente y volcarla en ~50 ms) y ~48 MiB de disco.

## Qué se guarda y qué no

El snapshot sale de una máquina **aparte**, la de preparación
(`kling-imgprep-…` en `kling ps` durante esos dos segundos), que no es de
nadie:

- La misma imagen, kernel, vCPU, memoria (y `-mem-max`), disco, caudal de red y
  `-allow-exec` que la `run` que lo provocó: es lo que queda fijado en la
  memoria y en la línea de comandos del kernel.
- Sin nada de esa `run`: ni su nombre, ni sus etiquetas, ni `-e`, ni
  `-entrypoint`/`-cmd`, ni volúmenes, ni carpetas, ni secretos, ni red hacia
  fuera (`-egress none`).
- Con `kling.imgsnap=1` en la línea del kernel. El agente de invitado arranca,
  escucha y **no lanza el servicio de la imagen** ni lee MMDS: espera. Lo dice
  en su `/healthz` (`late-start`) y el daemon comprueba además que no hay
  ningún otro proceso vivo (`GET /procs`) antes de volcarla.

Cada copia, al restaurarse, hace lo mismo que una copia de un dorado (disco
CoW propio, red propia, `POST /resync`: reloj, CSPRNG del kernel y
`machine-id` nuevos) y después lo que haría un arranque en frío: el daemon le
manda su entorno y su comando (`POST /machine/start`, una sola vez por copia) y
el agente lanza el servicio. Por eso los procesos del servicio nacen después del
resync y siembran sus aleatorios del kernel de esa copia: el problema de los
dorados con el servicio vivo (los mismos `Math.random`, `random.random()` o
claves de OpenSSL en todas las copias) no se da aquí. En la medida de arriba,
10 de 10 copias dieron distintos `/proc/sys/kernel/random/uuid` y distinta
semilla del servicio (16 bytes de `/dev/urandom` leídos al arrancar), y cada
una vio su `-e`.

Para todo lo demás la máquina es una arrancada en frío: `from` vacío en
`kling ps`/`inspect` (`image_snapshot` dice de dónde salió), `-e` vale, su disco
es la única copia de lo que escriba, `kling start` la arranca en frío sobre él,
y un `kling save` de ella es un dorado normal. Los ganchos de
`/etc/kindling/post-restore.d` no corren: el servicio acaba de nacer, no se ha
restaurado.

## La clave

El nombre del snapshot (`kling-img-<hash>`) es un hash de: la imagen y la
identidad de sus ficheros (inodo, tamaño y fecha: reimportarla con el mismo
nombre es otra clave), el sha256 del kernel, el VMM y su versión, la versión de
kling, macOS y la arquitectura, vCPU, memoria y techo, disco, caudal de red,
`-allow-exec` y la pila IPv6 de la receta. Cambiar cualquiera de ellos da otro
snapshot; el viejo deja de usarse y el barrido lo borra en cuanto nadie lo usa.
`-e`, el nombre, las etiquetas, `-egress`, `-ttl`, `-p` o `-cpu-pct` no cambian
la clave: llegan después.

## Cuándo no

- `-fresh`: arranca en frío y no guarda nada (para medir, o si se sospecha del
  snapshot). `KLING_IMAGE_SNAPSHOTS=0` en el entorno del daemon los apaga del
  todo.
- Con `-volume`, `-share` (de copia o viva), `-share-wait` o `volume_wait`: los
  discos y lo que el agente espera desde la línea del kernel se deciden al
  arrancar, y a una máquina restaurada no se le añaden.
- Con kling-vz: restaurar carga la RAM entera y no sale más barato que arrancar
  una imagen pequeña. Valen kling-hv y Firecracker (Linux), que mapean el
  volcado.
- Imágenes cuyo agente no sabe esperar: uno anterior a esta versión, el puente
  de kindling-mcp, un `/entrypoint` propio (pudo lanzar cosas antes de ceder el
  PID 1), el init de la imagen como PID 1 (`init: true`) o una imagen sin
  agente. El daemon lo ve en la máquina de preparación (o antes de arrancarla,
  sin agente), la borra, lo anota en el log y esa imagen sigue arrancando en
  frío; no lo vuelve a intentar mientras la imagen no cambie. Reconstruir la
  imagen con un `kling-guest` actual lo arregla.
- Si una restauración falla (el volcado no carga, el agente no se resincroniza o
  no acepta la entrega), esa `run` arranca en frío, el snapshot se tira y no se
  prepara otro en 10 minutos.

## Dónde viven y cuánto ocupan

En `snapshots/` de la raíz del daemon, como los dorados, con un fichero
`image-of` (la clave, y su fecha es el último uso) que los aparta de
`kling template ls`. Se ven en:

```
$ kling image snapshots
IMAGE     VCPUS   MEM       DISK           ON DISK   LAST USED          IN USE   NOTE
mi-base   1       128 MiB   512 MiB exec   48M       2026-11-05 09:45   2        —
$ kling image ls          # una línea al final: "Image snapshots: 1, 48M"
$ kling status            # "image snaps:  1 · 48M of 1.0G"
```

- Tope de disco: `KLING_IMAGE_SNAPSHOT_MIB` (1024 en macOS, 4096 en Linux).
  Antes de guardar uno y en cada barrido se borran, del menos usado al más,
  los que hagan falta para caber; nunca uno con copias vivas.
- Si el disco pasa de la marca alta del GC, son lo primero que se borra.
- `kling image rm X` se lleva los de `X` (y se niega mientras alguna copia viva
  lo use, como se niega por la propia máquina).
- `kling image snapshots prune` borra todos los que nadie usa.
- `run -from kling-img-…` se rechaza: no es una plantilla.

API: `GET`/`DELETE /image-snapshots`, `fresh` en `POST /machines`,
`image_snapshot` en la máquina e `image_snapshots` en `GET /info` y en cada
imagen de `GET /images` (capacidad `image-snapshots`, [api.md](api.md)).

## Lo que queda

- Firecracker usa el mismo camino (es `runFrom`), pero no se ha medido ni
  probado de punta a punta en Linux.
- Las carpetas vivas (`-share … :ro|rw`) se podrían entregar tras restaurar
  como el entorno; hoy esas `run` arrancan en frío.
- El propio agente (Go) es el único proceso que va en el volcado: su estado de
  `math/rand` y sus semillas de mapas se repiten entre copias. No usa
  `math/rand`, y lo que entrega a los procesos de la máquina viene del kernel
  ya resincronizado.
