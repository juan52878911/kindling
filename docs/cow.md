# Discos copy-on-write para `run -from` (`daemon.cow`)

Crear una instancia desde un dorado (`kling run -from`, `sandbox fork`, el gateway
despertando un servicio) le da su propio overlay: el disco escribible del invitado,
con el mismo contenido que el del dorado. Hasta ahora eso era **copiarlo entero**
con `cp --sparse=always`. La memoria ya no escalaba con el tamaño del dorado (el
`mem.file` se mapea y se comparte); el disco sí.

Medido en el laboratorio (CT 105 de Proxmox, ext4): **76 ms por cada 200 MiB** de
overlay, y **~6,7 GiB escritos para 32 copias**, con la latencia creciendo ronda a
ronda a medida que el disco se llenaba de copias idénticas.

Con `daemon.cow` el overlay de la instancia es un **clon por reflink**: un fichero
propio que comparte los bloques del dorado hasta que alguno de los dos escribe. El
coste de crear la copia deja de depender del tamaño del disco, y cada instancia sigue
teniendo un disco independiente: lo que escribe una no lo ve ninguna otra ni el dorado.

## Configuración

```sh
kling config set daemon.cow auto            # auto (defecto) | reflink-store | off
kling config set daemon.cow_store_gib 32    # tamaño del almacén; 0 = automático
sudo systemctl restart kling                # se lee al arrancar el daemon
```

`KLING_COW` y `KLING_COW_STORE_GIB` mandan sobre el fichero.

| `daemon.cow` | Qué hace |
|---|---|
| `auto` | Si la raíz de datos clona (FICLONE funciona entre `snapshots/` y `machines/`), reflink nativo. Si no, el almacén propio, si el host puede tenerlo (root, `/dev/loop-control` y `mkfs.xfs`, o `mkfs.btrfs` donde el núcleo no tiene XFS). Si tampoco, la copia completa de siempre, con un aviso en el log y en `kling doctor`. |
| `reflink-store` | El almacén propio aunque la raíz clone. Si no se puede, copia completa (con aviso). |
| `off` | La copia completa de siempre. |

El modo en uso se ve en `kling info`:

```
disk clones:  store (reflink inside kindling's XFS store)  [daemon.cow=auto]; store /var/lib/kindling/cow (XFS): 15870 of 16384 MiB free; since start: store 32
```

Antes del primer save o `run -from` el almacén aún no existe, y no se da por hecho:

```
disk clones:  store pending (created on first use)  [daemon.cow=auto]; no reflink on the data root: overlays are reflinked inside kindling's copy-on-write store (xfs store, created on the first save or run -from)
```

Si el núcleo todavía no lista ese sistema de ficheros, el motivo lo añade (tendrá que
cargar el módulo al montar). Lo que se sabe al arrancar sin crear nada —falta de sitio
en la raíz para el almacén, ningún `mkfs`, no ser root— deja el modo en `copy` con el
motivo en la misma línea; lo que solo se descubre montando (el núcleo no tiene el
módulo, el contenedor no deja montar) lo dice el primer save o `run -from`, que pasa a
`copy` con el error.

y en `GET /info` (campo `cow`: `setting`, `mode`, `reason`, `pending` —el almacén está
por crear—, `store` —con `fs`, `xfs` o `btrfs`— y `clones`, que
cuenta desde el arranque cuántas instancias recibieron su overlay de cada forma).
`kling doctor` avisa si las instancias copian el disco entero sin que nadie haya
puesto `off`, o si hay un almacén que no está montado.

## Los tres modos

### reflink (nativo)

La raíz de kindling está en XFS con `reflink=1` (el defecto de `mkfs.xfs` desde
xfsprogs 5.1) o en Btrfs. `run -from` hace `ioctl(FICLONE)` del overlay del dorado al
de la instancia: nada que montar ni que configurar. En este modo también se clonan
el overlay de un `commit` y la plantilla de overlay de un arranque en frío. Si un
FICLONE concreto falla, esa copia se hace entera y se avisa.

La detección es una **prueba real** al arrancar el daemon (clonar un fichero de 4 KiB
de `snapshots/` a `machines/`), no una deducción del tipo de sistema de ficheros: un
XFS formateado sin reflink no clona, y un Btrfs sí.

### store (almacén propio)

