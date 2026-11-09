# Tercer backend para macOS: `kling-hv` (Hypervisor.framework)

`kling-hv` es un VMM propio sobre Hypervisor.framework que habla **exactamente
el contrato de `kling-vz`** ([backend-vz.md](backend-vz.md)): mismo API HTTP
(rutas de Firecracker más `/kling/*`), mismo servidor (`vz/internal/server`),
misma red de espacio de usuario (`vz/internal/vnet`), MMDS, egress, proxy de
credenciales y aristas de grafo. Lo único distinto es la VM
(`vz/internal/hvvm`, C con cgo) y, por eso, cuánto cuesta restaurar.

Sale de `prototypes/kling-hv` (medidas y decisiones en su README).

## Por qué

Virtualization.framework copia el estado entero a memoria al restaurar:
`kling-vz` paga ~320 MiB y ~200 ms por instancia restaurada. En `kling-hv` la
RAM del invitado es un fichero mapeado con `mmap(MAP_PRIVATE)` y pasado a
`hv_vm_map`: restaurar es mapear el fichero de memoria del snapshot, las
páginas entran cuando el invitado las toca y las limpias se comparten entre
todas las copias (caché unificada). Es la semántica del `mem_backend: File` de
Firecracker.

## Elegirlo

Es el de por defecto en macOS 15.2 o posterior: `daemon.vmm` vale `auto` si
no se fija, y el daemon elige `hv` al arrancar si `kling-hv` está, va firmado y
pasa su sonda (`kling-hv --probe`); si no, `vz`, y dice por qué
([`mac.md`](mac.md), «El backend es una opción de configuración»). Para fijarlo:

```
kling config set daemon.vmm hv        # o KLING_VMM=hv para el daemon
```

`hv` se valida como `vz`: solo macOS en Apple Silicon. El daemon busca
`kling-hv` junto a `kling` y luego en el `PATH` (`KLING_VMM=/ruta` lo fuerza).
La release lo trae (`kling-hv-darwin-arm64`, en `SHA256SUMS`) y `install.sh` y
`kling upgrade` lo dejan junto a `kling`. Desde el repo se compila con
`make vz` (o `vz/build-hv.sh`: cgo + firma ad-hoc con
`com.apple.security.hypervisor`; sin ella `hv_vm_create` da `HV_DENIED`) y
`make install` lo copia.

Los dorados no se comparten entre backends: el sello del snapshot lleva
`kling-hv x.y.z`, y uno de `kling-vz` sale como obsoleto (hay que rehacerlo).

## El hardware

