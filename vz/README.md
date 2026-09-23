# kling-vz

El VMM de kindling en macOS. Un proceso por microVM que habla **el mismo API
HTTP que Firecracker** por un socket unix y, por debajo, usa
Virtualization.framework de Apple. El daemon lo lanza en lugar de `firecracker`
y el resto del núcleo (ciclo de vida, snapshots dorados, TTL, sandboxes, exec,
scheduler) funciona sin reescribirse.

El contrato entre el núcleo y este ayudante está en
[`docs/backend-vz.md`](../docs/backend-vz.md); las medidas que justifican el
diseño, en [`docs/vz-mac-prototipo.md`](../docs/vz-mac-prototipo.md).

Es un módulo Go aparte (`github.com/juan52878911/kindling/vz`) porque necesita
cgo, `Code-Hex/vz` y la pila de red de gVisor; el `go.mod` del núcleo sigue sin
dependencias y sin cgo.

Requisitos: Apple Silicon, macOS 14 o superior, y el binario firmado (ad-hoc
vale) con el entitlement `com.apple.security.virtualization`.

## Compilar y probar

```sh
./build.sh                      # -> bin/kling-vz, firmado con kling-vz.entitlements
make check                      # gofmt + vet (darwin y linux) + go test -race
GUEST_DIR=/ruta/guest make smoke  # prueba de humo en este Mac (ver abajo)
bin/kling-vz --version
```

`build.sh` acepta `OUT=` (ruta del binario) y `VERSION=` (por defecto,
`git describe`). Sin la firma, crear la VM falla con un error de permisos poco
claro.

## Cómo se usa

```sh
kling-vz --api-sock /ruta/fc.sock
```

- **stdout** es la consola del invitado (`hvc0`) más las líneas de diagnóstico
  del ayudante, que empiezan siempre en columna 0 con `kling-vz:`. El daemon
  las manda a `machines/<id>/firecracker.log`.
- Termina cuando el invitado se apaga o reinicia (`reboot=k panic=1`) y ante
  SIGTERM o SIGINT (para la VM, borra el socket y sale con 0). Un SIGKILL no
  deja nada: el proceso auxiliar de Apple muere con su cliente.
- `--id` se acepta y se ignora, para quien lo lance con los argumentos de
  Firecracker.

### API compatible con Firecracker

| Petición | Notas |
|---|---|
| `GET /` | Ping; devuelve `{"state": ...}` como Firecracker. |
| `PUT /boot-source` | Traduce `console=ttyS0` a `console=hvc0` y quita `pci=off`; el resto pasa igual. |
| `PUT /machine-config` | vCPUs y memoria. |
| `PUT /drives/{id}` | Un virtio-blk por drive, en el orden de las peticiones (`vda`, `vdb`...). `rate_limiter` se ignora. |
| `PATCH /drives/{id}` | Cambia la ruta registrada: antes de crear la VM, la que se usa al crearla; después, solo la que se graba en el próximo snapshot. |
| `PUT /network-interfaces/{id}` | Una sola tarjeta, con la MAC pedida, conectada a la red de usuario. |
| `PUT /entropy` | virtio-rng. |
| `PUT /balloon`, `PATCH /balloon` | `amount_mib` es lo inflado: el objetivo del framework es `mem_size_mib - amount_mib`. |
| `GET /balloon/statistics` | `target_mib` y `actual_mib` son lo pedido; la memoria del invitado va a 0 (el framework no la da). |
| `PUT /mmds/config` | Solo V2; `ipv4_address` tiene que ser de enlace local. |
| `PUT /mmds`, `PATCH /mmds`, `GET /mmds` | El almacén (PATCH es un merge patch RFC 7396). |
| `PUT /actions` | Solo `InstanceStart`. |
| `PATCH /vm` | `Paused` / `Resumed`. |
| `PUT /snapshot/create` | Solo `Full`, con la VM en pausa. |
| `PUT /snapshot/load` | Solo backend `File`, y solo en un ayudante recién lanzado. |

Lo demás contesta 400 con `{"fault_message": "kling-vz does not implement ..."}`,
y cada petición rechazada queda también en el log con el prefijo `kling-vz:`.

### Rutas propias

| Petición | Qué hace |
|---|---|
| `GET /kling/info` | `{"backend": "vz", "version": "x.y.z"}` |
| `PUT /kling/network` | `{"egress": "none"\|"internet"\|"allowlist", "allow_domains": [...]}`. Se puede cambiar en caliente. |
| `PUT /kling/forwards` | `{"ports": [8080]}` → `{"forwards": {"8080": "127.0.0.1:61234"}}`. Repetir un puerto devuelve la misma dirección. |
| `GET /kling/stats` | `{"footprint_mib": N}`: `phys_footprint` del ayudante más el del auxiliar de Apple que aloja la VM. |

