# Backend nativo para macOS: contrato entre el núcleo y `kling-vz`

En Linux el daemon lanza un proceso `firecracker` por microVM y le habla por un
socket unix con el API HTTP de Firecracker (`internal/fc`). En macOS lanza, en su
lugar, un proceso **`kling-vz`** por microVM que habla **el mismo API** y por
debajo usa Virtualization.framework. `kling-vz` vive en este repositorio, en
`vz/`, como **módulo Go aparte** (`github.com/juan52878911/kindling/vz`, binario
`vz/cmd/kling-vz`, `make vz`) porque necesita cgo y dependencias (`Code-Hex/vz`,
la pila de red de gVisor); el `go.mod` de la raíz sigue sin ninguna y sin cgo, y
`go test ./...` desde la raíz no entra en `vz/` (un módulo anidado queda fuera
del patrón).

El backend lo elige el usuario con la clave `daemon.vmm` de la configuración
(`firecracker` o `vz`; vacía = el de la plataforma) o con `KLING_VMM`, que acepta
un nombre de backend o la ruta del binario. Se valida contra la máquina: `vz`
solo en macOS sobre Apple Silicon, `firecracker` solo en Linux con KVM. Lo que
el núcleo hace alrededor del VMM (§5) va por etiquetas de compilación, así que
la clave elige binario y extensiones del protocolo dentro de lo que el binario
de `kling` sabe hacer en su sistema.

Por qué así y no un backend dentro del daemon: el `Manager` ya sabe arrancar,
congelar, descongelar, clonar y apretar máquinas hablando ese API. Si el ayudante
lo imita, el ciclo de vida, los snapshots dorados, el TTL, los sandboxes, el exec
y el scheduler funcionan en el Mac sin reescribirse. Lo que cambia en el núcleo
es lo que en Linux hace el HOST alrededor de Firecracker (red, cgroups, jailer,
`/proc`), no la conversación con el VMM.

Resultados del prototipo que justifican las decisiones: `docs/vz-mac-prototipo.md`.

## 1. El proceso

```
kling-vz --api-sock <ruta>
```

- Un proceso por microVM, como Firecracker. El daemon lo lanza con `setsid`,
  su stdout y stderr van a `machines/<id>/firecracker.log` (el nombre se
  conserva: es lo que lee `kling logs`).
- La ruta de `--api-sock` puede pasar de los 104 bytes de `sun_path` (la raíz
  vive en `~/Library/Application Support/kindling`): el ayudante se ata con
  `chdir` a su directorio y el nombre corto, y el núcleo conecta a través de un
  enlace simbólico corto en `/tmp/kling-<uid>/` (`internal/fc/dial.go`). La ruta
  completa sigue en la línea de órdenes: es como el núcleo reconoce sus VMM en
  la tabla de procesos.
- **stdout = consola del invitado** (`hvc0`), más las líneas de diagnóstico del
  propio ayudante con prefijo `kling-vz:`.
- Termina cuando la VM se para (el invitado se apaga o reinicia: `reboot=k
  panic=1` hace que un pánico acabe aquí) y ante SIGTERM (para la VM y sale).
  Un SIGKILL no deja nada: el proceso auxiliar de Apple muere con su cliente.
- Corre como el usuario, sin root. Tiene que estar firmado (ad-hoc vale) con el
  entitlement `com.apple.security.virtualization`.
- `kling-vz --version` imprime la versión y sale.
- Requisitos: Apple Silicon y macOS 14 o superior (guardar y restaurar estado).

## 2. API compatible con Firecracker

HTTP/1.1 sobre el socket unix de `--api-sock`. Mismos métodos, rutas y cuerpos
que `internal/fc/client.go`; los errores, con estado 4xx y
`{"fault_message": "..."}`. Lo que no esté aquí contesta 400 con un mensaje que
diga que `kling-vz` no lo implementa.

