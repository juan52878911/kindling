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
| discos | virtio-mmio block, uno por drive en el orden de las peticiones; cada disco tiene su hilo (el vCPU no espera a la E/S) |
| red | virtio-mmio net a un socket `SOCK_DGRAM` (una trama por datagrama): el contrato de `VZFileHandleNetworkDeviceAttachment`, así que `vnet` sirve tal cual |
| entropía | virtio-rng (`PUT /entropy`), con `arc4random_buf` |
| sin | globo, pantalla, vsock |

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

## Pausa y techo de CPU

La VM corre con todos los bits de pausa a cero: el del núcleo (`PATCH /vm`) y
el del regulador de CPU (`Deps.Freeze`, que en vz es SIGSTOP al auxiliar de
Apple). Pausar saca a los vCPU a un punto seguro (`hv_vcpus_exit`) y espera a
que los hilos de disco y red no toquen la RAM. Bajo el regulador el reloj del
invitado sigue corriendo, como bajo `cpu.max`. `CPUTime` es la CPU de los hilos
de vCPU.

## Lo que cambia en el núcleo con `hv`

- `restaurarComparteMemoria` es verdad: las copias de un dorado son
  `MemShared` y la reserva de la segunda en adelante es la fracción divergente
  (`shareReserveDiv`), como en Firecracker.
- La compuerta de restauraciones deja pasar 8 a la vez (3 con vz).
- El congelado diferencial sigue siendo solo de Firecracker: el fichero de
  memoria de hv ya es un clon de su base más lo sucio.
- `GET /kling/stats` es el `phys_footprint` del propio `kling-hv` (no hay
  auxiliar).

## Medidas (MacBook Pro M4, 16 GiB, macOS 26.5, daemon privado, n=10)

Ver el informe de la rama: cada operación vista desde el API del daemon
(incluye el `ps` de ~27 ms con que el núcleo comprueba que no quede un VMM
vivo y el `/resync` del agente).

| | hv | vz |
|---|---|---|
| run (frío) + primer exec | 202 ms | 591 ms |
| freeze | 58–86 ms | 156 ms |
| thaw + primer exec | 113–142 ms | 237 ms |
| run -from + primer exec | 75 ms | 255 ms |
| sandbox fork -n 10 | 429 ms | 2,8 s |
| memoria por copia de un dorado | 13,3 MiB | 319 MiB |
| restore a pelo (proceso nuevo → 200 de /healthz) | ~10 ms | — |

## Pendiente

- Perfil de sandbox (`kling-hv` corre hoy sin confinar: avisa si el daemon
  pide `KLING_VZ_CONFINE_ROOT`) y peercred ya está (es el de vnet).
- Globo (`squeeze` falla) y pantalla.
- Endurecer las colas virtio (longitudes, descriptores indirectos), fuzz de
  MMIO, e2e de Mac (`92-e2e-mac.sh`) contra hv.
- Bajar los ~9 MiB del proceso Go (runtime, gVisor, servidor) que se suman a
  los ~4 MiB del invitado en cada copia.