## Snapshots

`snapshot_path` recibe un JSON con todo lo necesario para recrear la VM
(`internal/spec`):

```json
{
  "kling_vz": 1,
  "boot_source": {"kernel_image_path": "...", "boot_args": "console=ttyS0 ..."},
  "machine_config": {"vcpu_count": 1, "mem_size_mib": 256},
  "drives": [{"drive_id": "rootfs", "path_on_host": "...", "is_root_device": true, "is_read_only": true}, ...],
  "network": {"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:AC:10:00:02"},
  "mmds_config": {"version": "V2", "ipv4_address": "169.254.169.254", "network_interfaces": ["eth0"]},
  "balloon": {"amount_mib": 0, "deflate_on_oom": true, "stats_polling_interval_s": 1},
  "entropy": true,
  "machine_identifier": "<VZGenericMachineIdentifier en base64>"
}
```

y `mem_file_path` el estado de la VM (`saveMachineStateTo`). Se guarda
`boot_args` sin traducir, tal y como llegó: el invitado despierta con su línea
de comandos congelada en memoria y no se vuelve a usar, pero así el JSON es el
mismo que describiría la máquina en Linux.

- El JSON se escribe de forma atómica (temporal + rename).
- Si `mem_file_path` ya existe se borra antes: Firecracker lo pisa y el
  framework se niega.
- `snapshot/load` con `resume_vm: false` deja la VM **pendiente**: los `PATCH
  /drives` posteriores cambian las rutas, y `PATCH /vm Resumed` crea la VM,
  restaura el estado y la reanuda. La red se monta ya en el `load`, así que
  `/kling/forwards` funciona en ese intervalo.
- Si una restauración falla, la VM a medio crear se para y el ayudante sigue
  vivo: se puede reintentar el `Resumed`.
- Tras restaurar con `amount_mib > 0`, se vuelve a aplicar el objetivo del globo.

## La red

`internal/vnet` monta, por máquina, un socketpair de datagramas
(`VZFileHandleNetworkDeviceAttachment`, una trama ethernet por datagrama) y al
otro lado una pila TCP/IP de gVisor (`gvisor.dev/gvisor/pkg/tcpip`, rama `go`).
Imita el namespace de Linux para que un snapshot sirva en los dos sistemas:

- Invitado `172.16.0.2/30`, pasarela `172.16.0.1` con MAC fija
  `06:00:ac:10:00:01` (el invitado la tiene en su caché ARP al restaurar), sin
  DHCP. La MAC del invitado se registra como vecino estático.
- **MMDS** en `169.254.169.254:80`, servido en el proceso con la semántica V2 de
  Firecracker (token por `PUT /latest/api/token` con
  `X-metadata-token-ttl-seconds` de 1 a 21600; lecturas con `X-metadata-token`;
  JSON con `Accept: application/json`, formato IMDS si no). La dirección es
  propia de la pila porque el puente del invitado pone una ruta on-link a ella y
  pregunta por ARP.
- **DNS**: toda consulta al puerto 53 (UDP y TCP), vaya a la IP que vaya, se
  contesta aquí. Se reenvía al primer `nameserver` IPv4 de `/etc/resolv.conf`
  del Mac (o a `1.1.1.1` si no hay).
- **TCP y UDP de salida**: la pila termina cada conexión del invitado y la
  vuelve a abrir desde el Mac solo si la política lo permite. Los flujos UDP se
  cierran tras 60 s sin tráfico.
- **Puertos reenviados**: `127.0.0.1:<elegido por el sistema>` → `172.16.0.2:<puerto>`.

### Política de salida

Misma semántica que `internal/net/firewall.go` y `dnsresolver.go` del núcleo:

| Modo | TCP/UDP | DNS |
|---|---|---|
| `none` (por defecto) | nada | `REFUSED` |
| `internet` | cualquier IP salvo las bloqueadas | cualquier nombre |
| `allowlist` | solo IPs sembradas | solo los dominios listados y sus subdominios (`REFUSED` al resto) |

Las IPs de la allowlist se siembran como en Linux: al recibir la política se
resuelven los dominios en segundo plano (permanentes) y cada respuesta DNS que
el invitado recibe por un dominio permitido siembra sus registros A con su TTL
(mínimo 60 s). Nunca entran IPs bloqueadas. Cambiar de política vacía el
conjunto.

