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
| CPU | hasta 8 vCPU, un hilo cada una; PSCI por HVC (`CPU_ON`, `SYSTEM_OFF/RESET`) |
| GIC | GICv3 del framework (`hv_gic_create`, macOS 15+): el timer virtual y el WFI no salen a espacio de usuario |
| consola | UART 16550 en MMIO: `console=ttyS0` como en Firecracker, la línea de comandos pasa **intacta** (a diferencia de vz, que la traduce) |
| reloj | PL031 (`rtc-pl031`, el mismo que da Firecracker en arm64): un arranque en frío nace con la hora del host |
| discos | virtio-mmio block, uno por drive en el orden de las peticiones; cada disco tiene su hilo (el vCPU no espera a la E/S). Un FLUSH es un `F_FULLFSYNC` del fichero (el `fsync` de macOS deja los datos en la caché del SSD), como el `synchronizationMode full` de vz; salvo en el overlay `kling_disposable` (como el `none` de vz), que no lo hace. Cuesta: 300 `write`+`fsync` de 4 KiB en el invitado, p50 0,1 → 4,0 ms por `fsync` (52-68 ms → 1,3 s en total), lo mismo que vz (3,9-4,2 ms) |
| red | virtio-mmio net a un socket `SOCK_DGRAM` (una trama por datagrama): el contrato de `VZFileHandleNetworkDeviceAttachment`, así que `vnet` sirve tal cual. MTU 16384 en las máquinas que arrancan (`KLING_VZ_MTU`, como vz), anunciada con `VIRTIO_NET_F_MTU`; viaja en el snapshot |
| entropía | virtio-rng (`PUT /entropy`), con `arc4random_buf` |
| vsock | virtio-vsock (`vsock.c`): el canal con el agente, el mismo contrato que `kling-vz` (abajo) |
| globo | virtio-balloon sin cola de estadísticas: `squeeze`, `-mem-max` y `resize` (abajo) |
| sin | pantalla |

Todas las colas pasan por un único recorrido de cadenas con topes
(`vq_cadena`: índices fuera de la tabla, ciclos, descriptores indirectos —no
se ofrecen— y cadenas más largas que la cola son inválidas) y cada aviso
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
ruido. `go test -fuzz FuzzColas ./internal/hvvm` programa los cinco dispositivos con
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

## El fichero de memoria del snapshot

`PUT /snapshot/create` escribe en `mem_file_path` un único fichero:

```
[0, ram)     la RAM página a página, dispersa (lo no tocado son huecos)
[ram, ...)   estado: vCPU, dispositivos, estado del GIC
pie (32 B)   magia KLNGHV02, tamaño de la RAM, dónde está el estado
```

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

El snapshot escribe también las páginas residentes que el invitado solo leyó
(`MINCORE_INCORE`): el clon es otro vnode y no comparte la caché unificada con
su base, así que una página que no se escribe la tendría que leer del SSD el
invitado restaurado. Sin ello, el thaw de una máquina ya descongelada antes
tardaba 60-90 ms en el primer `/resync` (cada interrupción del invitado
esperaba a un fallo de página); con ello, 2-4 ms. Lo escrito queda en la
caché.

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
cuenta, en la vuelta lenta de un hilo vigilante (cada 2 s en reposo, 200 ms
arriba); bajan tras 400 ms por debajo de una décima de núcleo. Los hilos de
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
123,6 ms, con el thaw igual. Queda por probar, con el Mac tranquilo,
pedir esas páginas con `F_RDADVISE` en vez de escribirlas: en una ronda bajó a
75 ms con thaw de 52 ms, en otra no se distinguía del ruido.

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
- `sandbox_init` cuesta ~9 ms (4 de base y 5 de `system.sb`) y ~3 MiB por
  proceso; ver el fondo de procesos, arriba.
- El vigía de energía (`--power-events`) se registra y se prueba con
  sandbox, pero un sueño de verdad del Mac no lo prueba ningún test: mirarlo
  en `pmset -g log` y en el log del daemon tras cerrar la tapa.