| Petición | Qué hace en `kling-vz` |
|---|---|
| `GET /` | 200. Es el ping de `waitSocket`. |
| `PUT /boot-source` `{kernel_image_path, boot_args}` | Guarda kernel y línea de comandos. **Traduce** `console=ttyS0` a `console=hvc0` y quita `pci=off` (el invitado usa virtio-PCI en vz); el resto, incluido `ip=`, pasa tal cual. El `vmlinux` aarch64 de Firecracker se usa sin convertir. |
| `PUT /machine-config` `{vcpu_count, mem_size_mib}` | CPUs y memoria. |
| `PUT /drives/{id}` `{drive_id, path_on_host, is_root_device, is_read_only}` | Un virtio-blk por drive **en el orden de las peticiones** (el primero es `vda`, el segundo `vdb`…), como hace Firecracker. `rate_limiter` se ignora. |
| `PATCH /drives/{id}` `{path_on_host}` | Cambia la ruta **registrada** del drive. Antes de arrancar o de reanudar un snapshot cargado, es la ruta que se usa al crear la VM. Con la VM ya creada, solo cambia lo que se escribe en el próximo `snapshot/create`: el núcleo solo parchea con la VM en pausa, alrededor de un snapshot, y deja la ruta como estaba antes de reanudar (`Commit`). |
| `PUT /network-interfaces/{id}` `{iface_id, host_dev_name, guest_mac}` | Tarjeta virtio-net con la MAC pedida, conectada a la red de espacio de usuario de la §4. `host_dev_name` se ignora. |
| `PUT /entropy` | virtio-rng. |
| `PUT /balloon` `{amount_mib, deflate_on_oom, stats_polling_interval_s}` | Globo tradicional. `amount_mib` es lo INFLADO, como en Firecracker: el objetivo del framework es `mem_size_mib - amount_mib`. |
| `PATCH /balloon` `{amount_mib}` | Mueve el globo en caliente. |
| `GET /balloon/statistics` | `{target_mib, actual_mib, free_memory, available_memory, total_memory}`. El framework no da estadísticas del invitado: `target_mib` y `actual_mib` son lo pedido y los tres de memoria van a 0, que el núcleo lee como "desconocido": `squeeze` aprieta entonces hasta dejar al invitado la mitad de su memoria (mínimo 128 MiB), mide lo devuelto con `GET /kling/stats` y, si devolvió algo, deja el globo inflado (desinflar repuebla las páginas). |
| `PUT /mmds/config` `{version, ipv4_address, network_interfaces}` | Activa MMDS en esa dirección (siempre `169.254.169.254`, versión `V2`). |
| `PUT /mmds` (cualquier JSON) | Sustituye el almacén de MMDS. |
| `PUT /actions` `{"action_type": "InstanceStart"}` | Crea y arranca la VM con lo configurado. |
| `PATCH /vm` `{"state": "Paused" \| "Resumed"}` | Pausa o reanuda. Reanudar un snapshot cargado con `resume_vm: false` es el momento en que se crea la VM y se restaura su estado (ver abajo). |
| `PUT /snapshot/create` `{snapshot_type, snapshot_path, mem_file_path}` | Con la VM en pausa. `mem_file_path` recibe el estado de la VM del framework (`saveMachineStateTo`). `snapshot_path` recibe un JSON con TODO lo necesario para recrearla: `{"kling_vz": 1, boot_source, machine_config, drives[], network, mmds_config, balloon, entropy, machine_identifier}`. El `machine_identifier` (el `VZGenericMachineIdentifier` en base64) es obligatorio: el framework se niega a restaurar con otro. |
| `PUT /snapshot/load` `{snapshot_path, mem_backend: {backend_path}, resume_vm}` | Lee el JSON. Con `resume_vm: true` crea la VM con la misma configuración e identificador, restaura el estado y la reanuda. Con `false` lo deja **pendiente**: los `PATCH /drives` que lleguen después cambian las rutas, y el `PATCH /vm Resumed` crea, restaura y reanuda. El núcleo carga siempre con `false` antes de parchear el overlay. |

La memoria de un snapshot es un único fichero que todas las réplicas LEEN; nadie
lo escribe tras crearlo. Eso preserva el modelo del snapshot dorado aunque el
framework copie el estado a memoria al restaurar en vez de mapearlo.

## 3. Rutas propias (solo `kling-vz`)

El núcleo las llama únicamente en macOS.

| Petición | Qué hace |
|---|---|
| `GET /kling/info` | `{"backend": "vz", "version": "x.y.z"}`. |
| `PUT /kling/network` `{"egress": "none"\|"internet"\|"allowlist", "allow_domains": [...]}` | Política de salida de esta máquina. Se manda antes de `InstanceStart` o de `snapshot/load`; si nunca llega, `none`. No viaja en el snapshot: es de la instancia, como el namespace en Linux. |
| `PUT /kling/forwards` `{"ports": [8080, ...]}` | Abre un puerto en `127.0.0.1` (elegido por el sistema) por cada puerto del invitado y contesta `{"forwards": {"8080": "127.0.0.1:61234", ...}}`. Se puede llamar en cualquier momento tras crear la red; repetir un puerto devuelve la misma dirección. |
| `GET /kling/stats` | `{"footprint_mib": N}`: memoria que ocupa esta máquina en el host, sumando el `phys_footprint` del ayudante y del proceso auxiliar `com.apple.Virtualization.VirtualMachine` que la aloja. Es lo que en Linux es el RSS de firecracker. |

