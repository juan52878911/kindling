# kling-hv: VMM propio sobre Hypervisor.framework con RAM respaldada por fichero

Prototipo del tercer VMM de kindling para macOS (arm64). La RAM del invitado es
un fichero mapeado con `mmap(MAP_PRIVATE)` y pasado a `hv_vm_map`, como
demostró `prototypes/hvf-cow`: perezosa, copy-on-write y compartida entre
procesos. Con eso un restore no carga la RAM: la va pidiendo el invitado y las
páginas limpias son las mismas para todas las copias. Es lo que
Virtualization.framework no deja hacer (`kling-vz` carga 324 MiB por instancia
y tarda 210 ms en descongelar).

Arranca el MISMO kernel (`vmlinux`, Image arm64 6.1) y las mismas imágenes ext4
de kindling, con la misma línea de comandos que Firecracker (consola `ttyS0`).

## Piezas

| fichero | qué hace |
|---|---|
| `main.c` | VM, RAM desde fichero, GICv3 de HVF (`hv_gic_create`), vCPU (un hilo cada uno), salidas (MMIO, PSCI por HVC, sysregs, WFI), snapshot y restore |
| `fdt.c` | device tree en memoria: memoria, cpus, GICv3, timer, PSCI, 16550, virtio-mmio |
| `devices.c` | UART 16550 en MMIO (lo que el kernel ya trae para Firecracker; no tiene PL011) y virtio-mmio block |
| `net.c` | virtio-mmio net; el otro extremo es un socket unix `SOCK_DGRAM` con una trama por datagrama (el contrato de `VZFileHandleNetworkDeviceAttachment`) |
| `hvnet/` | (Go, módulo aparte) pila gVisor al otro lado del socket y sonda HTTP al agente `kling-guest` |
| `n-restores.sh` | N restores simultáneos del mismo snapshot y suma de `phys_footprint` |

Mapa: GIC distribuidor `0x08000000`, redistribuidores `0x0A000000` (HVF reserva
32 MiB), UART `0x09000000`, virtio `0x0C000000 + 0x200·i`, RAM en `0x80000000`.

Se descartó partir de libkrun: es Rust con su propio gestor de memoria
(`vm-memory`, memoria anónima) y su GIC en espacio de usuario; adaptarlo a RAM
de fichero era más trabajo que escribir ~1500 líneas de C sin dependencias.

## Uso

```sh
./build.sh                                   # clang + firma ad-hoc (com.apple.security.hypervisor)
(cd hvnet && GOWORK=off go build -o hvnet .)  # opcional, para la red
# arranque en frío; --mem es el fichero (disperso) que respalda la RAM
./kling-hv boot -k vmlinux -d redis.ext4:ro -d overlay.ext4 --mem /tmp/a.mem \
  -a "console=ttyS0 reboot=k panic=1 root=/dev/vda ro init=/sbin/overlay-init quiet ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off" \
  --mark "listening on :8080" --on-mark snap --snap-dir /tmp/snap
./kling-hv restore /tmp/snap -T
hvnet/hvnet -kling-hv ./kling-hv -- restore /tmp/snap       # mide restore -> GET /healthz
./n-restores.sh /tmp/snap 6
```

El snapshot es un directorio: `state` (vCPU, GIC, dispositivos), `mem` (clon
APFS del fichero de RAM + las páginas que el invitado cambió, según `mincore`)
y `diskN.ext4` (clon APFS de cada disco escribible). Cada restore clona su
propio overlay y lo borra al salir.

## Resultados (MacBook Pro M4, 16 GiB, macOS 26.5, 2026-10-27)

La máquina estaba bajo presión de memoria (~60 MiB libres, compresor activo),
así que hay ruido en los arranques en frío.

| medida | resultado |
|---|---|
| `hv_vm_create` + `hv_gic_create` | 0,45–1,4 ms |
| `mmap` + `hv_vm_map` de 512 MiB | 0,03–0,3 ms |
| arranque en frío, primera línea del kernel (earlycon) | 17 ms desde el inicio del proceso |
| arranque en frío hasta `Run /bin/sh` (min.ext4) | 58 ms |
| arranque en frío hasta `kling-guest ... listening on :8080` (redis, `quiet`) | 58–125 ms (5 tandas) |
| arranque en frío hasta el primer 200 de `GET /healthz` por red | 83–157 ms (5 tandas) |
| snapshot (pausa + estado + memoria) | 26–56 ms; pausa 0,01 ms, estado 2,4–8 ms, memoria 23–48 ms para 37–52 MiB sucios |
| restore hasta el primer `hv_vcpu_run` | 2,4–6,4 ms |
| restore hasta el 200 de `GET /healthz` del agente (secuencial, 10 tandas) | 6,1–8,8 ms |
| 6 restores lanzados a la vez, cada uno hasta su 200 | 72–85 ms |
| `phys_footprint` por restore, agente asentado, a los 3–4 s | 4,0–6,2 MiB |
| 6 restores del agente (sin red / con red) | 24,7 / 35,7 MiB en total |
| 20 restores del agente | 81,7 MiB en total |
| 6 restores del shell ejecutando un comando | 31,4 MiB en total |

Comparación: `kling-vz` paga 324 MiB y 210 ms por instancia restaurada.

