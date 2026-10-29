# Carpetas compartidas

`kling run -share SRC:DST[:copy|ro|rw]` mete un directorio del host dentro de
una microVM. Hay dos formas de hacerlo y se eligen por el modo:

| modo   | qué ve el invitado                     | cambios del host | escribe el invitado | cliente remoto |
|--------|----------------------------------------|------------------|---------------------|----------------|
| `copy` | una copia de solo lectura (ext4)       | no               | no                  | sí             |
| `ro`   | el directorio vivo, en solo lectura    | sí (≤ 1 s)       | no (`EROFS`)        | no             |
| `rw`   | el directorio vivo, en escritura       | sí (≤ 1 s)       | sí                  | no             |

`copy` es el modo por defecto porque es el que no abre nada nuevo: el invitado
recibe un disco más, igual que un volumen de solo lectura. `ro` y `rw` son un
sistema de ficheros servido por el daemon, y por eso exigen que el operador haya
dicho qué directorios del host se pueden servir (`daemon.share_roots`).

Este documento es el diseño. Para usarlo basta con [su sección de la guía](guia.md#compartir-una-carpeta-del-host); la
API está en [api.md](api.md).

## Por qué así

El kernel del invitado (el vmlinux 6.1 de la CI de Firecracker, el mismo que usa
kling-vz en macOS) trae NFS y FUSE, y no trae virtio-fs ni 9p. Firecracker
tampoco tiene virtio-fs. Las opciones reales eran tres:

- **NFS**: descartado. Exige un servidor NFS del kernel del host expuesto a
  invitados que consideramos hostiles; es mucha superficie en ring 0 para
  compartir una carpeta.
- **virtio-fs**: no existe en Firecracker ni en el kernel del invitado.
- **Copia + FUSE**: lo que se hace. La copia no añade superficie (es un disco
  más). FUSE deja la superficie en código nuestro, en Go, en espacio de usuario,
  con el daemon validando cada petición.

## Modo copy

1. El CLI empaqueta el directorio local en un tar y lo sube al daemon
   (`POST /shares/uploads`). Da igual que el daemon sea local o esté al otro
   lado de SSH: el tar viaja por el mismo socket que el resto de la API, así que
   `kling -H ssh://lab run -share ./repo:/work` funciona sin nada más.
2. El daemon valida cada entrada del tar, la extrae en un directorio privado y
   construye con `mke2fs -t ext4 -d` un ext4 del tamaño justo (con holgura y sin
   journal), y borra el directorio. Devuelve un id.
3. `POST /machines` con `shares: [{mode: "copy", upload: <id>, mount: "/work"}]`
   mueve ese ext4 al directorio de la máquina y lo engancha como un disco de
   solo lectura DETRÁS de los volúmenes, con su punto de montaje en
   `kling.volume=…:ro`. El agente de invitado lo monta como monta un volumen
   de solo lectura, así que funciona incluso con agentes anteriores a esta
   versión.

Lo que el CLI mete en el tar: directorios, ficheros regulares (los enlaces duros
van como copias) y enlaces simbólicos relativos que no salen del directorio y
están en forma canónica (primero los `..`, luego lo que baja: `../../a/b`, nunca
`a/../b`). Se salta, avisando, lo demás: sockets, dispositivos, FIFOs, enlaces
que apuntan fuera y enlaces no canónicos. Los bits setuid/setgid/sticky se quitan.

Lo que el daemon ACEPTA, porque el cliente no es de fiar por serlo: solo
entradas de tipo directorio, fichero regular y enlace simbólico; ni rutas
absolutas, ni `..`, ni componentes de más de 255 bytes, ni rutas de más de 4096;
ni enlaces absolutos o que resuelvan fuera; ni entradas que cuelguen de un
enlace; ni nombres repetidos. Tamaño acotado (`daemon.share_copy_max_mib`, 1 GiB
por defecto) y número de entradas acotado (1 048 576). Cualquier violación
rechaza la subida entera con un 400: una copia a medias es peor que ninguna.

La copia trae además un `lost+found` vacío: lo crea `mke2fs` y no se puede
evitar. Un id de subida se consume al arrancar la máquina. Los que nadie usa se borran
a la hora; como mucho hay 8 pendientes a la vez.

## Modos ro y rw: FUSE sin dependencias

```
  invitado                                         host
  ┌─────────────────────────────┐                 ┌──────────────────────────────┐
  │ proceso ── VFS ── /dev/fuse │                 │ daemon                       │
  │                     │       │   TCP (el mismo │  internal/share.Server       │
  │ kling-guest (pkg/guest)     │   camino que    │   os.Root(SRC)               │
  │   protocolo FUSE del kernel │◀─ /exec) ──────▶│   una sesión por conexión    │
  │   tabla de nodos, handles   │  kling-share/1  │   handles acotados           │
  └─────────────────────────────┘                 └──────────────────────────────┘
```

- **En el invitado**, el agente (`pkg/guest`, Linux) abre `/dev/fuse`, monta
  un sistema de ficheros FUSE en DST y habla él mismo el protocolo del kernel:
  INIT, LOOKUP, FORGET/BATCH_FORGET, GETATTR, SETATTR, OPENDIR/READDIR/
  RELEASEDIR, OPEN/READ/WRITE/RELEASE/FLUSH/FSYNC, CREATE/MKDIR/UNLINK/RMDIR/
  RENAME(2), SYMLINK, READLINK, STATFS y ACCESS. LINK y MKNOD devuelven `EPERM`
  (ver más abajo); el resto, `ENOSYS`. No hay libfuse ni cgo.
- **El agente traduce** cada operación FUSE a una operación por RUTA
  (`stat a/b`, `open a/b`, `read handle off size`…) y la manda al daemon. La
  tabla de nodos (id de nodo → padre + nombre) vive en el invitado.
- **El daemon** sirve esas operaciones desde SRC con `os.Root` (Go 1.24,
  basado en `openat`): ni `..` ni un enlace simbólico sacan una operación del
  directorio.

### Quién abre la conexión

El daemon. El agente es un servidor (como en `/exec/pty`): el daemon hace
`POST /share/attach` con `Upgrade: kling-share/1` y un cuerpo con
`{tag, mount, mode}`; el agente contesta 101, monta si aún no lo había hecho, y
a partir de ahí esa conexión transporta tramas. Dentro, los papeles se invierten:
el agente pide y el daemon contesta.

Así la conexión no depende de la red del invitado —funciona con `egress none`,
como `exec`— y el agente no tiene que saber dónde está el daemon. El montaje se
hace al primer attach y no por la línea de comandos del kernel: el mismo agente
sirve para una máquina arrancada en frío y para una restaurada.

`/share` es una ruta de control: `guest.IsControlPath` la incluye para que el
gateway MCP no la reenvíe, y el agente rechaza el attach si viene del propio
invitado (loopback o su propia IP): un proceso sin privilegios de dentro no
puede hacerse pasar por el daemon y ver lo que se escribe en el montaje.

### Protocolo `kling-share/1`

Tramas `u32 longitud | cuerpo`, big-endian, con un máximo de 144 KiB.

- Petición: `u64 id | u8 op | argumentos`.
- Respuesta: `u64 id | u32 errno | resultado` (resultado solo si errno = 0).

`errno` es SIEMPRE el número de Linux, sea cual sea el sistema del host: el
daemon de macOS traduce sus errores a los de Linux antes de contestar. Las rutas
son relativas a SRC, ya limpias, sin componentes vacíos, `.` ni `..`, y el
daemon las revalida. Operaciones: STAT, FSTAT, LIST (paginado por índice), OPEN,
CREATE, READ, WRITE, RELEASE, FSYNC, SETATTR, MKDIR, UNLINK, RMDIR, RENAME,
READLINK, STATFS, HELLO, WATCH y SYMLINK. Los flags de apertura viajan en bits
propios del protocolo (los `O_*` de Linux y de macOS no coinciden).

Tras WATCH, el daemon es el único que manda tramas sin que se las pidan:
avisos con id 0 (`u64 0 | u32 0 | u8 evento | ruta`), con evento modify,
create o delete. Ver [Cambios hechos en el host](#cambios-hechos-en-el-host).

### Congelar, descongelar, reiniciar el daemon

La conexión muere al congelar: el VMM desaparece. Por eso:

- **freeze** cierra las conexiones de sus carpetas vivas antes de pausar, y con
  ellas los ficheros que el daemon tenía abiertos para el invitado. Lo que
  estuviera en vuelo recibe `EIO`; lo que llegue después espera.
- **thaw** vuelve a hacer attach en cuanto la máquina corre. El agente reconoce
  el montaje por su tag, cambia de sesión y sigue: el mismo punto de montaje,
  los mismos ids de nodo. Los ficheros que el invitado tenía abiertos se
  reabren solos por ruta la primera vez que se usan (el daemon contesta `ESTALE`
  a un handle que no conoce y el agente reabre y reintenta una vez).
- **reinicio del daemon**: igual. El nuevo daemon readopta la máquina y el
  vigilante hace attach de sus carpetas. El estado del lado del host es solo lo
  que se puede perder: ficheros abiertos, que se reabren.
- Mientras no hay sesión, una operación espera hasta 30 s a que vuelva y luego
  falla con `EIO`. Nunca se queda colgada.

**commit** de una máquina con carpetas compartidas —de cualquier modo— se
rechaza con 409: la memoria volcada llevaría un montaje que apunta a un
directorio de un host concreto, y un disco de copia que no viaja con el
snapshot. freeze/thaw de la misma máquina sí está soportado.

**El servicio espera a sus carpetas.** Con carpetas vivas, la línea del kernel
lleva `kling.shares=N` y el agente no lanza el servicio de la imagen hasta que
N carpetas tienen sesión (plazo: 30 s en marcha; al vencer, arranca igual y lo
dice en `kling logs`). Antes, `node /app/server.js` fallaba una vez porque
arrancaba antes de que el daemon conectara la carpeta. Un agente anterior
ignora el parámetro.

**Plantillas que esperan carpetas.** `run -share-wait N` arranca una máquina
cuyo agente espera N carpetas sin dárselas: su servicio no arranca, y
`kling save -force` la guarda así. `run -from` de esa plantilla acepta
exactamente N carpetas vivas (`-share ...:ro|rw`): se conectan tras restaurar y
el servicio arranca con ellas, con el código de ahora. Es lo que usa
`kling compose up` para los servicios con bind mounts. Una plantilla sin
`share_wait` rechaza carpetas (400): su servicio ya corre sin ellas. Las
copias (`mode copy`) siguen pidiendo arranque en frío.

### Por qué la tabla de nodos vive en el invitado

El daemon podría llevar él la tabla (id de nodo → ruta) y hablar FUSE con el
agente como mero relé. Se descartó por dos razones: la tabla se perdería al
reiniciar el daemon —y el kernel del invitado sigue nombrando esos ids—, y sería
estado del lado del host que un invitado hostil puede hacer crecer. Con rutas,
el daemon no guarda nada por nodo: solo ficheros abiertos, acotados y de la
sesión viva.

`os.Root` no tiene `Rename` ni `Readlink` hasta Go 1.25, y el núcleo se compila
con 1.24: esas dos van con `renameat`/`readlinkat` sobre el descriptor del padre
que da `os.Root`. En Linux, del paquete `syscall`; en macOS, que no las trae, con
sus números de llamada de XNU.

### Qué valida cada lado

El invitado es hostil, y el agente corre dentro: lo que cuenta es lo que valida
el daemon. El agente valida lo que le llega del kernel para no romperse él.

Daemon (`internal/share`):
- Rutas: relativas, limpias, sin NUL, componente ≤ 255 bytes, total ≤ 4096.
- Todo acceso por `os.Root`. Las operaciones sobre el último componente que no
  deben seguir enlaces (unlink, rmdir, rename, readlink) van con `*at` sobre un
  descriptor del directorio padre obtenido por `os.Root`; `open` exige tras
  abrir que lo abierto sea un fichero regular.
- Solo se sirven directorios, ficheros regulares y enlaces simbólicos. Los
  dispositivos, FIFOs y sockets del host no aparecen en los listados, `stat` los
  da por inexistentes y `open` los rechaza.
- Lecturas y escrituras de 128 KiB como máximo, o de 1 MiB si agente y daemon lo
  negocian al conectar (`OpHello`; uno viejo se queda en 128 KiB), con como mucho 8
  operaciones grandes en vuelo por sesión (antes 4: la lectura adelantada y las
  escrituras diferidas se esperaban entre sí); offsets no negativos y sin
  desbordamiento.
- Un enlace del host con destino absoluto dentro de la carpeta se lee como relativo;
  con destino fuera de ella, `readlink` da EACCES.
- Handles por sesión: 1024 como máximo, y 16384 en todo el daemon; más allá,
  `EMFILE`. Los ids de handle llevan la época de la sesión: uno de una sesión
  anterior nunca apunta a un fichero de la actual.
- En vuelo por sesión: 16 operaciones a la vez; la siguiente trama no se lee
  hasta que hay hueco (la contrapresión llega al invitado por TCP).
- `ro` rechaza con `EROFS` toda operación que modifique, en el daemon, aunque el
  invitado haya montado en escritura.
- Propietarios: el invitado ve todo como uid/gid 0. `chown` no hace nada. Los
  modos se recortan a `0777`: ni setuid, ni setgid, ni sticky. Si el daemon
  corre como root, lo que crea el invitado se entrega al dueño de SRC.
- SYMLINK solo en `rw` y solo si el enlace no puede sacar a nadie de la
  carpeta: destino relativo y canónico (ningún `..` tras un componente que
  baja: `x -> a/up/../..` y luego `a/up -> ..`, válidos por separado, sacaban
  a x dos niveles), que resuelto desde donde está el enlace se queda dentro (la
  regla de las copias, `share.CheckLink`), sin atravesar otro enlace
  (`share.LinkTraverses`) y en un directorio nombrado sin pasar por enlaces. Si
  no, `EPERM`. Comprobar y crear no compite con un `rename` o `rmdir`
  concurrente: esos dos toman el árbol en exclusiva y las operaciones que crean,
  compartido. El agente traduce a relativo
  un destino absoluto que cae dentro del propio montaje (`/app/x`); uno fuera
  (`/usr/bin/python3`) es `EPERM`. Un enlace válido podría dejar de serlo si se
  mueve más arriba (`a/b/l -> ../../x` movido a `l`): `rename` revalida el
  enlace movido (y da `EXDEV` si origen o destino se nombran a través de un
  enlace) y, si un directorio sube de nivel, recorre sus enlaces (hasta
  20 000 entradas); si alguno saldría, `EXDEV`, y `mv` cae a copiar y borrar,
  que vuelve a pasar cada enlace por SYMLINK.
- LINK se rechaza (`EPERM`): un enlace duro al fichero que sea es un arma
  contra lo que el host haga luego con ese directorio. MKNOD, igual. Los
  enlaces que ya hubiera se leen (`readlink`) pero el daemon no los sigue
  nunca: los resuelve el kernel del invitado, dentro de su propio árbol.

Agente (`pkg/guest`):
- Cada mensaje de `/dev/fuse` se comprueba contra su cabecera y el tamaño de su
  estructura; los nombres, NUL-terminados, sin `/`, ni `.`/`..`, ≤ 255 bytes; los
  ids de nodo y de handle tienen que existir.
- La tabla de nodos tiene tope (262 144). Si un kernel no manda FORGET y se
  llena, se expulsan los nodos hoja usados hace más tiempo; una operación sobre
  un nodo expulsado recibe `ESTALE` y el kernel vuelve a buscarlo por nombre.
- Handles del agente: 4096 por montaje. En vuelo hacia el daemon: 64.
- Un directorio abierto se lista entero al abrirlo (hasta 262 144 entradas) y
  READDIR se sirve de esa foto, como hace un NFS.

### Coherencia y límites

- Cachés del kernel del invitado de 1 s (atributos y entradas) y con
  `auto_inval_data`. Con un daemon que vigila (WATCH), un cambio hecho en el
  host llega en milisegundos y dispara inotify en el invitado (ver abajo); con
  uno anterior, se ve en ≤ 1 s y ningún vigilante se entera. La caché de
  páginas se invalida en cada `open`.
- Escritura diferida en el agente (`pkg/guest/fuse_wb.go`), con un daemon que
  la anuncia en `OpHello` (`CapWriteBehind`, v0.20): `write()` vuelve en cuanto
  el agente tiene los datos; los contiguos se juntan en trozos de hasta 1 MiB y
  salen en cuanto no hay otro en vuelo (como mucho 4 MiB pendientes por
  fichero; más allá, `write()` espera). Leer, pedir atributos, truncar, abrir,
  `fsync` y `close` esperan antes a que lo pendiente llegue al host, y un error
  del host en algo ya contestado lo devuelve el siguiente `write`, `fsync` o
  `close` de ese fichero, como en NFS. Si la máquina muere de golpe se pierde
  lo que no hubiera llegado (como con la caché de páginas de cualquier disco).
  Con un agente o un daemon anteriores, cada `write()` llega al host antes de
  volver, como antes. No se usa `FUSE_WRITEBACK_CACHE` del kernel: con él Linux
  (6.1) se queda con su tamaño y su mtime de cada fichero y deja de creer los
  del host, así que un cambio del host sin aviso (fuera del tope de vigilancia,
  o en el segundo de eco) no se vería nunca; y medido, la escritura secuencial
  bajaba a ~80 MB/s por el límite de páginas sucias que el kernel pone a FUSE.
- Lectura adelantada de 4 MiB (`/sys/class/bdi/<dev>/read_ahead_kb`, que el
  agente sube tras INIT: el kernel le daba 128 KiB), hasta 64 peticiones en
  segundo plano, y los destinos de los enlaces simbólicos en la caché del kernel
  (`FUSE_CACHE_SYMLINKS`): un aviso de WATCH los invalida, y si el host cambia
  un enlace sin aviso se ve al caducar su entrada (1 s). Dos máquinas escribiendo el mismo fichero: gana la última escritura de
  cada rango; no hay bloqueos entre máquinas (los `flock`/`fcntl` son locales a
  cada invitado).
- `rename` con `RENAME_NOREPLACE` comprueba y luego renombra: no es atómico
  frente a otro escritor del host.
- Enlaces simbólicos nuevos solo dentro de la carpeta (ver arriba): `npm
  install` funciona en una carpeta `rw`; `python -m venv` necesita `--copies`
  (sus enlaces apuntan a `/usr/bin/python3`, fuera).
- Caudal: en Linux la conexión pasa por la interfaz de red del invitado, que en
  Firecracker tiene un limitador de 16 MiB/s por sentido. Es el techo de lectura
  y escritura secuencial de una carpeta viva allí.

### Cambios hechos en el host

Un `node --watch`, nodemon, vite o webpack del invitado vigilan con inotify, y
inotify solo ve lo que pasa por el VFS del propio invitado: lo que se edita en
el host no lo despierta. Con WATCH:

1. **El daemon vigila lo que el invitado ha mirado** (`internal/share/watch.go`):
   cada ruta de un stat, list, open, create o mkdir, con kqueue en macOS (un
   descriptor `O_EVTONLY` por ruta) e inotify en Linux (por `/proc/self/fd`
   de un `O_PATH` abierto con `os.Root`, sin descriptores). Tope de 4096 rutas
   por sesión, expulsando la menos usada; en macOS, 4096 descriptores en todo
   el daemon. Lo que el invitado nunca miró no puede estar en su caché ni
   tener un vigilante: un `node_modules` sin abrir no cuesta nada.
2. Los cambios se agrupan 15 ms y se comparan: un directorio vigilado se vuelve
   a listar para saber qué nombre apareció o desapareció; un fichero sustituido
   por rename (el guardado atómico de un editor) es un cambio del mismo nombre.
   Lo que el propio invitado tocó en el último segundo es eco y no se avisa.
   Si el host pierde eventos (la cola de inotify se desborda, `IN_Q_OVERFLOW`)
   se repasa todo lo vigilado: cada directorio contra su foto y cada fichero
   contra su huella (tamaño, mtime, inodo), y se avisa de lo que difiera. Si no
   cabe una vigilancia más (los 4096 descriptores de macOS o
   `max_user_watches` en Linux) se dice una vez en el log: esa ruta se ve al
   caducar la caché de 1 s y sus vigilantes del invitado no se enteran.
3. **El agente lo lleva al kernel** (`pkg/guest/fuse_notify.go`):
   `FUSE_NOTIFY_INVAL_INODE`, `INVAL_ENTRY` o `DELETE`, y luego hace él la
   operación que dispara inotify, desde un hilo suyo: truncar al tamaño que
   ya tiene (IN_MODIFY), crear con `O_EXCL` o `mkdir` (IN_CREATE), `unlink` o
   `rmdir` (IN_DELETE). Las peticiones FUSE que eso provoca llevan el id de
   ese hilo en la cabecera y las contesta el agente sin ir al host: el host no
   cambia y no hay eco. Un aviso de FUSE solo no genera eventos de inotify.

En una carpeta `ro` se invalida pero inotify no dispara (el VFS no deja
truncar ni crear). Se descartó sondear desde el invitado (CPU y latencia aun
sin cambios, y tampoco dispara inotify) y emular inotify fuera del kernel.

Medido en un M4 (kling-vz), con un servidor de Node en la carpeta: editar en
el host y que `curl` devuelva lo nuevo tarda 254 ms con `node --watch` (Docker
Desktop: 257 ms); sin WATCH no recargaba nunca.

### Medidas

Con `dd` de 200 MiB (vaciando la caché del invitado antes de leer) y un script
que crea, consulta y borra 1000 ficheros de 1 KiB:

| | lectura sec. | escritura sec. | crear | `stat` | borrar |
|---|---|---|---|---|---|
| Linux, Firecracker anidado (laboratorio arm64 sobre un Mac M4) | 13–18 MB/s | 12–17 MB/s | ~210–245/s | ~1250/s | ~400–500/s |
| macOS, kling-vz (M4) | ~490 MB/s | ~190 MB/s | ~1230/s | ~10 200/s | ~4 000/s |
| disco de la propia máquina (referencia, macOS) | 1,9 GB/s | 880 MB/s | ~80 000/s | — | — |

En Linux el techo es el limitador de red de la microVM (16 MiB/s por sentido) y
la latencia de ida y vuelta de la virtualización anidada (~0,8 ms por
operación). `npm install express` dentro de la carpeta tardó 56 s frente a 45 s
en el disco de la máquina (la red domina); extraer el paquete npm entero (1284
ficheros) en la carpeta, 20 s en el laboratorio y 3 s en macOS.

## Configuración

```
kling config set daemon.share_roots /srv/src,/home/juan/code   # quién se puede servir en vivo
kling config set daemon.share_copy_max_mib 2048               # tope de una copia
```

- `daemon.share_roots` (o `KLING_SHARE_ROOTS`, separada por comas): directorios
  del host bajo los que puede estar SRC en los modos vivos. Vacío = ninguno, y
  `ro`/`rw` se rechazan diciendo cómo permitirlo. Se comparan rutas ya resueltas
  (sin enlaces), y nunca se sirve un directorio que contenga la raíz de datos de
  kindling. Lo lee el daemon en cada petición: no hace falta reiniciarlo.
- `daemon.share_copy_max_mib` (o `KLING_SHARE_COPY_MAX_MIB`): tope del contenido
  de una subida en modo `copy`.

En un Linux con systemd el daemon corre como root y lee la configuración de
root: `sudo kling config set …`, o `Environment=KLING_SHARE_ROOTS=…` en la
unidad.

## Capacidades

`shares-copy` y `shares-live` en `GET /info`. Un agente anterior contesta 404 a
`/share/attach`: la máquina no llega a arrancar y el error dice que hay que
reconstruir la imagen. Un kernel sin FUSE contesta 501. Una imagen sin agente
(`min`) no puede tener carpetas compartidas: nadie las montaría.

## macOS

Igual que en Linux. El daemon de macOS construye la copia con el `mke2fs` que
encuentre (el de Homebrew o el de android-platform-tools sirven; tiene que
aceptar `-d`) y sirve las carpetas vivas desde APFS: sin distinción de
mayúsculas por defecto, así que `A` y `a` son el mismo fichero para el
invitado. La conexión viaja por el reenvío de puertos de kling-vz, sin
limitador de caudal.