La raíz no tiene reflink (ext4, el caso del laboratorio). kindling crea **la primera
vez que hace falta** (el primer `run -from` en ese modo) un fichero `$root/cow.xfs`, lo
formatea XFS con reflink y lo monta por loop en `$root/cow`. Si el núcleo no tiene XFS
pero sí Btrfs, el fichero es `$root/cow.btrfs` y se formatea Btrfs (ver
[Btrfs](#btrfs-en-lugar-de-xfs)); todo lo demás es igual:

```
/var/lib/kindling/
├── cow.xfs                      # el almacén: reservado entero con fallocate
├── cow/                         # su punto de montaje (nodev,nosuid,noexec)
│   ├── bases/<dorado>/<clave>.ext4   # UNA copia del overlay de cada dorado (0400, root)
│   ├── bases/<dorado>/<clave>.mem    # y UNA de su memoria (el espejo; ver abajo)
│   └── m/<id>/overlay.ext4          # el overlay de cada instancia: un clon de su base
│       m/<id>/mem.file              # su diff congelado (lo escrito desde el dorado)
│       m/<id>/mem.full              # base + diff mientras la copia corre tras un thaw
└── machines/<id>/overlay.ext4   # enlace simbólico ABSOLUTO a cow/m/<id>/overlay.ext4
    machines/<id>/mem.file, mem.full   # ídem
```

**La memoria también.** Una copia de un dorado se congela en diferencial (solo
las páginas que escribió; [imagenes.md](imagenes.md#imágenes-grandes-ram-cpu-y-disco))
y para despertarla el VMM necesita base + diff en un solo fichero. El almacén
guarda un espejo del `mem.file` de cada dorado (`.mem`, una copia por versión,
como la base del overlay), el VMM vuelca el diff de la copia directamente en
`m/<id>/mem.diff` (un fichero que el daemon deja creado, 0660 por grupo, bajo la
cuota de la instancia) y el daemon lo funde en `m/<id>/mem.file`. Despertar es
clonar el espejo a `m/<id>/mem.full` (FICLONE) y clonar encima los tramos con
datos del diff (FICLONERANGE): no se mueve ningún dato, solo metadatos, así que
cuesta lo que cuesten los tramos (medido: ~40 µs cada uno en Btrfs). `mem.full`
es del daemon y el VMM solo lo lee (0640): lo mapea MAP_PRIVATE y nunca escribe
en él. Se retira al congelar otra vez o al borrar la máquina; el espejo, cuando
el dorado desaparece o se reemplaza (`barrer`). Sin almacén, o si el overlay de
la copia no vive en él, el diff se queda en `machines/<id>` y despertar copia
la base en la raíz. El espejo se copia en segundo plano al guardar el dorado
(si el almacén aún no existe, ese save lo crea, como lo haría el primer
`run -from`) y solo si después queda libre al menos la mitad del almacén; si
no, lo copia el primer thaw que lo necesite.

`scripts/e2e-diff-freeze.sh` lo prueba de punta a punta en el host con KVM, con
daemons privados que levanta él mismo (almacén con jailer, `KLING_COW=off` con
jailer y sin él): un dorado de Postgres, una copia que pone a cero páginas
heredadas y tres vueltas de freeze→thaw comprobando filas, RAM, el sello
(`diff_base`), el tamaño del diff, el `schema` de `state.json` y que no quedan
restos. En el laboratorio: freeze 0,1–0,2 s, thaw 0,1–0,4 s, diff de 27 MiB
sobre 512.

- **Una copia completa por dorado, no por instancia.** La primera instancia de un
  dorado copia su overlay dentro del almacén (la "base"); todas las demás son clones de
  esa base. La base se identifica por dispositivo, inodo, tamaño y fecha del overlay
  del dorado: si el dorado se reemplaza (`commit -replace`), la siguiente instancia
  crea una base nueva y la vieja se borra (sus bloques siguen vivos en las instancias
  que se clonaron de ella; XFS y Btrfs cuentan las referencias).
- **La base se escribe a un temporal, se sincroniza y se renombra**: una base a medias
  no puede quedar con su nombre definitivo, porque de ella se clonarían instancias
  corruptas para siempre.
- **El resto del daemon no cambia**: `machines/<id>/overlay.ext4` es un enlace
  simbólico al overlay del almacén, y todo lo que abría esa ruta (Firecracker, commit,
  `kling cp`) la sigue abriendo.
- **El almacén se monta al arrancar el daemon si existe, sea cual sea el modo**: las
  instancias que tienen su overlay dentro lo necesitan para descongelarse. Poner
  `daemon.cow=off` después no las deja tiradas.
- **Lleno**: si quedan menos de 256 MiB libres dentro del almacén, la instancia nueva
  recibe una copia completa en la raíz, como antes. Pasado el 85 % de uso, `kling info`
  y `kling doctor` avisan con el comando para agrandarlo
  ([Hacer crecer el almacén](#hacer-crecer-el-almacén)).
- **Si no se puede crear o montar** (el núcleo no tiene el módulo, el contenedor no deja
  montar ese tipo, el sistema de ficheros no clona), la imagen recién creada **se
  desmonta y se borra** y se prueba con el otro tipo (Btrfs si era XFS, y al revés),
  si su `mkfs` está instalado. Si ninguno sirve, no queda ninguna imagen, el daemon
  vuelve a copiar hasta que se reinicie, y el motivo de cada tipo sale una vez en el log
  y en `kling info`.
- **Al arrancar**, un `cow.xfs`/`cow.btrfs` que ya existe y no monta se borra si
  **ninguna** máquina tiene su overlay en él (ningún `machines/<id>/overlay.ext4`
  apunta dentro de `cow/`): no guarda nada que no se pueda rehacer (las bases se
  vuelven a copiar) y el primer `run -from` lo crea de nuevo, con el tipo que ese núcleo
  pueda montar. Si alguna lo usa, se queda y `kling doctor` avisa: sus instancias no
  arrancarán hasta que se monte. Y solo si el fallo es definitivo: el núcleo no tiene
  ese sistema de ficheros o aquí no se deja montar (`EPERM`/`EACCES`). Con cualquier
  otro (un loop ocupado, un superbloque que no se lee, un tiempo agotado), que puede
  ser pasajero, se avisa en el log y la imagen se queda.
- **Limpieza**: `kling rm` borra el directorio de la instancia en el almacén. El
  vigilante barre lo que quede sin máquina (un `run -from` que falló, un directorio
  huérfano) y las bases de dorados que ya no existen.

### Btrfs en lugar de XFS

El núcleo de Proxmox (6.17) visto desde un contenedor LXC no tiene el módulo `xfs` y el
contenedor no puede cargarlo; `btrfs` sí está (medido en el laboratorio: un Btrfs por
loop clona 100 MiB con `cp --reflink=always` en 5 ms). El almacén elige su sistema de
ficheros así:

1. Si ya existe `$root/cow.xfs` o `$root/cow.btrfs`, ese: un almacén no cambia de tipo.
2. Si no, el primero que el núcleo lista en `/proc/filesystems` **y** cuyo `mkfs` está
   instalado, XFS antes que Btrfs.
3. Si el núcleo no lista ninguno de los dos (módulo sin cargar; se cargaría al montar),
   el primero cuyo `mkfs` esté instalado.
4. Si nada de eso, no hay almacén, y el motivo (qué instalar) sale en `kling info` y
   `kling doctor`.

Btrfs se formatea con `mkfs.btrfs -K -m single -d single -L kling-cow` y se monta con
`loop,nodev,nosuid,noexec,nodiscard`:

- **`-m single -d single`**: un solo dispositivo. DUP duplicaría los metadatos dentro
  del mismo fichero, sin proteger de nada que el disco de debajo no cubra. Sin modo
  mixto: el almacén mide al menos 1 GiB.
- **`-K` y `nodiscard`**: desde Linux 6.2 Btrfs monta con `discard=async` si el
  dispositivo lo admite, y un loop lo admite **agujereando el fichero de debajo**. El
  almacén dejaría de estar reservado y podría quedarse sin sitio debajo, que es justo lo
  que la reserva con `fallocate` evita. Lo mismo en el formateo.
- Todo lo demás es idéntico: reserva entera, temporal + rename durable, comprobación
  del montaje por ruta canónica y **tipo** (un Btrfs montado donde se espera el XFS, o
  al revés, no es el almacén), prueba de FICLONE real dentro del almacén antes de
  usarlo, y vuelta a la copia completa con un único aviso si algo falla.

Hay un test con un Btrfs de verdad (`TestAlmacenBtrfsDeVerdad`, root y
`KLING_TEST_MOUNTS=1`): crea el almacén, comprueba que sigue reservado entero y
montado sin discard, y clona dos instancias.

### Cuota por instancia

Cada overlay del almacén lleva una **cuota por instancia**, impuesta por el núcleo: el
tamaño lógico del overlay más una holgura (3 % y 16 MiB). El invitado no puede pasar de
su disco, pero el VMM escribe el fichero y un Firecracker comprometido podría hacerlo
crecer hasta llenar el almacén compartido; con la cuota recibe `EDQUOT`. El overlay es
`root:grupo-del-VMM` 0660 y no del VMM: el dueño de un fichero puede cambiarle el id de
proyecto de XFS (`FS_IOC_FSSETXATTR`) y con eso salirse de la cuota.

- **XFS**: cuota de proyecto. El almacén se monta con `prjquota`; cada overlay recibe un
  id de proyecto propio (ioctl sobre el fichero, no sobre el directorio: `PROJINHERIT`
  rompería el FICLONE desde la base con `EXDEV`) y un límite duro con `xfs_quota`.
- **Btrfs**: un subvolumen por instancia con un qgroup de límite referenciado
  (`btrfs quota enable` sobre el almacén, que es del daemon). FICLONE entre subvolúmenes
  funciona.

Ni XFS ni Btrfs descuentan lo compartido con la base al contar (XFS cuenta los bloques
enteros; el `rfer` de Btrfs también), así que el límite no puede ser menor que el tamaño
lógico: es una cota de crecimiento, no una reserva de lo que reescribe cada instancia.
Que muchas instancias reescriban su disco a la vez sigue pudiendo llenar el almacén
(dimensiónalo con `daemon.cow_store_gib`).

Si faltan `xfs_quota` o `btrfs`, o un almacén XFS ya montado no tiene `prjquota`
(una cuota solo se activa al montar: desmóntalo con el daemon parado y las microVMs
apagadas, y el daemon lo vuelve a montar con ella), el almacén funciona como antes, sin
cuota, con un aviso en el log y en `kling doctor`. Si la cuota está activa y no se puede
aplicar a una instancia, esa instancia va a copia completa en lugar de al almacén. Las
instancias creadas antes de la cuota siguen sin ella. Los tests con un almacén de verdad
(`TestCuotaXFSDeVerdad`, `TestCuotaBtrfsDeVerdad`) necesitan root y `KLING_TEST_MOUNTS=1`.

### copy

La copia completa y dispersa de siempre (`cp --sparse=always`; en macOS `cp -c`, que
en APFS ya es un `clonefile` y se informa como `clonefile`).

## El jailer

El jail de cada microVM recibe sus ficheros por **hardlink** (el snapshot, el
`mem.file`, las imágenes, el overlay): así se comparte la caché de páginas del
`mem.file`. Un hardlink no cruza de sistema de ficheros, y el almacén es otro.

Lo que entra al jail es el **enlace simbólico** de `machines/<id>` (Linux enlaza el
enlace, no su destino), que apunta a la ruta absoluta del overlay en el almacén. Para
que esa ruta resuelva dentro del chroot, el daemon hace un **bind del directorio
`cow/m/<id>` de esa instancia** en la misma ruta dentro de su jail, **antes de lanzar
jailer**: jailer entra en su propio espacio de montajes y hace un bind recursivo del
chroot sobre sí mismo antes del `pivot_root`, así que se lleva el bind consigo. Hacerlo
después dependería de la propagación de montajes del anfitrión.

- Solo el directorio de esa instancia: el almacén entero expondría los discos de todas
  a un VMM comprometido.
- Al borrar un jail (tras un `freeze`, al eliminar la máquina, antes de relanzarla) el
  bind se desmonta **antes** del `RemoveAll`, y si sigue montado el jail no se borra:
  `RemoveAll` no se para en los puntos de montaje y se llevaría el overlay por delante.
  Hay un test que lo comprueba con un montaje de verdad (`TestBindDelAlmacenEnElJail`,
  root y `KLING_TEST_MOUNTS=1`).
- Al arrancar, el daemon desmonta los binds que un daemon anterior dejara en jails de
  máquinas que ya no existen. Los de máquinas que siguen existiendo se quedan: su VMM
  puede estar vivo, y desmontar en el anfitrión se propagaría a su jail.

## Por qué esta opción (y no las otras)

Se evaluaron tres. El criterio: seguridad, luego sencillez, luego eficiencia; sin
cambiar el sistema de ficheros del host ni el formato de las imágenes; y que en hosts
con XFS o Btrfs no haga falta nada.

### (a) Almacén XFS (o Btrfs) con reflink en un fichero, montado por loop — **elegida**

- **Requisitos**: root (el daemon ya lo es), loop (el daemon ya monta imágenes por
  loop para construirlas y ampliarlas), `mkfs.xfs` (xfsprogs) y el módulo `xfs` del
  núcleo. Nada más: ni `dmsetup`, ni udev, ni metadatos propios.
- **Jailer**: resuelto con un bind por instancia (arriba). El VMM sin privilegios abre
  su overlay por grupo (0660, dueño root, grupo el del VMM) en un directorio que solo
  atraviesa (0750 root:grupo del VMM); la raíz del almacén y `m/` son 0750 root:grupo del
  VMM, como `machines/`.
- **Crash-safety**: XFS es transaccional (el reflink va al journal). Las bases se
  publican con fsync + rename. Tras un corte, lo que quede a medias (un clon sin
  máquina, un temporal) lo barre el vigilante. El `cow.xfs` también se crea en un
  temporal y se renombra.
- **Espacio**: el fichero se **reserva entero** con `fallocate` al crearlo. Un XFS
  sobre un fichero disperso que se queda sin sitio debajo recibe errores de E/S y **se
  apaga**, con todas las instancias dentro; con la reserva no hay sobreasignación
  posible: el almacén es una cuota fija. Tamaño por defecto: una cuarta parte del
  disco libre con un máximo de 16 GiB (`daemon.cow_store_gib` para fijarlo). El coste
  es que ese espacio cuenta como usado en la raíz desde el primer momento.
- **Limpieza**: borrar un clon libera solo lo que la instancia escribió; borrar la base
  no rompe a nadie (XFS cuenta referencias).
- **LXC privilegiado** (el laboratorio): los montajes por loop ya funcionan ahí (el
  daemon los usa); XFS necesita el módulo en el núcleo del anfitrión Proxmox, que lo
  trae, pero el contenedor no puede cargarlo si no está ya cargado: ahí el almacén es
  Btrfs (arriba). Si el perfil de AppArmor del contenedor no deja montar, el primer
  `run -from` lo descubre y el daemon vuelve a copiar, avisando.
- **XFS/Btrfs**: `auto` usa el reflink nativo y no crea nada.

### (b) device-mapper thin (dm-thin): un volumen fino por instancia

Un pool fino sobre dos ficheros por loop (datos y metadatos), el overlay del dorado
volcado a un volumen fino, y cada instancia como un snapshot fino (`create_snap`), que
Firecracker abriría como dispositivo de bloques.

- Más piezas: `dmsetup`, el módulo `dm-thin-pool`, nodos en `/dev/mapper` (en un LXC
  sin udev hay que crearlos a mano con `--noudevsync`), y **metadatos propios que
  reconstruir tras cada reinicio** (qué id fino es de qué instancia, activar el pool y
  cada volumen antes de poder descongelar nada).
- **Espacio**: un pool fino lleno pone en error **todas** las escrituras de todos los
  volúmenes, y recuperarlo es trabajo de administrador (`thin_check`, ampliar el pool).
  Es el mismo riesgo que el XFS disperso, sin la opción sencilla de reservar.
- **Jailer**: habría que crear un nodo de bloque dentro de cada jail (mknod), algo más
  que un bind.
- **Formato**: `commit` tendría que volcar el volumen a un fichero (el snapshot sigue
  siendo un `overlay.ext4`), y los discos dejarían de ser ficheros que se pueden copiar,
  inspeccionar con `debugfs` o mover con `kling images copy`.
- Ventaja real: granularidad de bloque sin sistema de ficheros intermedio. No compensa
  la complejidad ni el modo de fallo.

### (c) Seguir copiando

Sencillo y seguro, pero es el problema de partida: el coste escala con el tamaño del
dorado y con el número de copias. Se queda como `daemon.cow=off` y como red de
seguridad de los otros dos modos.

## Lo que no cambia

- El formato de imágenes y snapshots: un dorado sigue teniendo su `overlay.ext4` en
  `snapshots/<nombre>/`.
- Lo que ya existe no se migra: las instancias viejas siguen con su copia en
  `machines/<id>`.
- `commit` de una instancia del almacén copia su overlay entero al snapshot (el dorado
  vive en la raíz, fuera del almacén). Con reflink nativo, `commit` también clona.
- `commit` (y `fork` y `graph snapshot`, que pasan por él) lee el overlay de la
  instancia **por el descriptor que comprobó**, nunca volviendo a abrir la ruta: el
  overlay es del VMM, que podría cambiarlo por un enlace entre la comprobación y la
  copia. La copia es FICLONE entre descriptores o, si no, una copia dispersa en Go
  (`SEEK_DATA`/`SEEK_HOLE` y sin escribir los bloques a cero, como
  `cp --sparse=always`). En macOS es `fclonefileat(2)` desde ese mismo descriptor
  (por su número de llamada al sistema, sin cgo ni `x/sys`): el dorado comparte los
  bloques del overlay en APFS. `fclonefileat` crea el fichero y falla si ya existe
  algo con ese nombre (un enlace plantado incluido); después se abre relativo al mismo
  directorio y sin seguir enlaces, y se exige que sea un fichero regular con un solo
  enlace, de quien clona (`CLONE_NOOWNERCOPY`) y del tamaño del origen. Si no se puede
  clonar (`ENOTSUP` fuera de APFS, `EXDEV` entre volúmenes) se copia dispersa como en
  Linux.
- `DiskBytes` de `kling ps` no cuenta el overlay del almacén (sus bloques son
  compartidos: sumarlos por instancia mentiría). El uso real está en `kling info`.

## Hacer crecer el almacén

El almacén se crea con una cuarta parte del disco libre (máximo 16 GiB) o con
`daemon.cow_store_gib`, y no crece solo: la reserva entera es lo que lo protege de
quedarse sin sitio debajo. Para agrandarlo, en caliente y sin parar ninguna microVM:

```sh
kling cow                 # modo, uso del almacén y aviso si pasa del 85 %
kling cow grow +8G        # añade 8 GiB
kling cow grow 32G        # o fija el tamaño nuevo
```

El daemon (solo admin, `POST /cow/store/grow`) comprueba que el almacén está montado,
que no se pide encoger y que en la raíz quedan después los mismos 2 GiB de margen que
al crearlo. Luego:

1. Reserva el fichero hasta el tamaño nuevo con `fallocate` (sin sobreasignar, como al
   crearlo).
2. Busca el loop montado en `cow/` en `/proc/self/mountinfo` y comprueba en
   `/sys/block/loopN/loop/backing_file` que su fichero es el del almacén; le hace
   `LOOP_SET_CAPACITY` (lo que hace `losetup -c`) para que vea el tamaño nuevo.
3. Agranda el sistema de ficheros montado: `xfs_growfs cow/`, o
   `btrfs filesystem resize max cow/`.

Pedir el tamaño que ya tiene el fichero repite solo los pasos 2 y 3: así se completa un
crecimiento que se quedó a medias. No hay forma de encogerlo (XFS no encoge); para eso,
quitarlo (abajo) y dejar que se cree de nuevo con `daemon.cow_store_gib`. Solo Linux
(en macOS no hay almacén). La lógica (márgenes, no encoger, qué loop) tiene tests sin
root; el crecimiento de verdad sobre un loop, XFS y Btrfs, está en
`TestCrecerAlmacenDeVerdad` (root y `KLING_TEST_MOUNTS=1`, en el laboratorio).

## Almacén lleno

Un clon no ocupa nada al nacer y la cuota de cada instancia es una cota, no una reserva:
varias instancias que escriben a la vez pueden llenar el almacén aunque cada una cupiera
al crearla. Lleno, el VMM recibe `ENOSPC` del host y se lo da al invitado como un error
de E/S: ext4 aborta su journal y Postgres hace PANIC. Visto en el laboratorio con
`kling db fork -n 8`, que decía OK mientras las copias morían al escribir. Ahora:

- **Espacio asignable de verdad.** En Btrfs, `statfs` no basta: el espacio va por chunks
  de datos y de metadatos, y un almacén con cientos de MiB libres en sus chunks de datos
  falla igual si los metadatos se llenaron y no queda nada sin asignar. El daemon lo
  calcula con `BTRFS_IOC_SPACE_INFO` (`internal/machine/cow_asignable.go`): datos libres
  más lo sin asignar, 0 si los metadatos no tienen ni 16 MiB, y nunca más que `statfs`.
  En XFS, `statfs` es exacto. Es la cifra que enseñan `kling cow` y `kling info`.
- **Admisión.** Con menos de 256 MiB asignables no se clona en el almacén: la instancia
  nueva va a una copia completa en la raíz **solo si cabe entera** con los 2 GiB de margen
  de la raíz; si no, `run -from` (y `fork`, `kling db up`...) falla con
  `copy-on-write store full (N MiB free): kling cow grow +4G`. Lo mismo al descongelar o
  reanudar una instancia cuyo disco vive en el almacén.
- **El vigilante del almacén** mira lo asignable cada vez más a menudo cuanto menos queda
  (de 1 s a 10 ms: las escrituras van a la caché del host a velocidad de memoria). Por
  debajo de 128 MiB **pausa** a la vez todas las instancias en marcha del almacén (las que
  tienen carpetas vivas se congelan) y les pone `hold: copy-on-write store full`. Una base
  de datos que espera no pierde nada; una que vio EIO, quizá sí. En cuanto vuelve a haber
  256 MiB (`kling cow grow`, o al borrar otras instancias) las **reanuda solo**. `kling ps`,
  `kling cow` y `kling db ls` las marcan con `!` y lo explican.
- **Si aun así el invitado ve errores de disco** (un almacén que se llena más rápido de lo
  que se pausa, un disco del host que falla), el vigilante de máquinas los encuentra en
  la consola del invitado (`I/O error, dev vdb`, `EXT4-fs error`, `Aborting journal`) y
  los anota en la máquina (`disk_errors`, `disk_error_at`, `disk_error`): lo dicen
  `kling ps`, `kling db ls` y `kling db doctor` (DB055, CRITICAL). El texto es del
  invitado: solo avisa de su propia máquina.
- **Un almacén lleno al arrancar** se monta igual (antes el daemon lo daba por roto y
  copiaba hasta reiniciarse): sus instancias lo necesitan para despertar, y se libera solo
  en cuanto el limpiador de Btrfs termina de borrar los subvolúmenes de las borradas.

`scripts/e2e-cow-lleno.sh` lo prueba de punta a punta contra un daemon de pruebas con un
almacén pequeño (`KLING_COW_STORE_GIB=1`): cinco copias de Postgres escribiendo hasta
llenarlo, una rama congelada, un reinicio del daemon y `kling cow grow`. En el laboratorio
(Btrfs de 1 GiB, 5 copias escribiendo ~1,5 GiB): 0 errores de disco, 0 PANIC, la rama
rechazada al descongelar con el almacén lleno y con sus datos tras crecer. Antes del
vigilante, las cinco hacían PANIC. El pico medido fue de 336 MiB consumidos en 50 ms: por
eso la marca de pausa crece con la tasa de llenado.

El GC de disco (`gc.go`) ya no borra congeladas que no sean de un servicio: una copia de
`kling db` congelada al cambiar de rama tiene en su disco lo que escribió desde su dorado,
y el dorado no la "recrea" (se perdían ramas con el disco de la raíz al 88 %).

## Desmontar o quitar el almacén

Con el daemon parado y sin microVMs vivas:

```sh
sudo umount /var/lib/kindling/cow
sudo rm /var/lib/kindling/cow.xfs     # o cow.btrfs; borra los overlays de las instancias que lo usaban
```

Las instancias que tenían su overlay en el almacén no podrán arrancar después: bórralas
(`kling rm`) antes.

## Medir

`scripts/bench-cow.sh` (en el host, como root, junto al daemon) compara `cow=off` con
`cow=auto`: tiempo de cada `run -from` y disco usado para 1, 8 y 32 copias de un dorado
(`GOLD=...`, o `BUILD=1` para crear uno con ~200 MiB escritos en su overlay).

```sh
sudo BUILD=1 ./scripts/bench-cow.sh
sudo GOLD=mi-dorado MODES="off auto" COUNTS="1 8 32" ./scripts/bench-cow.sh
```
