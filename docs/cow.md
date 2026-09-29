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
| `auto` | Si la raíz de datos clona (FICLONE funciona entre `snapshots/` y `machines/`), reflink nativo. Si no, el almacén propio, si el host puede tenerlo (root, `/dev/loop-control`, `mkfs.xfs`). Si tampoco, la copia completa de siempre, con un aviso en el log y en `kling doctor`. |
| `reflink-store` | El almacén propio aunque la raíz clone. Si no se puede, copia completa (con aviso). |
| `off` | La copia completa de siempre. |

El modo en uso se ve en `kling info`:

```
disk clones:  store (reflink inside kindling's XFS store)  [daemon.cow=auto]; store /var/lib/kindling/cow: 15870 of 16384 MiB free; since start: store 32
```

y en `GET /info` (campo `cow`: `setting`, `mode`, `reason`, `store` y `clones`, que
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
formatea XFS con reflink y lo monta por loop en `$root/cow`:

```
/var/lib/kindling/
├── cow.xfs                      # el almacén: reservado entero con fallocate
├── cow/                         # su punto de montaje (nodev,nosuid,noexec)
│   ├── bases/<dorado>/<clave>.ext4   # UNA copia del overlay de cada dorado (0400, root)
│   └── m/<id>/overlay.ext4          # el overlay de cada instancia: un clon de su base
└── machines/<id>/overlay.ext4   # enlace simbólico ABSOLUTO a cow/m/<id>/overlay.ext4
```

- **Una copia completa por dorado, no por instancia.** La primera instancia de un
  dorado copia su overlay dentro del almacén (la "base"); todas las demás son clones de
  esa base. La base se identifica por dispositivo, inodo, tamaño y fecha del overlay
  del dorado: si el dorado se reemplaza (`commit -replace`), la siguiente instancia
  crea una base nueva y la vieja se borra (sus bloques siguen vivos en las instancias
  que se clonaron de ella; XFS cuenta las referencias).
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
  recibe una copia completa en la raíz, como antes. Si el almacén no se puede crear o
  montar (no hay módulo `xfs`, el contenedor no deja montar), el daemon vuelve a copiar
  hasta que se reinicie, y lo dice una vez en el log.
- **Limpieza**: `kling rm` borra el directorio de la instancia en el almacén. El
  vigilante barre lo que quede sin máquina (un `run -from` que falló, un directorio
  huérfano) y las bases de dorados que ya no existen.

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

### (a) Almacén XFS con reflink en un fichero, montado por loop — **elegida**

- **Requisitos**: root (el daemon ya lo es), loop (el daemon ya monta imágenes por
  loop para construirlas y ampliarlas), `mkfs.xfs` (xfsprogs) y el módulo `xfs` del
  núcleo. Nada más: ni `dmsetup`, ni udev, ni metadatos propios.
- **Jailer**: resuelto con un bind por instancia (arriba). El VMM sin privilegios abre
  un fichero suyo (0600, dueño el usuario del VMM) en un directorio suyo; la raíz del
  almacén y `m/` son 0750 root:grupo del VMM, como `machines/`.
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
  trae. Si el perfil de AppArmor del contenedor no deja montar XFS, el primer
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
- `DiskBytes` de `kling ps` no cuenta el overlay del almacén (sus bloques son
  compartidos: sumarlos por instancia mentiría). El uso real está en `kling info`.

## Desmontar o quitar el almacén

Con el daemon parado y sin microVMs vivas:

```sh
sudo umount /var/lib/kindling/cow
sudo rm /var/lib/kindling/cow.xfs     # borra los overlays de las instancias que lo usaban
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
