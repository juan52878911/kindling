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

Este documento es el diseño. Para usarlo basta con la sección del README; la
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
van como copias) y enlaces simbólicos relativos que no salen del directorio. Se
salta, avisando, lo demás: sockets, dispositivos, FIFOs y enlaces que apuntan
fuera. Los bits setuid/setgid/sticky se quitan.

Lo que el daemon ACEPTA, porque el cliente no es de fiar por serlo: solo
entradas de tipo directorio, fichero regular y enlace simbólico; ni rutas
absolutas, ni `..`, ni componentes de más de 255 bytes, ni rutas de más de 4096;
ni enlaces absolutos o que resuelvan fuera; ni entradas que cuelguen de un
enlace; ni nombres repetidos. Tamaño acotado (`daemon.share_copy_max_mib`, 1 GiB
por defecto) y número de entradas acotado (1 048 576). Cualquier violación
rechaza la subida entera con un 400: una copia a medias es peor que ninguna.

Un id de subida se consume al arrancar la máquina. Los que nadie usa se borran
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
  RENAME(2), READLINK, STATFS y ACCESS. SYMLINK, LINK y MKNOD devuelven `EPERM`
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
READLINK, STATFS. Los flags de apertura viajan en bits propios del protocolo
(los `O_*` de Linux y de macOS no coinciden).

### Congelar, descongelar, reiniciar el daemon

La conexión muere al congelar: el VMM desaparece. Por eso:

- **freeze** cierra antes de pausar las conexiones de sus carpetas vivas (y las
  del host que tuvieran ficheros abiertos). Lo que estuviera en vuelo recibe
  `EIO`; lo que llegue después espera.
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
snapshot. Por lo mismo, `run -from` no acepta `shares` (400): las carpetas se
piden al arrancar en frío. freeze/thaw de la misma máquina sí está soportado.

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
- Lecturas y escrituras de 128 KiB como máximo; offsets no negativos y sin
  desbordamiento.
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
- SYMLINK y LINK se rechazan (`EPERM`): un enlace creado por el invitado en el
  host es un arma contra lo que el host haga luego con ese directorio (un
  proceso del host que siga el enlace escribiría donde el invitado quiso).
  MKNOD, igual. Los enlaces que ya hubiera se leen (`readlink`) pero el host no
  los sigue nunca: los resuelve el kernel del invitado, dentro de su propio
  árbol.

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
  `auto_inval_data`: un cambio hecho en el host se ve en ≤ 1 s. La caché de
  páginas se invalida en cada `open`.
- Escritura directa (sin write-back): cada `write()` llega al host antes de
  volver. Dos máquinas escribiendo el mismo fichero: gana la última escritura de
  cada rango; no hay bloqueos entre máquinas (los `flock`/`fcntl` son locales a
  cada invitado).
- `rename` con `RENAME_NOREPLACE` comprueba y luego renombra: no es atómico
  frente a otro escritor del host.
- Sin enlaces simbólicos nuevos: `npm install` (que crea `node_modules/.bin`)
  falla en una carpeta `rw`; `npm install --no-bin-links` funciona, o se
  instala en el disco de la máquina.
- Caudal: la conexión pasa por la interfaz de red del invitado, que en
  Firecracker tiene un limitador de 16 MiB/s por sentido. Es el techo de lectura
  y escritura secuencial de una carpeta viva.

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