| | |
|---|---|
| CPU | hasta 64 vCPU (lo que deje `hv_vm_get_max_vcpu_count`, 64 en el M4) y nunca más que núcleos del host; un hilo cada una; PSCI por HVC (`CPU_ON`, `SYSTEM_OFF/RESET`). El MPIDR va en grupos de 8 (Aff0 = i % 8, Aff1 = i / 8: el noveno es `cpu@100` en el FDT), así que hasta 8 vCPU es el de siempre. El kernel de kindling se compila con `CONFIG_NR_CPUS=32`: con más vCPU, el invitado ve 32 |
| GIC | GICv3 del framework (`hv_gic_create`, macOS 15+): el timer virtual y el WFI no salen a espacio de usuario |
| consola | UART 16550 en MMIO: `console=ttyS0` como en Firecracker, la línea de comandos pasa **intacta** (a diferencia de vz, que la traduce) |
| reloj | PL031 (`rtc-pl031`, el mismo que da Firecracker en arm64): un arranque en frío nace con la hora del host |
| discos | virtio-mmio block, uno por drive en el orden de las peticiones; cada disco tiene su hilo (el vCPU no espera a la E/S). Un FLUSH es un `F_FULLFSYNC` del fichero (el `fsync` de macOS deja los datos en la caché del SSD), como el `synchronizationMode full` de vz; salvo en el overlay `kling_disposable` (como el `none` de vz), que no lo hace, y en un volumen montado con otro modo (`kling_sync`, [abajo](#sincronización-de-los-discos)). Cuesta: 300 `write`+`fsync` de 4 KiB en el invitado, p50 0,1 → 4,0 ms por `fsync` (52-68 ms → 1,3 s en total), lo mismo que vz (3,9-4,2 ms). Los de escritura ofrecen `DISCARD` y `WRITE_ZEROES` ([abajo](#discard-y-write_zeroes)) |
| red | virtio-mmio net a un socket `SOCK_DGRAM` (una trama por datagrama): el contrato de `VZFileHandleNetworkDeviceAttachment`, así que `vnet` sirve tal cual. MTU 16384 en las máquinas que arrancan (`KLING_VZ_MTU`, como vz), anunciada con `VIRTIO_NET_F_MTU`; viaja en el snapshot |
| entropía | virtio-rng (`PUT /entropy`), con `arc4random_buf` |
| vsock | virtio-vsock (`vsock.c`): el canal con el agente, el mismo contrato que `kling-vz` (abajo) |
| carpetas | virtio-fs (`vfs.c`), un dispositivo por carpeta viva (`-share …:ro\|rw`): el kernel del invitado habla FUSE con el mismo servidor del agente, ahora en el proceso de `kling-hv` (abajo) |
| globo | virtio-balloon con cola de estadísticas (`VIRTIO_BALLOON_F_STATS_VQ`): `GET /balloon/statistics` da la memoria libre, disponible y total que ve el driver del invitado, así que `squeeze` reclama lo libre de verdad sin preguntar al agente; `-mem-max` y `resize` (abajo) |
| sin | pantalla |

Todas las colas pasan por un único recorrido de cadenas con topes
(`vq_cadena`: índices fuera de la tabla, ciclos, descriptores indirectos —no
se ofrecen, salvo en virtio-fs— y cadenas más largas que la cola son
inválidas; un anillo sin la alineación de virtio deja la cola sin atender) y
cada aviso
atiende como mucho una vuelta de la cola: un invitado que pusiera el anillo
`used` encima del `avail` dejaba un hilo de dispositivo girando para siempre, y
con él cualquier pausa o snapshot. Una petición de disco sin byte de estado o
fuera del disco se rechaza (antes rompía el proceso o hacía crecer el overlay).
La red y los discos ofrecen `VIRTIO_RING_F_EVENT_IDX` (y sin él se respeta
`VRING_AVAIL_F_NO_INTERRUPT`): el driver solo avisa y recibe interrupciones
cuando le hacen falta (`vq_interrumpir`, `vq_pedir_aviso` en `virtio.c`);
`KLING_HV_EVENT_IDX=0` no lo ofrece y `KLING_HV_AVISOS=1` escribe los contadores
en el registro cada 5 s. Medido (M4, n=10, dos rondas, 10×256 MiB por el
reenvío más 7 × [20.000 ficheros de 4 KiB + sync, 256 MiB + fsync]): los avisos
de rx de red pasan de ~30.000 a ~100, las interrupciones de red de ~198.000 a
~112.000 (−45 %) y los avisos de disco de ~6.300 a ~3.200 (−48 %); el caudal
(~2.300-2.600 / ~1.700-1.900 MiB/s) y los tiempos de disco quedan dentro del
ruido. `go test -fuzz FuzzColas ./internal/hvvm` programa los seis dispositivos con
datos al azar sobre una RAM de mentira (`prueba.c`); el fuzz de Go no ve la
cobertura del C, así que es aleatorio puro (8 M de ejecuciones en 90 s sin
fallos).

### vsock

Como en `kling-vz` (ver [backend-vz.md](backend-vz.md)): las máquinas que
arrancan llevan el dispositivo y `kling.vsock=1` (salvo `KLING_VZ_VSOCK=0`), el
reenvío del 8080 y `/kling/probe` prueban vsock primero y el snapshot dice si lo
lleva. Cada `DialVsock` es un `socketpair`: un extremo es el `net.Conn` de Go y
el otro lo bombea un hilo de C hacia la cola de rx del invitado, con el control
de flujo por crédito de virtio-vsock (256 KiB por conexión) y el medio cierre
(`CloseWrite`) como `SHUTDOWN`. Las conexiones no van en el snapshot: al
restaurar se manda `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET`. Comprobado: 20 MB de
`kling cp` íntegros y `kling exec` con `eth0` caído en el invitado.

### Globo

El invitado entrega marcos de 4 KiB y la página del host es de 16 KiB: una
página se suelta cuando sus cuatro marcos están en el globo. Soltar es volver a
mapear ese tramo del fichero de RAM (`mmap MAP_FIXED` + `hv_vm_unmap/map`):
`madvise` (`FREE`, `FREE_REUSABLE`, `DONTNEED`) **no** baja `phys_footprint`
de las páginas privadas de un mapeo `MAP_PRIVATE` de fichero (medido en macOS
26.5). Medido: 405 → 115 MiB al encoger con `resize` una máquina que había
escrito 300 MB, y `squeeze` devuelve ~242 MiB de otra; lo que se vuelve a
escribir tras desinflar es correcto. Las copias de un dorado comparten sus
páginas y el núcleo no las aprieta sin `-force`, como con Firecracker.

### Sandbox

`kling-hv` se encierra en `vz/cmd/kling-hv/kling-hv.sb` al crear o restaurar
la VM, con el mismo reparto que `kling-vz` (`vz/internal/sandbox`, que
comparten): escribe solo en el directorio de su máquina, lee los ficheros de su
VM y los dorados los publica el daemon. Sin `mach-lookup`, IOKit ni memoria
compartida: Hypervisor.framework no los necesita. Ningún perfil lleva reglas
por puerto: los reenvíos escuchan en cualquier puerto de 127.0.0.1, y el
loopback del Mac está cerrado siempre (el `-upstream 127.0.0.1:5432` de una
credencial lo marca el daemon por el broker), así que una máquina en allowlist
ya no compila 1000 reglas para cerrar el rango de los reenvíos. Lo que queda
de `sandbox_init` (~9 ms, `go test -bench SandboxInit ./internal/sandbox` en
vz/) es fijo: ~4 ms un perfil vacío y ~5 ms `system.sb`.

### Sincronización de los discos

Qué hace un `VIRTIO_BLK_T_FLUSH` del invitado (un `fsync`, un `sync`, el
commit del journal de ext4) lo decide cada drive con `kling_sync` en
`PUT`/`PATCH /drives` (`spec.Drive.Sync`, `KHV_SINC_*` en `khv.h`):

| modo | FLUSH | si el Mac se cae (corte de luz, pánico) |
|---|---|---|
| `full` (por defecto) | `F_FULLFSYNC`: espera a que el SSD vacíe su caché | lo que el invitado vio asentado está en disco |
| `barrier` | `F_BARRIERFSYNC`: lo de antes del FLUSH llega al SSD antes que lo de después, sin esperar a su caché | se pueden perder los últimos FLUSH, pero en orden: el journal de ext4 queda coherente y el sistema de ficheros monta |
| `none` | nada (el overlay `kling_disposable` va siempre así) | lo escrito puede no estar, y en cualquier orden: **el ext4 puede quedar roto** |

Donde `F_FULLFSYNC` o `F_BARRIERFSYNC` no se admiten (un volumen de red), un
`fsync`. Un cierre limpio del Mac no pierde nada en ningún modo: lo escrito
está en la caché del host y macOS lo lleva al disco; lo que se juega es un
corte. Los drives de solo lectura y la raíz van siempre en `full` (no hay
nada que relajar). El modo **va en el estado del snapshot**: un `thaw` no
reapunta los volúmenes y sigue como estaba; al restaurar un dorado, el núcleo
lo vuelve a mandar siempre (`full` si la réplica no pidió otro), así que una
réplica no hereda el del dorado. Un `kling-hv` anterior ignora el campo
(`full`).

Desde la CLI, solo para volúmenes de escritura: `-volume datos:/data:sync=barrier`
(ver `docs/guia.md`, «Varios volúmenes»). Con `kling-vz`, `barrier` es su
`synchronizationMode fsync` (vz no tiene `F_BARRIERFSYNC`: el `fsync` de macOS
no vacía la caché del SSD y no promete orden) y `none` el suyo. Con Firecracker
(Linux) el modo **se acepta y se ignora**: sus discos van con la caché por
defecto (`cache_type` sin poner, `Unsafe`), que no anuncia flush al invitado,
así que no hay nada que relajar; rechazarlo rompería un mismo script o compose
entre Mac y Linux, y el overlay desechable ya hace lo mismo (el campo solo
viaja a `kling-vz` y `kling-hv`).

Medido (M4 16 GiB, macOS 26.5, el Mac en uso por otros agentes, daemon
privado, un volumen de 256 MiB, 3 tandas de 300 `pwrite` de 4 KiB + `fsync`
en el invitado, ms por tanda):

| modo | 300 `fsync` |
|---|---|
| sin `sync=` (full) | 1997 / 1749 / 1756 |
| `full` | 1962 / 1621 / 1652 |
| `barrier` | 146 / 152 / 104 |
| `none` | 18 / 10 / 10 |

En el host, sin invitado (`TestFlushModos`: 300 × 4 KiB + FLUSH sobre el
fichero de prueba): 1,34 s / 153 ms / 7 ms. `barrier` es ~12× más rápido que
`full` y conserva el orden; `none` solo para volúmenes de desarrollo que se
pueden perder.

### DISCARD y WRITE_ZEROES

Los discos de escritura ofrecen `VIRTIO_BLK_F_DISCARD` y
`VIRTIO_BLK_F_WRITE_ZEROES` (los de solo lectura no, y rechazan las dos
peticiones con IOERR). Lo que el invitado borra vuelve al host: un `fstrim`
(o `mount -o discard`) hace huecos en el fichero del overlay o del volumen con
`fcntl(F_PUNCHHOLE)`. APFS solo los hace en bloques enteros de su tamaño
(`f_bsize`, 4 KiB), que es la `discard_sector_alignment` que se anuncia; los
bordes que no llenan un bloque se escriben con ceros (`pwrite`), así que
después todo el tramo se lee como ceros. `WRITE_ZEROES` con `unmap` hace lo
mismo; sin `unmap` (`REQ_NOUNMAP`: `fallocate -z` sobre el dispositivo) escribe
ceros de verdad y deja los bloques asignados. Si el sistema de ficheros del
host no sabe hacer huecos, `WRITE_ZEROES` escribe ceros y `DISCARD` no hace
nada (es un consejo).

Configuración: `max_discard_sectors` 2 GiB y `max_discard_seg` 32;
`max_write_zeroes_sectors` 32 MiB y `max_write_zeroes_seg` 1 (Linux no manda
más; ceros sin `unmap` ocupan el hilo del disco, que no deja pausar la VM
mientras dura); `write_zeroes_may_unmap` 1. Cada segmento
(`{sector, num_sectors, flags}`) se copia de la RAM del invitado y se valida
**antes** de tocar nada, como los sectores de un read/write: fuera del disco,
desbordes, más largo que el tope, más segmentos de los anunciados o un búfer
que no es múltiplo de 16 son IOERR; un flag desconocido, o `unmap` en un
`DISCARD`, UNSUPP (virtio 1.2, 5.2.6). Uno malo y no se toca ningún segmento.

El kernel del invitado (6.1) no necesita nada: `virtio_blk` lee las features y
ext4 hace `FITRIM`. Medido (mismo Mac, un volumen de 512 MiB, `st_blocks` del
fichero en el host):

| paso | en disco |
|---|---|
| volumen recién arrancado | 16 MiB |
| `dd` de 200 MiB al azar + `sync` | 216 MiB |
| `rm` + `sync` (sin `fstrim`) | 216 MiB |
| `fstrim -v /data` (487 MiB recortados) | 16 MiB |

Sobre el dispositivo crudo (volumen de 128 MiB desmontado en el invitado, 64
MiB al azar): `fallocate -z` de 16 MiB lee ceros y sigue en 64 MiB;
`fallocate -p` de 16 MiB (WRITE_ZEROES con `unmap`) y `blkdiscard` de 16 MiB
leen ceros y bajan a 48 y 32 MiB; lo de al lado sigue intacto. Un fichero
escrito antes de `fstrim` y `fallocate -z` conserva su md5 con la caché del
invitado vaciada y tras rearrancar la máquina.

### Carpetas vivas por virtio-fs

Con `kling-hv`, las carpetas `-share SRC:DST:ro|rw:virtiofs` de un arranque en
frío no pasan por el agente: el kernel del invitado las monta con `mount -t virtiofs` y
habla FUSE con el host por las colas de un dispositivo virtio-fs (device id 26;
la cola de alta prioridad, con los FORGET, y una de peticiones; tag
`kling-share-N`, con N el tag de la carpeta). El diseño de las carpetas está en
[compartir.md](compartir.md); lo que cambia es dónde corre cada pieza:

**Es opt-in por carpeta**, porque cambia velocidad por avisos: por virtio-fs
los metadatos van 3-5× más rápido y la E/S secuencial ~2× (tabla abajo),
pero se pierden los avisos de cambios del host (WATCH): un cambio hecho en el
Mac se ve al caducar la caché del invitado (1 s) y no despierta a inotify, así
que `node --watch`, nodemon o vite no recargan solos. Para un flujo de
desarrollo con vigilantes, FUSE (lo de siempre); para compilar, instalar
dependencias, `git` o leer mucho, virtio-fs.

- `-share SRC:DST:ro|rw:virtiofs` (API: `ShareSpec.transport: "virtiofs"`) la
  pide por virtio-fs; sin nada, FUSE.
- `KLING_HV_VIRTIOFS=1` en el entorno del daemon hace virtio-fs el defecto;
  entonces `:fuse` (`transport: "fuse"`) pide FUSE para una carpeta.
- `KLING_HV_VIRTIOFS=0` lo prohíbe: pedir `:virtiofs` es un error (400).
- En Linux (Firecracker) pedir `:virtiofs` es un error; en kling-vz la
  carpeta va por FUSE.

```
 invitado                  kling-hv (host)                                   daemon (host)
 proceso ─ VFS ─ virtiofs ─▶ vfs.c: colas ─▶ pkg/guest.HostFS ─kling-share/1─▶ internal/share
            (colas virtio)   (copia, valida)  (el fuseFS del agente)  (unix)     os.Root(SRC)
```

1. **Al crear la máquina** se decide por dónde va cada carpeta viva
   (`transporteCarpeta` en `internal/machine/plataforma_vz.go`) y se apunta en
   ella (`transport: virtiofs`). **Al arrancar**, el núcleo pide un
   dispositivo por cada una de esas (`PUT /kling/shares`, antes de
   `InstanceStart`; `carpetasAntesDeArrancar`). `kling-vz` contesta que no
   (501) y esas carpetas pasan a FUSE.
2. **El attach** pide al agente `Transport: virtiofs`: el agente monta el
   dispositivo de ese tag y contesta 200 sin tomar la conexión, y el núcleo
   conecta la sesión `kling-share/1` con `kling-hv` (`POST /kling/share/attach`
   con Upgrade, por el socket de su API), donde la recoge el servidor FUSE de
   ese dispositivo. La sesión, `os.Root`, `share_roots` y lo que valida el host
   son lo de siempre (`internal/share`, en el daemon): `kling-hv` no abre nunca
   SRC, así que su sandbox no cambia.
3. **El servidor FUSE** es el del agente (`fuseFS`, `pkg/guest`) tal cual, con
   su tabla de nodos, sus handles, la lectura adelantada y la escritura
   diferida; `pkg/guest.HostFS` lo envuelve para leer las peticiones de un
   dispositivo en vez de `/dev/fuse`. Sin WATCH: virtio-fs no tiene en Linux
   cola de avisos (`FUSE_NOTIFY_*`), y el truco de inotify del agente
   (operaciones sintéticas dentro del invitado) no se puede hacer desde fuera.

**El puente C↔Go** es cgo, sin hilo de C ni socket: una goroutine por
dispositivo llama a `khv_vfs_leer`, que espera en la cv del dispositivo a que
haya petición (la despierta el notify del vCPU), valida la cadena y copia su
parte de lectura al búfer de Go; `khv_vfs_responder` escribe la respuesta en la
parte de escritura. El C solo transporta y valida, como el resto de
dispositivos (`vfs_cadena`: lecturas antes que escrituras, todo dentro de la
RAM, sin ciclos, una tabla indirecta como mucho y solo como descriptor único
—se ofrece `VIRTIO_RING_F_INDIRECT_DESC`: una petición de 1 MiB son ~260
búferes y la cola tiene 256—, la cabecera FUSE entera y nada más grande que lo
negociado en INIT). Go no ve el `unique` del invitado: el C lo cambia por una
ficha (generación, cola, head) y lo repone al escribir la respuesta, así que dos
peticiones con el mismo `unique`, o una respuesta tardía a una cadena que ya no
espera (tras un reset), no escriben donde no deben. FORGET y BATCH_FORGET se
devuelven en seguida (no llevan respuesta) y los avisos de Go (`unique` 0) se
tiran. Las cadenas que contesta el propio C (inválidas) van de 512 en 512 por
despertar. Con la VM en pausa nadie toca la RAM: las dos llamadas esperan, y
pausar espera a las que estuvieran dentro, como con los discos.

**Congelar y descongelar.** El servidor FUSE vive en un proceso que muere al
congelar, y el kernel restaurado sigue nombrando sus nodos y sus handles:

- el estado del dispositivo (las colas y, por cola, qué heads tenía Go leídos
  sin contestar) va en una sección del `mem.file` detrás de la cabecera, que
  sube a la **versión 6** (ver la tabla de versiones en «El fichero de memoria
  del snapshot»); al restaurar, esas peticiones se le vuelven a dar a Go antes que las
  nuevas;
- lo que sabe el servidor FUSE (cada nodo con su padre y su nombre, los
  ficheros y directorios abiertos, y lo escrito por la escritura diferida que
  aún no llegó al host) va en el `snap.file`, en `kling_virtiofs[].state`, que
  sube `kling_vz` a **4**. Un fichero abierto se reabre por ruta en la sesión
  nueva, como en el agente.

Una máquina sin carpetas por virtio-fs sigue escribiendo la versión 4 (o la 5,
con más de 8 vCPU) y un `kling_vz` de 3 o menos: un `kling-hv` anterior (`kling upgrade -rollback`) la
lee. Lo que se repite al restaurar es lo que estaba en vuelo en el momento
exacto de la pausa (el núcleo corta antes las sesiones, así que casi siempre es
una operación que no llegó a salir): una escritura repetida es idempotente; un
`mkdir`, `unlink` o `rename` que sí llegó al host y se repite contesta
`EEXIST` o `ENOENT`, como el `EIO` que recibe hoy por FUSE lo que está en
vuelo. Medido en el M4: congelar a mitad de escribir 300 MB (con 63 MB
escritos) tarda 170 ms; tras descongelar el fichero llega entero (el mismo
sha256), el proceso que lo tenía abierto sigue escribiendo y el que tenía el
cwd dentro de la carpeta sigue allí.

**Cuándo no hay virtio-fs** (y todo va por FUSE, como antes, sin hacer nada):

- no se pidió (`:virtiofs`, o `KLING_HV_VIRTIOFS=1` en el daemon);
- un kernel sin `CONFIG_VIRTIO_FS` (los anteriores a esta versión): el
  `mount -t virtiofs` falla, el agente contesta 501 con
  `X-Kling-Share-Transport: fuse` y el núcleo repite el attach por FUSE;
- un agente anterior: no sabe de `Transport`, monta por FUSE y contesta 101;
- `kling-vz` y Firecracker: no tienen el dispositivo;
- `KLING_HV_VIRTIOFS=0` en el entorno del daemon (que pasa a `kling-hv`, que
  entonces tampoco crea dispositivos);
- las plantillas: `run -share-wait N` y `run -from` con carpetas no llevan
  dispositivo (la máquina de la plantilla se creó sin carpetas, y un
  dispositivo virtio-mmio no se añade en caliente), así que `kling compose up`
  sigue por FUSE.

`kling ps -json` dice por dónde va cada carpeta con sesión
(`"transport": "virtiofs"`; sin el campo, FUSE).

**El kernel.** `CONFIG_VIRTIO_FS=y` está en
`scripts/builders/kernel/config-arm64` (y `scripts/check-kernel-config.sh` lo
exige en arm64), sin `FUSE_DAX`. Un snapshot lleva su kernel dentro del
`mem.file` (al restaurar no se vuelve a leer el `vmlinux`), así que un kernel
nuevo no invalida plantillas ni congeladas: siguen despertando con el suyo, y
sus carpetas, por FUSE. Por eso ninguna clave tiene que cambiar con el kernel:
el dispositivo solo se crea en un arranque en frío, que siempre usa el
`vmlinux` de ahora, y si ese kernel no lo tiene, el agente lo dice y se cae a
FUSE. Una plantilla vieja sigue con su kernel hasta que se rehace;
`KernelSHA256` del snapshot dice con cuál se hizo.

**DAX** (la ventana de memoria compartida de virtio-fs, con la que el invitado
mapearía las páginas del host sin copiarlas) no está: pide una región de la
IPA mapeada bajo demanda a trozos de ficheros del host (`FUSE_SETUPMAPPING`),
con `hv_vm_map` por trozo y su invalidación al congelar. Queda en Pendiente.

Medido en el M4 (macOS 26, daemon privado, máquina de 512 MiB y 1 vCPU al
100 %, mediana de 3 vueltas; 2000 ficheros de 0,5-4 KiB en 20 directorios, uno
de 200 MiB con la caché del invitado vaciada antes de leer, `dd` de 200 MiB con
`conv=fsync`, `git status` de un clon de kindling de 2470 entradas, y crear,
mirar y borrar 1000 ficheros desde `sh`); en ms salvo los MB/s:

| | ro FUSE (kernel anterior) | ro FUSE (kernel nuevo) | ro virtio-fs | rw FUSE (kernel anterior) | rw FUSE (kernel nuevo) | rw virtio-fs |
|---|---|---|---|---|---|---|
| `find \| wc` en frío | 31 | 38 | 15 | 39 | 30 | 10 |
| `find \| wc` en caliente | 22 | 21 | 7 | 29 | 27 | 9 |
| `cat` de los 2000 | 1122 | 1239 | 548 | 1162 | 1037 | 543 |
| lectura secuencial (`dd`) | 667 MB/s | 859 MB/s | 1456 MB/s | 743 MB/s | 741 MB/s | 1446 MB/s |
| escritura secuencial (`dd`, a la carpeta rw) | 643 MB/s | 470 MB/s | 1184 MB/s | 504 MB/s | 517 MB/s | 1370 MB/s |
| `git status` en frío | 1397 | 1100 | 832 | 1259 | 1345 | 822 |
| `git status` en caliente | 1009 | 617 | 595 | 214 | 254 | 135 |
| crear 1000 | 1469 | 1510 | 312 | 1397 | 2009 | 356 |
| `stat` de 1000 | 230 | 232 | 55 | 260 | 241 | 56 |
| borrar 1000 | 527 | 510 | 139 | 543 | 606 | 140 |

«Kernel anterior» es el `vmlinux` instalado, sin virtio-fs; «kernel nuevo», el de
esta versión con `KLING_HV_VIRTIOFS=0`, para separar lo que es del kernel de lo
que es del transporte. Los metadatos van 3-5× más rápido (cada operación era un
viaje por vsock al agente y otro del agente al host; ahora es uno, por un socket
unix entre dos procesos del Mac) y la E/S secuencial ~2×. En `ro`, `git status`
en caliente mejora poco porque no puede escribir el índice y vuelve a mirarlo
todo. La sesión de virtio-fs lleva búferes de 2 MiB en su socket unix
(`fc.ShareSockBuf`, en los dos extremos): con los 8 KiB por defecto de macOS,
una escritura de 1 MiB eran 128 idas y vueltas entre los dos procesos, y la
lectura secuencial se quedaba en ~850 MB/s y la escritura en ~450.

## El fichero de memoria del snapshot

`PUT /snapshot/create` escribe en `mem_file_path` un único fichero:

```
[0, ram)     la RAM página a página, dispersa (lo no tocado son huecos)
[ram, ...)   estado: vCPU, dispositivos, estado del GIC
pie (32 B)   magia KLNGHV02, tamaño de la RAM, dónde está el estado
```

El estado lleva una versión (4 desde el free page reporting del globo; la 3
le dio su cola de estadísticas y la 2 lo añadía a él y a vsock al final): un `kling-hv` lee
las anteriores, que son un prefijo de la suya (la 2 convierte el globo), y
el daemon rechaza una del futuro con el mensaje de `versionHVMax`
(`internal/machine/meta.go`). La 5 es la 4 con el estado de los vCPU que no
caben en la cabecera de la 4 (del noveno en adelante) detrás, solo los que
tiene la máquina (`hdr_len` = 11 776 + 1 296 por vCPU de más). Una máquina de
hasta 8 vCPU se sigue guardando como 4, byte a byte igual que antes: la lee
un `kling-hv` anterior. Probado en el M4: un dorado de 8 vCPU congelado por
esta versión lo descongela el `kling-hv` de main (v4) y al revés; uno de 10
vCPU se guarda como 5 (`hdr_len` 14 368, GIC 123 KiB) y vuelve con `nproc` 10
y las 10 trabajando. Un invitado restaurado de una versión 2 no
negoció la cola de estadísticas: `squeeze` vuelve al agente hasta que
arranque de nuevo.

La 6 lleva detrás de la cabecera de la 4 (hasta 8 vCPU) o de la 5 (con más)
una sección con los dispositivos virtio-fs (`struct snap_vfs`: su número, 16
B, y 184 B por dispositivo con sus colas y las peticiones en vuelo), así que
se combina con los vCPU de más: `hdr_len` = la cabecera de la 4 o la 5 + 16 +
184 por dispositivo. Cada versión se escribe solo cuando hace falta, y lo que
no usa ninguna extensión sigue siendo la 4 byte a byte:

| versión | qué es | la escribe | `hdr_len` | la lee |
|---|---|---|---|---|
| 1 | sin vsock ni globo | nadie ya | 11 384 | todos |
| 2 | + vsock y globo | nadie ya | 11 696 | ≥ v0.19 |
| 3 | + cola de estadísticas del globo | nadie ya | 11 744 | ≥ 3 |
| 4 | + cola del informe de libres | hasta 8 vCPU, sin virtio-fs | 11 776 | ≥ v1.0 |
| 5 | 4 + los vCPU del noveno en adelante | más de 8 vCPU, sin virtio-fs | 11 776 + 1 296 por vCPU de más | ≥ 5 |
| 6 | 4 o 5 + sección de virtio-fs | con carpetas por virtio-fs | lo de la 4 o la 5 + 16 + 184 por dispositivo | ≥ 6 |

`khv_restore` comprueba primero que `hdr_len` tenga la forma de alguna
(`snap_forma`) y luego la exacta de su versión, sus vCPU y, en la 6, el
número de dispositivos que dice la sección; `versionHVMax` del daemon es 6.

Se hace clonando (APFS, `fclonefileat`) el fichero que respalda la RAM y
escribiendo encima las páginas que el invitado cambió según `mincore`
(`MODIFIED|COPIED|ANONYMOUS|PAGED_OUT`), más el estado. Un arranque en frío
respalda la RAM con un fichero disperso creado en el directorio de la máquina
y borrado en seguida (se clona por su fd). `snapshot_path` es el mismo JSON de
`kling-vz`.

`PUT /snapshot/load` mapea ese fichero; la VM queda en pausa hasta el
`PATCH /vm Resumed` (los `PATCH /drives` del núcleo llegan antes). El contador
virtual sigue donde se pausó: el tiempo congelado no existe para el invitado y
el reloj de pared lo arregla `/resync` del agente, como con vz.

Nadie escribe un fichero de memoria después de crearlo: lo exige el mapeo
`MAP_PRIVATE` (sus páginas limpias son las del fichero).

El snapshot pide también las páginas residentes que el invitado solo leyó
(`MINCORE_INCORE`): el clon es otro vnode y no comparte la caché unificada con
su base, así que una página que nadie pide la tendría que leer del SSD el
invitado restaurado. Sin ello, el thaw de una máquina ya descongelada antes
tardaba 60-90 ms en el primer `/resync` (cada interrupción del invitado
esperaba a un fallo de página); con ello, 2-4 ms. Antes se reescribían; ahora
se piden con `F_RDADVISE` a la caché del clon, en segundo plano y sin copiarlas
(`vm.c`): el freeze va en torno al doble de rápido y lo congelado ocupa menos
(76 → 60 MB en el M4).

## Pausa y techo de CPU

La VM corre con todos los bits de pausa a cero: el del núcleo (`PATCH /vm`) y
el del regulador de CPU (`Deps.Freeze`, que en vz es SIGSTOP al auxiliar de
Apple). Pausar saca a los vCPU a un punto seguro (`hv_vcpus_exit`) y espera a
que los hilos de disco y red no toquen la RAM. Bajo el regulador el reloj del
invitado sigue corriendo, como bajo `cpu.max`. `CPUTime` es la CPU de los hilos
de vCPU, con el tiempo en núcleos E cobrado a lo que vale en P
(`footprint.Contador`, la misma cuenta que `kling-vz`), leída por hilo y por
tipo de núcleo con `PROC_PIDTHREADCOUNTS`. Medido con techo 25 % y el proceso
en segundo plano (`taskpolicy -b`, todo en E) tras una pasada en P: el mismo
bucle pasa de 9,3 a 2,9 s (n=5). Sin más muestra en P que el arranque, la
proporción sale ~0,9 y apenas corrige (como en vz).

Pausar sube antes la QoS de los vCPU (como un exec): ociosos en `UTILITY` y con
el Mac cargado tardaban en llegar al punto de pausa. Medido con 4 vCPU ociosos
y pause/thaw seguidos (2026-10-28, carga ~17): sin el impulso, `khv_pause` p50
0,3 ms, p90 28 ms, p99 104 ms (n=656) en una tanda y p99 1,0 ms, máx. 14,7 ms
(n=200) en otra; con él, p99 0,19 ms, máx. 1,7 ms (n=200). Además, cada
milisegundo sin todos se vuelve a llamar a `hv_vcpus_exit` sobre los que
faltan, por si uno miró `pausa_pedida` justo antes de que se pusiera; en 600
pausas medidas no hizo falta ni una vez.

### QoS de los hilos

En macOS la QoS es la única palanca sobre el tipo de núcleo. Los vCPU van de
base en `QOS_CLASS_UTILITY` y suben a `USER_INITIATED` con un override mientras
tienen trabajo: en el acto cuando el host le manda algo al invitado (datos o una
conexión por vsock, tramas de red) y, si gastan más de medio núcleo por su
cuenta, cuando lo mide un hilo vigilante (cada 200 ms arriba; abajo no mira
el reloj: lo despiertan 50 salidas del timer virtual, 0,2 s de tics de un vCPU
ocupado, y solo cada 60 s como red de seguridad; hasta 2026-11 era una vuelta
fija de 2 s, medio despertar por segundo por VM ociosa); bajan tras 400 ms por
debajo de una décima de núcleo. Los hilos de
disco, red y vsock van en `UTILITY`. Nunca `BACKGROUND`: eso lo pone el modo
batería del daemon a todo el proceso, solo con la máquina ociosa.
`KLING_HV_QOS=0` lo apaga. Medido (M4 con carga ~10 de otros procesos, n=15):
`exec true` tras 1 s de reposo 25 → 27 ms y el bucle de CPU 622 → 629 ms
(ruido); 10 máquinas ociosas, energía de sus hilos (`PROC_PIDTHREADCOUNTS`)
2,4/3,1/1,7 mW con QoS frente a 2,9/3,0/0,6 sin ella (tres rondas): con el Mac
cargado no se ve ahorro, y el vigilante añade ~0,5 despertares/s por máquina.

## Lo que cambia en el núcleo con `hv`

- `kling up -check` mira `kling-hv`, su entitlement y macOS 15.2 cuando el
  backend es `hv` (fijado o elegido por `auto`); `make vz` compila los dos VMM.
- Si una creación, un arranque o una restauración fallan después de crear la
  VM de Hypervisor.framework (una por proceso, sin vuelta atrás), `kling-hv`
  sale con código 1 tras contestar, para que el núcleo lance uno nuevo; el
  servidor compartido con `kling-vz` habría intentado otra VM en el mismo
  proceso.
- `restaurarComparteMemoria` es verdad: las copias de un dorado son
  `MemShared` y la reserva de la segunda en adelante es la fracción divergente
  (`shareReserveDiv`), como en Firecracker.
- La compuerta de restauraciones deja pasar 8 a la vez (3 con vz).
- `kling run -image` restaura un snapshot por imagen en vez de arrancar en frío
  (con vz no: restaurar carga la RAM entera). Medido: de ~150 a ~45 ms hasta
  que contesta el agente ([snapshot-imagen.md](snapshot-imagen.md)).
- El congelado diferencial sigue siendo solo de Firecracker: el fichero de
  memoria de hv ya es un clon de su base más lo sucio.
- `GET /kling/stats` es el `phys_footprint` del propio `kling-hv` (no hay
  auxiliar).

## Medidas (MacBook Pro M4, 16 GiB, macOS 26.5, daemon privado, n=10)

Cada operación vista desde el API del daemon, p50 (el Mac con carga de otros
procesos: media de carga ~10-15). Los dos VMM confinados, con vsock.

| | hv | vz |
|---|---|---|
| run (frío) + primer exec | 225 ms | 527 ms |
| freeze | 73 ms | 122 ms |
| thaw + primer exec | 37 ms (30–55) | 171 ms |
| run -from + primer exec | 42 ms | 238 ms |
| sandbox fork -n 10 + exec en todas | 558 ms | no acabó (memoria) |
| exec en caliente (API) | 0,6 ms | 0,6 ms |
| memoria por copia de un dorado (10 / 50 copias) | 13,1 / 13,0 MiB | ~349 MiB |
| 50 copias | 651 MiB | — |

Banco consolidado del 2026-10-28, tras fusionar `hv-paridad`,
`hv-distribucion`, `compose-paridad`, `mac-huella` y `amd64-en-arm` (d65c33d2):
el mismo M4, daemon privado y solo, imagen `redis:7-alpine` importada, 256
MiB, egress none; desde el API del daemon, p50 / p90.

| | hv | vz |
|---|---|---|
| run (frío) + primer exec (n=12 / 10) | 210 / 223 ms | 502 / 539 ms |
| freeze | 115 / 130 ms | 168 / 205 ms |
| thaw + primer exec | 36 / 81 ms | 176 / 183 ms |
| run -from + primer exec | 40 / 46 ms | 218 / 226 ms |
| sandbox fork -n 10 + exec en todas | 521 / 601 ms | 2 587 / 5 282 ms (n=10; en otras 2 tandas, una copia no contestó: el vsock mudo de [backend-vz.md](backend-vz.md#el-canal-con-el-agente-vsock)) |
| exec en caliente (API, n=200) | 0,7 / 1,1 ms | 0,5 / 0,7 ms |
| memoria por copia de un dorado (`kling top`) | 16,1 MiB (10) · 15,7 MiB (50) | 365 MiB (3) · 318 MiB (10) |
| 50 copias | 784 MiB, arriba en 4,5 s | — |

Frente a la tabla de arriba, la memoria por copia de hv sube de 13 a 16 MiB:
es la MTU de 16 KiB (92a4d658). Con `KLING_VZ_MTU=1500` en el daemon, la
misma prueba da 14,0 MiB por copia (n=10): ~2 MiB por copia es el precio de
la red rápida. El freeze de hv (115 frente a 73 ms) también sube; ver
[Dónde se va el freeze](#dónde-se-va-el-freeze). La comprobación del canal vsock de c46f0f8b no se nota: en dos
vueltas alternas (n=12 cada una), thaw + primer exec 35-36 ms con ella y 46-113
ms sin ella (el ruido del Mac), run -from + primer exec 40-41 frente a 40-43 ms.

Freeze, thaw y memoria medidos de nuevo el 2026-10-28 (`kling top`, que
desde entonces suma los hijos de cada VMM), antes y después de quitar el
custodio y las reglas por puerto del perfil, en las mismas condiciones (carga
~10, máquinas con egress none, p50):

| | antes | después |
|---|---|---|
| hv: memoria por copia (10 copias, con su custodio) | 22,1 MiB | 13,1 MiB |
| hv: thaw + primer exec (n=12 / n=20) | 48,7 ms | 37,3 ms |
| vz: memoria por copia (3 copias, custodio y freno) | 356 MiB | 349 MiB |
| vz: thaw + primer exec (n=10) | 276 ms | 171 ms |
| `sandbox_init` kling-vz, none / allowlist (n=15-20) | 53 / 69 ms | 9 / 9 ms |
| `sandbox_init` kling-hv, none / allowlist | 10 / 29 ms | 9 / 9 ms |

La cifra de 17,4 MiB de la tabla de antes no contaba el custodio (~6 MiB por
máquina). El thaw de hv sigue repartido en el VMM anterior (~3 ms), lanzar el
proceso (~5 ms), cargar (de ellos ~9 de `sandbox_init`), `/resync` (2–4 ms) y
el API. El freeze escribe también las páginas residentes.

### Dónde se va el freeze

Trazado por fases en el daemon (2026-10-28, redis:7-alpine, 256 MiB, egress
none, n=12, el Mac con carga ~17 de otros agentes: los absolutos bailan ±40 %),
p50 desde que llega la petición:

| fase | ms |
|---|---|
| apretar el globo antes de volcar | ~10 |
| marca `volcado.en-curso` (una escritura durable) | ~6 |
| pausa + `khv_snapshot` (clon, `mincore`, escribir las páginas residentes, estado) | ~45 |
| matar el VMM, perforar, kernel | ~3 |
| llevar snap/mem/overlay al disco (barreras) | ~2 |
| sello (`durable.Escribir`: el F_FULLFSYNC que vacía la caché del SSD) | ~30-50 |

Lo que creció es el volcado y el sello, y los dos son el mismo coste: desde
f94233c6 se escriben también las páginas residentes que el invitado solo leyó
(~75 MiB en esta máquina), y el F_FULLFSYNC del sello tiene que llevarlas al
SSD. Sin ellas (probado con un interruptor de prueba) el volcado baja ~15 ms y
el sello ~10 ms, pero el thaw pasa de ~60 a ~170 ms: no compensa. La MTU de
16 KiB (`KLING_VZ_MTU=1500`) y `KLING_HV_EVENT_IDX=0` no mueven el freeze más
que el ruido. Antes de esta versión cada freeze hacía 7 F_FULLFSYNC (~5 ms cada
uno en el M4 frente a 0,7 ms de una barrera F_BARRIERFSYNC); ahora 2 (la marca
y el sello) y barreras en el resto. Medido en vueltas alternas (n=12 cada una,
p50 del API): hv 93,5 / 94,5 → 77,9 / 72,4 ms y vz 144 / 135,5 → 124,8 /
123,6 ms, con el thaw igual. Con `F_RDADVISE` (arriba), el p50 del freeze en
hv quedó en 67 ms.

### Trazas: dónde se va el arranque

`kling-hv` marca con `os_signpost` (subsistema `dev.kindling.hv`, categoría
`fases`; `vz/internal/hvvm/traza.c`) intervalos para cada fase: `vm+gic`,
`dispositivos`, `ram`, `kernel+fdt` y `vcpus` en un arranque en frío;
`leer-estado`, `ram`, `estado-dispositivos`, `vcpus`, `estado-gic` y
`estado-cpu` en una restauración; `pausa`, `snap-estado`, `snap-clon`,
`snap-paginas` y `snap-cabecera` en un freeze. Y eventos: `soltar` (la VM
corre), `primer-run` (el primer `hv_vcpu_run`), `primer-virtio` y
`primer-uart` (el kernel llega a sus drivers), `agente` (la primera conexión
vsock que acepta el invitado: el agente escucha) y `salida-cara` (una salida
de vCPU que tardó más de 50 µs en atenderse, con su EC y su dirección). Sin
nadie trazando no cuestan nada más que una marca de tiempo por fase.

Para verlos, con Xcode, Instruments (plantilla *os_signpost* o *Points of
Interest*, adjuntado a todos los procesos) o

```
xctrace record --template 'os_signpost' --all-processes --time-limit 10s
```

y, sin Xcode, `log stream --signpost --predicate 'subsystem == "dev.kindling.hv"'`.
Con `KLING_HV_TRACE=1` en el entorno del daemon (pasa a `kling-hv` como todo
`KLING_HV_*`) cada `kling-hv` escribe además en su log (el `firecracker.log` de
la máquina) una línea con el desglose cuando el agente responde, y otra tras
cada freeze:

```
kling-hv: trace boot ms: process 17.2 | vm+gic 0.06 dispositivos 0.14 ram 0.11 kernel+fdt 3.25 vcpus 0.12 wait 0.09 | ->primer-run 0.01 ->primer-uart 15.15 ->primer-virtio 14.86 ->agente 111.28 | total 115.10
kling-hv: trace freeze ms: pausa 0.04 snap-estado 2.40 snap-clon 0.16 snap-paginas 24.02 snap-cabecera 0.29
```

`process` es de que arranca el proceso a `khv_create` (Go, el sandbox y las
llamadas de configuración del daemon); `wait`, lo que pasa entre fases antes
de soltar la VM (Go y el API); los `->` cuentan desde que se suelta y `total`
va de `khv_create` al agente.

Medido el 2026-11-05 (M4, macOS 26.5, el Mac en uso: carga 2,5-5; daemon
privado, imagen `e2e-tools`, 256 MiB, egress none; con 10 vCPU, 512 MiB),
medianas en ms (n=10 con 1 vCPU, n=5 con 10). La primera columna «desde el
daemon» es `kling run` + primer `exec`, `freeze` y `thaw` + primer `exec`:

| arranque en frío | 1 vCPU | 10 vCPU |
|---|---|---|
| desde el daemon: run + primer exec | 165 | 207 |
| proceso → `khv_create` | 17,2 | 17,3 |
| `vm+gic` + `dispositivos` + `ram` | 0,3 | 0,3 |
| `kernel+fdt` (leer 18 MB de kernel en la RAM) | 3,3 (3-18) | 3,3 |
| `vcpus` (crearlos en orden) | 0,1 | 0,6 |
| soltar → primer acceso a virtio (kernel) | 14,9 | 30,1 |
| soltar → agente (kernel + init + agente) | 111 | 151 |
| `khv_create` → agente | 115 | 155 |

| restauración (thaw) | 1 vCPU | 10 vCPU |
|---|---|---|
| desde el daemon: thaw + primer exec | 47,5 | 50 |
| proceso → `khv_create` | 17,1 | 17,5 |
| `leer-estado` + `ram` (mapear el `mem.file`) | 0,35 | 0,46 |
| `vcpus` | 0,08 | 0,50 |
| `estado-gic` (`hv_gic_set_state` y reafirmar las líneas) | 1,52 | 1,53 |
| resume → agente | 1,0 | 0,9 |
| `khv_create` → agente | 3,2 | 3,6 |

| freeze | 1 vCPU | 10 vCPU |
|---|---|---|
| desde el daemon | 71 | 95 |
| `snap-estado` (vCPU, dispositivos, GIC) | 2,4 | 2,4 |
| `snap-paginas` (`mincore`, escribir lo sucio, `F_RDADVISE`) | 24 | 39 |

Lo que domina el arranque en frío es el invitado, no el VMM: de los ~115 ms
de `khv_create` al agente, `kling-hv` pone ~4 (la mayor parte, copiar el
kernel en la RAM) y el resto es el kernel y el init. Su `dmesg` (con
`initcall_debug`, solo para medirlo) da ~15 ms hasta los drivers, ~24 ms de
`ip_auto_config` (los `msleep` de la configuración de IP del kernel, `ip=`
en la línea de comandos; quitar `VIRTIO_NET_F_STATUS` no lo mueve: 30,8 ms), ~26 ms entre liberar la
memoria de init y lanzar `/sbin/overlay-init` (`mark_readonly` espera a un
periodo de gracia de RCU; con `rcupdate.rcu_expedited=1` el agente llegó a
los 85-108 ms en 5 arranques, frente a ~111, dentro del ruido del Mac) y
~30 ms de init hasta que el agente escucha. Nada de eso se recorta desde el
VMM sin tocar el kernel o su línea de comandos (que es del núcleo y la
comparte Firecracker), así que no se ha cambiado. En la restauración el VMM
pone ~3 ms (la mitad, el estado del GIC, que es del framework); lo que se ve
desde el daemon son el lanzamiento del proceso (~17 ms, ~9 de ellos
`sandbox_init`, ver arriba) y el `/resync` y el API.

## Estabilidad de `kling-hv`

Estable desde 1.0 ([`compatibilidad.md`](compatibilidad.md)), con una
condición mientras no haya un runner Apple Silicon en CI: cada release pasa a
mano `92-e2e-mac.sh` y `95-e2e-upgrade-mac.sh` con `VMM=hv`, y
`scripts/release.sh` no etiqueta sin eso ([`releases.md`](releases.md)).

Lo que dejó la auditoría de estabilidad de noviembre de 2026, en el M4.

### Probado

- **Ciclos largos**: un soak de 60 ciclos seguidos sin un fallo.
- **Invitado hostil**: abuso de los registros MMIO, de la red y del globo
  desde el invitado, sin cuelgues ni caídas del proceso; además del fuzz de
  `FuzzColas` (arriba), que el CI corre 30 s en cada commit.
- **Presión de memoria sin pérdida de páginas**: con el Mac apretado, el
  invitado no pierde páginas. La RAM es un
  `mmap(MAP_PRIVATE)` del `mem.file`, que es inmutable (nadie lo escribe tras
  crearlo): una página limpia que macOS suelta se vuelve a leer del fichero, y
  una sucia va al swap como cualquier página anónima. El #87 de vz (páginas
  del invitado que se pierden bajo presión cuando se regula la VM) no tiene
  por dónde darse aquí.
- **Reloj tras un thaw**: el contador virtual sigue donde se pausó y el
  `/resync` del agente pone la hora de pared: comprobado que el invitado
  descongelado tiene la hora del host.
- **Cabecera del volcado**: `go test ./internal/hvvm` (en `vz/`) cambia bits
  en cada byte de la cabecera y del pie y recorta el fichero en cada frontera
  de sección; `khv_restore` rechaza todo lo que no cuadra antes de mapear la
  RAM o devuelve un error, sin romper el proceso
  (`cabecera_corrupta_darwin_test.go`).
- **Los dispositivos con sanitizadores**: `vz/scripts/sanitizers-hv.sh` corre
  las pruebas de C sin VM (`prueba.c`, RAM de mentira, sin entitlement) con
  `-fsanitize=address,undefined`; el CI lo pasa en cada commit.
- **De punta a punta en el CI**: `.github/workflows/e2e-mac.yml` corre
  `scripts/92-e2e-mac.sh` y `scripts/95-e2e-upgrade-mac.sh` con `VMM=hv`
  cada noche y en los PR que tocan `vz/` o `internal/machine/`, en un runner
  self-hosted con Apple Silicon. Solo corre si la variable del repo
  `KLING_MAC_RUNNER` vale `true`: sin runner, el job sale saltado. En el job
  `vz` de `ci.yml` (un runner de GitHub, sin hipervisor) `kling-hv --probe`
  solo puede comprobar que la sonda contesta su fallo en vez de colgarse.

### Sin probar

- **Sueño y despertar del Mac** con máquinas vivas (ver «Pendiente»).
- **Disco lleno**: un snapshot o un overlay que no caben. El sello
  (`volcado.ok`) evita dar por buena una congelación a medias, pero no hay
  prueba de qué ve el invitado cuando su overlay no puede crecer.
- **vsock con carga**: muchas conexiones a la vez y transferencias largas en
  paralelo. Lo comprobado son 20 MB de `kling cp` y el agente con `eth0`
  caído.

### Límites

- **8 discos** por máquina (`KHV_MAX_DISKS` en `khv.h`; `khv_create` en
  `vm.c` y `hvvm_darwin.go` lo comprueban). El daemon no lo mira antes: una
  máquina con su raíz, sus capas, sus copias y sus volúmenes puede pasar de 8
  discos y falla tarde, al crear la VM, con `kling-hv supports at most 8
  drives`.
- **vCPU**: hasta `MAX_CPUS` (64) o lo que deje Hypervisor.framework, y no
  más que núcleos del host. Lo comprueban el daemon al pedir la máquina
  (`maxVCPUsPlataforma` en `internal/machine/plataforma_vz.go`: una vCPU por
  núcleo en macOS, también para vz), `kling-hv` en `PUT /machine-config`
  (`hvvm.MaxVCPUs`) y `khv_create`. Más vCPU que núcleos no corre más y deja
  al invitado esperando a hilos que el host no atiende a la vez.
- **Sin pantalla ni ventana**: una máquina con pantalla se rechaza
  (`kling-hv has no display device`). `KLING_VZ_GRAPHICS`, `KLING_VZ_WINDOW` y
  `KLING_VZ_DISK_CACHING`/`KLING_VZ_DISK_SYNC` pasan al entorno de `kling-hv`
  (el daemon deja pasar todo `KLING_VZ_*`, `entornoVMMPermitido` en
  `internal/machine/plataforma_vz.go`) y `kling-hv` los ignora.
- **`daemon.vmm auto` vuelve a vz sin mirar lo de hv**: la elección
  (`cmd/kling/vmm_auto.go`) protege lo que congeló vz (no pasa a hv si lo
  hay), pero no al revés. Si la sonda de `kling-hv` falla un día, o falta tras
  una instalación a medias, el daemon arranca con vz y las congeladas y
  dorados de hv salen `stale` (`causaObsoleto` en
  `internal/machine/meta.go`: «made with kling-hv …; this host runs
  kling-vz …») hasta que vuelva hv. No se pierden; `kling status -v` dice por
  qué se eligió vz. Para no depender de la sonda: `daemon.vmm hv`.
- **El estado del volcado no lleva suma**: `volcado.ok` sella el sha256 del
  `snap.file` (el JSON) y solo el **tamaño** del `mem.file`, y en hv el estado
  de vCPU, dispositivos y GIC va dentro del `mem.file`, tras la RAM. Una
  cabecera cambiada sin cambiar de tamaño solo la para la validación de
  `khv_restore` (magia, versión, tamaños, `ram_size`, `gic_size`), no una suma.
- **Bajada de versión solo hasta 8 vCPU y sin virtio-fs**: `kling-hv` escribe
  la versión más vieja que describe la máquina (la 4 hasta 8 vCPU, la 5 con
  más, la 6 con carpetas por virtio-fs), así que un `kling-hv` viejo lee lo que
  congela uno nuevo salvo esas dos (ver [compatibilidad.md](compatibilidad.md)); `kling upgrade -rollback`
  mira la versión de cada fichero antes de volver.

## Virtualización anidada (`kling run -nested`)

Para correr Firecracker (o cualquier KVM) dentro de una máquina del Mac, y con
ello los e2e de Linux sin salir del portátil. Es opt-in por máquina y solo de
kling-hv: Hypervisor.framework da EL2 a sus VM desde macOS 15 en un M3 o
posterior (`hv_vm_config_get_el2_supported`; en el M4 con macOS 26.5 da
`true`). Virtualization.framework no lo da a una VM Linux, así que kling-vz
rechaza la opción, y en Linux (Firecracker) el daemon también.

```sh
VARIANT=nested TARGET_ARCH=arm64 scripts/builders/kernel/build.sh   # en el lab
cp vmlinux-6.1.140-kindling-arm64-nested <raíz>/images/vmlinux-nested
kling run -name kvm -nested -allow-exec -mem 1G <imagen>
kling exec kvm -- ls -l /dev/kvm
```

**Lo que hace kling-hv** (`vm.c`, `fdt.c`), solo con `kling_nested` en
`PUT /machine-config` (la extensión que manda el núcleo; Firecracker no la
conoce y el núcleo no la pone si no se pide):

- crea la VM con `hv_vm_config_set_el2_enabled` y arranca cada vCPU (el 0 y
  los de `CPU_ON`) en **EL2h** (CPSR `0x3c9`). El kernel decide: con VHE se
  queda en EL2 (`CPU: All CPU(s) started at EL2`) y KVM corre ahí;
- en el FDT, **PSCI por SMC** (un HVC desde EL2 va al propio vector del
  invitado; el SMC lo atrapa HVF y `vm.c` ya lo atendía) y la **interrupción
  de mantenimiento** del GIC (PPI 9 = INTID 25, `HV_GIC_INT_MAINTENANCE`) en
  el nodo `intc`, que es lo que KVM busca para el vGIC (`vgic interrupt
  IRQ9`). El GIC de HVF da el interfaz de virtualización por registros
  (`ICH_*`, sin región GICV/GICH: KVM dice `no GICV resource entry` y usa
  GICv3 por sistema). El timer ya anunciaba el PPI del hipervisor (10 =
  `HV_GIC_INT_EL2_PHYSICAL_TIMER`), que es el que usa un kernel en EL2; HVF
  no da el timer virtual de EL2 (PPI 12), y sin él va bien;
- **registros de EL2 que HVF devuelve al VMM**: en el M4, `MSR CNTHCTL_EL2`
  sale como MSR atrapado (EC 0x18, ISS `0x313802`). Sin atenderlo el kernel
  se quedaba colgado en el arranque del timer; ahora `sysreg_el2` traduce el
  ISS (`khv_sysreg_iss`) y lo pasa a `hv_vcpu_{get,set}_sys_reg`, como QEMU.
  Lo que HVF no conoce sigue como antes (lectura a 0, escritura ignorada);
- un `CPU_ON` vuelve a poner también los EL2 de reinicio (SCTLR, HCR, CPTR,
  MDCR, TCR, TTBR0/1, MAIR, VBAR, CNTHCTL, CNTHP_CTL, VTTBR, VTCR del vCPU
  recién creado), como ya hacía con los de EL1;
- el WFI de respaldo (que con el GIC de HVF no se usa) se acota a 2 ms: no
  mira el timer de EL2.

**Sin snapshot, a propósito**. Una máquina anidada no se congela, ni se
guarda (`save`/commit), ni se bifurca: `khv_snapshot` y `khv_restore` lo
rechazan, el servidor también (`PUT /snapshot/create`: «snapshots of a
nested-virtualization VM are not supported») y el núcleo antes que ellos
(Freeze 409, commit 409, fork 409, con el porqué), como con `HasSecrets`; su
TTL de congelar se retira una vez, diciéndolo en el log. El estado de EL2 son
los registros de EL2, los `ICH_*` (con las list registers de un invitado de
segundo nivel en vuelo), el timer de EL2 y lo que HVF lleve por dentro de un
invitado anidado en marcha; nada de eso va en la cabecera v4, y no hay forma
de comprobar desde fuera que `hv_gic_state` guarde los `ICH_*`. Guardarlo
pediría una cabecera v5 y, sobre todo, una prueba de que un KVM con un
invitado vivo sobrevive al viaje; para el caso de uso (arrancar, correr los
e2e, tirar) no compensa. La cabecera v4 no cambia (`TestCabeceraKLNGHV`).

**Kernel aparte**. El de siempre no lleva KVM; la variante `nested`
(`scripts/builders/kernel/config-arm64-nested`: `VIRTUALIZATION`, `KVM`,
`TUN`) es otro fichero, `images/vmlinux-nested`, que el núcleo usa solo para
las anidadas (`kernelDe`, `internal/machine/anidada.go`). Así el sha256 de
`images/vmlinux`, que firma los dorados, no cambia y no se invalida ninguna
plantilla. Comprobado: el `.config` normal sale byte a byte igual con el
`build.sh` de antes y el de ahora (`CONFIG_ONLY=1`, `cmp`). Sin el kernel
anidado, `run -nested` es un 400 que dice cómo construirlo. `state.json` pasa
al esquema 4 mientras haya alguna máquina anidada (un kling ≤ 1.0 perdería el
campo y la arrancaría sin EL2); sin ellas sigue escribiendo el de antes.

**Probado** (M4, macOS 26.5.1, kling-hv directo por su API, 2 vCPU, 512 MiB):
el kernel anidado arranca en EL2 con `kvm [1]: Hyp mode initialized
successfully`, el invitado ve `/dev/kvm` y **una microVM de Firecracker
v1.17.0 aarch64 arranca dentro** (1 vCPU, 128 MiB, el mismo kernel, el disco
raíz del invitado como su disco) hasta su init y sale limpia: de lanzar
Firecracker a que termina, 2,8 s de reloj del invitado, que incluyen la
consola serie con earlycon a dos niveles y el segundo de `panic=1`. El kernel
de siempre también arranca en EL2 (sin KVM). Pausa y reanudación funcionan;
el snapshot se niega.

Coste del arranque (ms de `InstanceStart` a que `init=/bin/echo` escribe,
`quiet`, n=7, mediana y rango, en serie):

| kernel | sin EL2 | con EL2 |
|---|---|---|
| `vmlinux` de siempre (6.1.177) | 136 (125-194) | 161 (141-251) |
| `vmlinux-nested` (6.1.140 + KVM) | 112 (101-175) | 131 (126-190) |

EL2 cuesta unos 20-25 ms por arranque en frío; las máquinas sin `-nested` no
cambian de camino (misma VM, mismo CPSR, mismo FDT).

**Lo que falta**:

- los e2e de Linux dentro: la anidada solo tiene `TUN` de más. Para un daemon
  de kindling dentro harían falta además puente, netfilter y lo que pida
  `scripts/check-kernel-config.sh` para un anfitrión, y no está probado;
- el invitado de segundo nivel corre en EL1: no se puede anidar otra vez;
- PMU: con EL2, HVF manda a `ID_AA64DFR0_EL1.PMUVer`; no se ha mirado qué ve
  KVM ahí ni si `perf` funciona en el invitado;
- SMP del invitado de segundo nivel y carga sostenida dentro: solo probada
  una microVM de 1 vCPU que arranca y sale.

## Fondo de procesos pre-lanzados (propuesta, sin hacer)

La idea: tener unos `kling-hv` ya arrancados esperando un `snapshot/load`
para que el thaw no pague el proceso. Medido (n=15, p50): del exec a tener el
API escuchando son 5,2 ms. Es lo único que se ahorraría sin abrir el perfil:
un proceso no se puede encerrar hasta saber de qué máquina es (su directorio,
su dorado, su overlay), y un sandbox no se puede apilar ni ampliar después
(`sandbox_apply` sobre un proceso ya encerrado da EPERM, medido con
`sandbox-exec` anidado). Así que el fondo esperaría SIN confinar y se
encerraría al recibir la máquina: los ~9 ms de `sandbox_init` se quedan, y
también la carga y el `/resync`. De ~35-40 ms se bajaría a ~30, no a 15-20.

Lo que pediría además: que el daemon dé a un proceso del fondo su socket del
API y su entorno (hoy van en la línea de órdenes y en el entorno del exec, y
el vigilante reconoce a cada VMM por su `--api-sock`: procesos_vz.go), y que
un proceso del fondo sin confinar no sea alcanzable por nadie más que el
daemon. Para bajar de verdad haría falta compilar el perfil fuera del camino
del thaw (`sandbox_compile_string` + `sandbox_apply` son privados y piden cgo
en quien compila; el núcleo no lo tiene) o un perfil sin `system.sb` (~5 de
los ~9 ms), que es otra auditoría.

## Pendiente

- Pantalla.
- virtio-fs: DAX (ver Carpetas vivas por virtio-fs); los avisos de cambios del
  host (WATCH) y su inotify en el invitado, que por virtio-fs se pierden (un
  cambio del host se ve al caducar la caché de 1 s, y `node --watch` no se
  entera: por eso virtio-fs es opt-in por carpeta); y las plantillas con
  `-share-wait`, que siguen por FUSE porque su máquina se crea sin
  dispositivo.
- `sandbox_init` cuesta ~9 ms (4 de base y 5 de `system.sb`) y ~3 MiB por
  proceso; ver el fondo de procesos, arriba.
- El vigía de energía (`--power-events`) se registra y se prueba con
  sandbox, pero un sueño de verdad del Mac no lo prueba ningún test: mirarlo
  en `pmset -g log` y en el log del daemon tras cerrar la tapa.