Bloqueadas siempre: `10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `127/8`,
`100.64/10` (la lista del núcleo) más `0/8`, `224/4`, `240/4` y las IPs de las
interfaces del propio Mac.

## Diferencias con Firecracker y con el contrato

Decisiones tomadas donde el contrato no decía nada o no bastaba:

1. **DNS en `none`**: se contesta `REFUSED` en vez de descartar. No sale nada
   del Mac, y el invitado falla al momento en vez de esperar 5 s por intento.
2. **Bloqueo ampliado**: además de la lista de `isBlockedIP`, `0/8` (en macOS
   `connect()` a `0.0.0.0` llega a localhost), multicast, reservadas y las IPs
   del propio Mac (con una IP pública asignada directamente no caerían en
   ningún rango privado).
3. **Upstream DNS**: el del Mac, no `1.1.1.1` fijo como en la allowlist de
   Linux, para respetar VPNs y redes corporativas.
4. **Socket con ruta larga**: `sun_path` admite 104 bytes en macOS. Con una ruta
   más larga el ayudante hace `chdir` al directorio y escucha por el nombre
   relativo. Quien conecte tiene que hacer lo mismo (o usar rutas cortas).
5. **`squeeze`**: como `/balloon/statistics` da la memoria del invitado a 0, el
   cálculo de `squeezeLocked` no encuentra holgura y no infla nada. El globo
   funciona con `PATCH /balloon` directo (ver números abajo).
6. **ICMP** no sale del Mac; el invitado sí recibe respuesta a ping a la pasarela.
7. **Consola**: solo salida. La entrada del `hvc0` es una tubería que nadie
   escribe.

## Estructura

```
cmd/kling-vz        main: flags, socket, señales, stdout sincronizado
internal/server     API de Firecracker + /kling/*, máquina de estados; la VM es una interfaz
internal/spec       configuración, JSON del snapshot, traducción de boot_args
internal/vnet       red de usuario con gVisor: enlace, MMDS, DNS, salida, reenvíos
internal/egress     política none/internet/allowlist, conjunto de IPs con TTL, DNS
internal/mmds       metadata service V2
internal/vzvm       la VM real con Code-Hex/vz (solo darwin)
internal/footprint  phys_footprint del ayudante y de su auxiliar de Apple (solo darwin)
scripts/smoke.sh    prueba de humo contra un Mac real
```

La huella se atribuye por la tubería de la consola: el framework entrega al
auxiliar `com.apple.Virtualization.VirtualMachine` el mismo extremo de tubería
que le da el ayudante, y el kernel expone su identidad (`pipe_handle`) en los
dos procesos. El padre del auxiliar es launchd y su proceso "responsable" es la
app que lanzó al daemon, igual para todas las máquinas, así que ninguno de los
dos sirve para distinguirlas.

## Pruebas

`go test -race ./...` cubre sin VM: la máquina de estados del API con una VM
falsa (secuencias de boot, Commit, runFrom y Thaw del núcleo, errores con
`fault_message`), el JSON del snapshot, la traducción de `boot_args`, las
decisiones de salida y el DNS, el flujo de tokens de MMDS, y la red completa con
**otra pila de gVisor haciendo de invitado** al otro lado de un socketpair
(reenvíos, MMDS desde el invitado, `none`/`internet`/`allowlist`).

`scripts/smoke.sh` prueba el ayudante solo, con curl, en un Mac:

```sh
GUEST_DIR=/ruta/con/vmlinux,min.ext4,overlay-64.ext4,everything.layer.ext4 scripts/smoke.sh
```

Arranque en frío con la capa `everything` → agente del invitado alcanzable por
el puerto reenviado → MMDS desde el invitado → `none` bloquea → pausa + snapshot
con el overlay reapuntado (como `Commit`) → SIGTERM → ayudante nuevo con `load`
`resume_vm=false` + `PATCH` del overlay + `Resumed` → alcanzable → globo →
`internet` deja salir → tercer ayudante con `resume_vm=true` y `allowlist`. Nunca
escribe en `GUEST_DIR`: lo escribible se clona con `cp -c`.

Resultado en un Mac M4 (16 GiB, macOS 26.5.1), 1 vCPU y 256 MiB, 2026-09-23:

| | |
|---|---|
| Arranque en frío hasta que el agente contesta por el reenvío | 1176 ms (`InstanceStart` 93 ms) |
| Pausa | 13–15 ms |
| `snapshot/create` | 109–127 ms, estado de 38 MiB |
| Restaurar (`load` + `Resumed`) | 171–180 ms; el agente contesta 15–33 ms después |
| `load` con `resume_vm=true` | 159–163 ms |
| Huella en marcha | 94–95 MiB |
| Huella restaurada | 379–382 MiB |
| Huella restaurada con 128 MiB inflados | 261 MiB |
