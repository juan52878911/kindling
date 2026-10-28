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

```
kling config set daemon.vmm hv        # o KLING_VMM=hv para el daemon
```

`hv` se valida como `vz`: solo macOS en Apple Silicon. El daemon busca
`kling-hv` junto a `kling` y luego en el `PATH` (`KLING_VMM=/ruta` lo fuerza).
Se compila con `vz/build-hv.sh` (cgo + firma ad-hoc con
`com.apple.security.hypervisor`; sin ella `hv_vm_create` da `HV_DENIED`).

Los dorados no se comparten entre backends: el sello del snapshot lleva
`kling-hv x.y.z`, y uno de `kling-vz` sale como obsoleto (hay que rehacerlo).

## El hardware

| | |
|---|---|
| CPU | hasta 8 vCPU, un hilo cada una; PSCI por HVC (`CPU_ON`, `SYSTEM_OFF/RESET`) |
| GIC | GICv3 del framework (`hv_gic_create`, macOS 15+): el timer virtual y el WFI no salen a espacio de usuario |
| consola | UART 16550 en MMIO: `console=ttyS0` como en Firecracker, la línea de comandos pasa **intacta** (a diferencia de vz, que la traduce) |
| reloj | PL031 (`rtc-pl031`, el mismo que da Firecracker en arm64): un arranque en frío nace con la hora del host |
| discos | virtio-mmio block, uno por drive en el orden de las peticiones; cada disco tiene su hilo (el vCPU no espera a la E/S). Un FLUSH es un `fsync` del fichero, salvo en el overlay `kling_disposable` (como el `synchronizationMode none` de vz), que no lo hace |
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
`go test -fuzz FuzzColas ./internal/hvvm` programa los cinco dispositivos con
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
VM y los dorados los coloca el custodio. Sin `mach-lookup`, IOKit ni memoria
compartida: Hypervisor.framework no los necesita. Los reenvíos escuchan en
cualquier puerto de 127.0.0.1 en vez de una regla por puerto del rango
reservado: las 1000 reglas costaban ~38 ms de compilar el perfil en cada
restauración. Aun así `sandbox_init` cuesta ~10 ms y ~3 MiB de huella por
proceso (medido con un perfil vacío), y el custodio es otro proceso de ~8 MiB.

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

- `kling up -check` mira `kling-hv`, su entitlement y macOS 15 cuando
  `daemon.vmm` es `hv`; `make vz` compila los dos VMM.
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
| freeze | 102 ms | 149 ms |
| thaw + primer exec | 35 ms (30–55) | 201 ms |
| run -from + primer exec | 42 ms | 238 ms |
| sandbox fork -n 10 + exec en todas | 558 ms | no acabó (memoria) |
| exec en caliente (API) | 0,6 ms | 0,6 ms |
| memoria por copia de un dorado (10 / 50 copias) | 17,4 / 17,5 MiB | ~365 MiB |
| 50 copias | 875 MiB | — |

El thaw se reparte (fases del daemon) en ~3 ms de comprobar el VMM anterior,
~6 ms de lanzar el proceso, ~13 ms de cargar (de ellos ~10 de `sandbox_init`),
2–4 ms de `/resync` y el resto del CLI/API. El freeze es más caro que antes
(58–86 ms) porque escribe también las páginas residentes. La copia pesa más
que los 13,3 MiB de antes por el sandbox (~3 MiB por proceso); el custodio de
cada máquina es otro proceso de ~8 MiB que `kling top` no suma.

## Pendiente

- Pantalla.
- Que el daemon publique los dorados (una petición por el socket del broker)
  en vez del custodio: ~8 MiB menos por máquina, en vz también.
- `sandbox_init` cuesta ~10 ms y ~3 MiB por proceso: un perfil precompilado
  lo bajaría.
- Empaquetar `kling-hv` en `release.sh`, `install.sh`, `release.yml` y `kling
  upgrade` (hoy solo llevan `kling-vz`).