Comprobaciones de que el restore es fiel: un shell restaurado ejecuta comandos,
lee 40 MiB del disco con el mismo md5 que el fichero, y `sleep 1` dura 1 s
(el timer virtual sigue donde estaba). 120 MiB de `/dev/urandom` escritos en un
tmpfs del invitado (solo existen en su RAM), con 26 MiB del proceso ya
comprimidos por el host, tienen el mismo md5 tras el restore. Con `-c 2` arranca
la segunda CPU por PSCI `CPU_ON` y el restore sigue con las dos (el timer
interrumpe en ambas). Tras el restore, cada petición HTTP al agente tarda
0,3–2 ms.

Si el snapshot se toma justo al imprimir `listening`, cada restore ensucia
~13 MiB más en su primer segundo (el runtime de Go del agente termina de
arrancar): 16,8 MiB por restore. Asentado 2,5 s antes, 4 MiB.

## Lo que se ha aprendido de la API

- Con el GIC de HVF (`hv_gic_create`, macOS 15+) el timer virtual y el WFI los
  resuelve el propio framework: no hay ni una salida `VTIMER_ACTIVATED` ni de
  WFI en todo el arranque, y el código que las trata no se usa.
- `hv_gic_state_create`/`get_data` (126 KiB) + los registros `ICC_*` de cada
  vCPU + `hv_vcpu_set_vtimer_offset` bastan para restaurar: no hubo que tocar
  los registros `ICH_*` ni los del redistribuidor por separado.
- `hv_gic_set_state` pisa cualquier SPI que un dispositivo suba antes de
  llamarlo: si una trama de red llega mientras se restaura y el dispositivo
  sube su línea, el invitado no la ve nunca y la tarjeta se queda muerta (7 de
  10 restores fallaron así). Los dispositivos esperan ahora a que la VM corra.
  Después de `hv_gic_set_state` se vuelve a poner el nivel guardado de cada
  línea; que el estado del GIC ya conserve una línea alta en el snapshot no
  está comprobado (`KHV_NO_REAFIRMAR=1` lo desactiva para probarlo).
- El snapshot incremental sale de `mincore`: una página va al fichero si está
  `MODIFIED`, `COPIED`, `ANONYMOUS` o `PAGED_OUT`. Con el compresor activo no
  se ha perdido ninguna (prueba del tmpfs); `--snap-full` escribe toda la RAM
  como control.
- Las copias de un proceso en marcha (no restaurado) cuestan más de lo que el
  invitado usa (318 MiB de `phys_footprint` con ~160 MiB escritos): sin
  investigar; los restores no lo sufren.

## Qué falta

- El reloj de pared: no hay RTC y el invitado arranca en 1970. En kindling lo
  arregla `/resync` tras el restore; para el arranque en frío hace falta un PL031.
- Peticiones de disco síncronas en el hilo del vCPU; sin `virtio-rng`, balloon
  ni vsock; sin descriptores indirectos ni `EVENT_IDX`.
- Un vCPU parado en WFI no se entera de las IPI: hoy da igual porque HVF
  resuelve el WFI dentro, pero el código de respaldo espera como mucho 2 ms con SMP.
- Sin pruebas automáticas ni integración con el núcleo.

## Esbozo del contrato con el núcleo (`vmm: hv`)

El núcleo elige el VMM en `internal/machine/vmm.go` (`daemon.vmm`:
`firecracker` | `vz`, y `KLING_VMM`). Lo más corto es que `hv` hable EXACTAMENTE
el contrato de `kling-vz` (docs/backend-vz.md): HTTP sobre `--api-sock` con las
rutas de Firecracker más `/kling/*`. Casi todo lo que hay detrás ya existe en
`vz/`: el servidor HTTP (`vz/internal/server`), la red de espacio de usuario
(`vz/internal/vnet`, que ya habla tramas por un socket `SOCK_DGRAM`, el mismo
que usa `net.c`), MMDS, egress y el proxy de credenciales. Solo cambia la capa
de la VM (`vz/internal/vzvm`). Diferencias de semántica:

| ruta | con `kling-hv` |
|---|---|
| `PUT /boot-source` | el `boot_args` de Firecracker pasa intacto (consola `ttyS0`, `pci=off` vale) |
| `PUT /machine-config` | `vcpu_count`, `mem_size_mib`: tamaño del fichero de RAM |
| `PUT /drives/{id}` | virtio-mmio block en el orden de llegada |
| `PUT /network-interfaces/{id}` | virtio-mmio net al socket de `vnet` |
| `PUT /snapshot/create` | `mem_file_path` = clon del fichero base + páginas sucias (`mincore`); `snapshot_path` = `state` |
| `PUT /snapshot/load` | `mem_backend: File` como Firecracker: `mmap(MAP_PRIVATE)` del fichero, sin copiar |
| `PATCH /vm` | `Paused`: `hv_vcpus_exit` + punto seguro en cada hilo; `Resumed` |

## Plan restante (estimación)

1. Convertir el C en una biblioteca (`khv_boot`, `khv_pause`, `khv_snapshot`,
   `khv_restore`, `khv_set_net_fd`) y un binario Go con cgo en `vz/` (o
   `hv/`, módulo aparte) que reutilice el servidor, `vnet`, MMDS y egress de
   `kling-vz`: 3–4 días.
2. RTC PL031, virtio-rng, disco asíncrono (un hilo por cola) y `/resync`
   tras el restore: 1–2 días.
3. `vmm: hv` en el núcleo (`BackendHV`, `NombreVMM`, admisión de memoria
   distinta: un restore ya no cuesta la RAM entera) y el e2e de Mac
   (`92-e2e-mac.sh`) contra él: 2–3 días.
4. Endurecer: límites de colas y longitudes de descriptor, sandbox del
   proceso como `kling-vz` (peercred), pruebas de fuzz de MMIO: 2 días.