### Por qué puertos en loopback y no la IP del invitado

En Linux cada invitado es `172.16.0.2` dentro de su namespace, y el host lo
alcanza por la IP del veth (`Machine.IP`). En el Mac no hay namespaces: todos los
invitados tienen la misma IP y viven en redes de espacio de usuario separadas. El
host los alcanza por los puertos que abre su ayudante en `127.0.0.1`, que el núcleo
guarda en `Machine.Forwards` y resuelve con `Machine.Addr(puerto)`. Quien hoy
construye `mc.IP + ":" + puerto` (el proxy del daemon, exec, shell, el scheduler y
kindling-mcp) pasa a usar `Addr`, que en Linux sigue devolviendo `IP:puerto`. Es
el mismo alcance que ya tiene la ruta al veth en Linux: cualquiera en el host.

Los puertos que se reenvían son `api.GuestPort` más los de la etiqueta
`kling.ports`. Se piden tras cada arranque o descongelación, porque un proceso
nuevo abre puertos nuevos.

## 4. La red de cada máquina

Cada `kling-vz` monta una red de espacio de usuario propia
(`VZFileHandleNetworkDeviceAttachment` + pila TCP/IP en el proceso) que imita la
del namespace de Linux, para que un snapshot sirva igual en los dos sistemas:

- invitado `172.16.0.2/30`, pasarela `172.16.0.1`, MAC `06:00:AC:10:00:02` (la
  que manda el núcleo), sin DHCP (el `ip=` de la línea de comandos la fija);
- MMDS v2 en `169.254.169.254:80`, con la semántica de Firecracker que usa
  `pkg/guest/mmds.go` (`PUT /latest/api/token` con
  `X-metadata-token-ttl-seconds`, lecturas con `X-metadata-token`, JSON con
  `Accept: application/json`);
- DNS: las consultas al puerto 53 de cualquier destino se resuelven en el host
  cuando la política lo permite;
- **egress** con la misma semántica que `internal/net/firewall.go`: `none` no
  deja salir nada salvo MMDS; `internet` sale por el host pero nunca a los rangos
  que bloquea `isBlockedIP` (privados, enlace local, metadatos, el propio host);
  `allowlist` solo a las IPs públicas de los dominios de la lista, resueltas y
  refrescadas como hace `dnsresolver.go`, y su DNS solo contesta esos dominios.

## 5. Lo que cambia en el núcleo en macOS

| Linux | macOS |
|---|---|
| `firecracker` (o `jailer`) | `kling-vz`, buscado junto a `kling` o en el `PATH` (`KLING_VMM` lo fuerza; `daemon.vmm` elige el backend) |
| netns + veth + tap + iptables (`internal/net`) | `PUT /kling/network` al ayudante; sin red en el host |
| `Machine.IP` = IP del veth | `Machine.IP` = `172.16.0.2` (informativa) y `Machine.Forwards` |
| cgroups, jailer, bajada de privilegios | no existen; el aislamiento es el proceso auxiliar de Apple |
| RSS de `/proc/<pid>` | `GET /kling/stats` |
| clon del overlay con `cp --reflink` | `clonefile` de APFS (`cp -c`) |
| admisión por PSI (`/proc/pressure/memory`) | presión de memoria del sistema (`kern.memorystatus_level`) |
| barrido de huérfanos por `/proc` | por la tabla de procesos del sistema |
| raíz `/var/lib/kindling`, daemon como root | raíz en el directorio del usuario, daemon sin root |
| `mkfs.ext4`, `debugfs` de e2fsprogs | los mismos, de Homebrew (`brew install e2fsprogs`) |
| construir imágenes (`POST /images`) | no disponible (501): se construyen en Linux y se copian (`kling images copy`, `GET/PUT /images/{name}/blob`) |
| arranques simultáneos: 2 anidado, núcleos/2 en hierro | 4 (`KLING_MAX_PARALLEL_BOOT`) |
