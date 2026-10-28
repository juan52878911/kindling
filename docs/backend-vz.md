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
| `PUT /drives/{id}` `{drive_id, path_on_host, is_root_device, is_read_only, kling_disposable?}` | Un virtio-blk por drive **en el orden de las peticiones** (el primero es `vda`, el segundo `vdb`…), como hace Firecracker. `rate_limiter` se ignora. Los de solo lectura (base, capas) van con la caché del host (`cached`); los de escritura, con la del framework. `kling_disposable` (opcional, extensión de `kling-vz`; Firecracker no lo admite, así que solo se manda a `kling-vz`): el drive es el overlay desechable de una máquina **no persistente**, y entonces los flush del invitado no se llevan al disco físico (`synchronizationMode none`): si el Mac se cae, se pierde un disco que se iba a borrar igual. Nunca para volúmenes ni discos de `kling db`; se ignora en drives de solo lectura y en la raíz. No se guarda en el snapshot. `KLING_VZ_DISK_CACHING` y `KLING_VZ_DISK_SYNC` en el entorno de `kling-vz` mandan sobre todo esto, para todos los drives. |
| `PATCH /drives/{id}` `{path_on_host, kling_disposable?}` | Cambia la ruta **registrada** del drive. Antes de arrancar o de reanudar un snapshot cargado, es la ruta que se usa al crear la VM. Con la VM ya creada, solo cambia lo que se escribe en el próximo `snapshot/create`: el núcleo solo parchea con la VM en pausa, alrededor de un snapshot, y deja la ruta como estaba antes de reanudar (`Commit`). `kling_disposable` (ver `PUT`) solo se acepta antes de crear la VM: es como se marca el overlay de un clon restaurado (load con `resume_vm: false` + este `PATCH`). |
| `PUT /network-interfaces/{id}` `{iface_id, host_dev_name, guest_mac}` | Tarjeta virtio-net con la MAC pedida, conectada a la red de espacio de usuario de la §4. `host_dev_name` se ignora. |
| `PUT /entropy` | virtio-rng. |
| `PUT /balloon` `{amount_mib, deflate_on_oom, stats_polling_interval_s}` | Globo tradicional. `amount_mib` es lo INFLADO, como en Firecracker: el objetivo del framework es `mem_size_mib - amount_mib`. |
| `PATCH /balloon` `{amount_mib}` | Mueve el globo en caliente. |
| `GET /balloon/statistics` | `{target_mib, actual_mib, free_memory, available_memory, total_memory}`. El framework no da estadísticas del invitado: `target_mib` y `actual_mib` son lo pedido y los tres de memoria van a 0, que el núcleo lee como "desconocido": `squeeze` aprieta entonces hasta dejar al invitado la mitad de su memoria (mínimo 128 MiB), mide lo devuelto con `GET /kling/stats` y, si devolvió algo, deja el globo inflado (desinflar repuebla las páginas). |
| `PUT /mmds/config` `{version, ipv4_address, network_interfaces}` | Activa MMDS en esa dirección (siempre `169.254.169.254`, versión `V2`). |
| `PUT /mmds` (cualquier JSON) | Sustituye el almacén de MMDS. |
| `PUT /actions` `{"action_type": "InstanceStart"}` | Crea y arranca la VM con lo configurado. |
| `PATCH /vm` `{"state": "Paused" \| "Resumed"}` | Pausa o reanuda. Reanudar un snapshot cargado con `resume_vm: false` es el momento en que se crea la VM y se restaura su estado (ver abajo). |
| `PUT /snapshot/create` `{snapshot_type, snapshot_path, mem_file_path}` | Con la VM en pausa. `mem_file_path` recibe el estado de la VM del framework (`saveMachineStateTo`). `snapshot_path` recibe un JSON con TODO lo necesario para recrearla: `{"kling_vz": 1, boot_source, machine_config, drives[], network, mmds_config, balloon, entropy, machine_identifier}`. El `machine_identifier` (el `VZGenericMachineIdentifier` en base64) es obligatorio: el framework se niega a restaurar con otro. `kling_vz` es la versión más baja que lo describe: 2 con `graphics`, 1 sin ella (un kling-vz anterior la lee). |
| `PUT /snapshot/load` `{snapshot_path, mem_backend: {backend_path}, resume_vm}` | Lee el JSON. Con `resume_vm: true` crea la VM con la misma configuración e identificador, restaura el estado y la reanuda. Con `false` lo deja **pendiente**: los `PATCH /drives` que lleguen después cambian las rutas, y el `PATCH /vm Resumed` crea, restaura y reanuda. El núcleo carga siempre con `false` antes de parchear el overlay. |

La memoria de un snapshot es un único fichero que todas las réplicas LEEN; nadie
lo escribe tras crearlo. Eso preserva el modelo del snapshot dorado aunque el
framework copie el estado a memoria al restaurar en vez de mapearlo.

**Sin VMGenID.** Firecracker da al invitado un VMGenID que cambia en cada
restauración y el kernel resiembra solo su generador; Virtualization.framework
no tiene nada parecido. Dos réplicas restauradas del mismo snapshot sacaban los
mismos bytes de `getrandom()` —y con ellos los mismos ids de sesión— y el reloj
de pared seguía en el instante del volcado. Por eso el núcleo, tras el `PATCH /vm
Resumed` (o la carga con `resume_vm: true`) y la apertura de reenvíos, llama a
`POST /resync` del agente con la hora del host y 64 bytes de entropía antes de
dar la máquina por arrancada (ver [api.md](api.md#tras-restaurar-reloj-y-entropía-del-invitado)).
Lo hace igual en Linux, donde VMGenID resiembra pero el reloj también se queda
parado.

## 3. Rutas propias (solo `kling-vz`)

El núcleo las llama únicamente en macOS.

| Petición | Qué hace |
|---|---|
| `GET /kling/info` | `{"backend": "vz", "version": "x.y.z", "credential_kinds": [...]}`. `credential_kinds` dice qué entiende su proxy: `http`, `postgres`, `postgres-upstream` y, si el daemon le dio broker (`KLING_VZ_BROKER`), `graph-link`: aristas de grafo y credenciales con `upstream_machine`. |
| `PUT /kling/network` `{"egress": "none"\|"internet"\|"allowlist", "allow_domains": [...]}` | Política de salida de esta máquina. Se manda antes de `InstanceStart` o de `snapshot/load`; si nunca llega, `none`. No viaja en el snapshot: es de la instancia, como el namespace en Linux. |
| `PUT /kling/credentials` `{"credentials": [{"env", "domain", "placeholder", "secret", "allow"}, ...]}` | Juego COMPLETO de credenciales del proxy de credenciales (`pkg/credproxy`); la lista vacía las quita. Exige que la política ya sea `allowlist`, salvo si TODAS van a otra máquina (`upstream_machine`, las aristas `credential` de un grafo): esas se piden al broker del daemon en cada conexión y no salen a ningún otro sitio. El ayudante sirve el proxy en la pasarela:80 y su DNS contesta esos dominios con la pasarela. La clave queda en la memoria del ayudante (SECURITY.md §7). `allow` (opcional) es el mismo formato que en Linux (`"MÉTODO /ruta"`, `*` y `**`) y lo aplica el mismo `pkg/credproxy`. El núcleo la manda tras `PUT /kling/network` y antes de arrancar o cargar, y otra vez con los marcadores en MMDS. Contesta `{"domains": [...]}`. |
| `PUT /kling/graph` `{"links": [{"host": "api.graph", "port": 8081}], "hosts": ["api.graph", "db.graph"]}` | Aristas salientes del nodo (sustituye las anteriores). Su DNS contesta los `hosts` con la pasarela en todos los modos (y NXDOMAIN a cualquier otro `*.graph`), y una conexión a la pasarela en el puerto de un `link` se pide al broker de enlaces del daemon (`pkg/linkbroker`), que entrega el socket ya conectado. Sin direcciones. El núcleo la manda antes de arrancar o cargar un nodo, y al montar el grafo. Ni el 8080 ni 53/80/443. |
| `PUT /kling/forwards` `{"ports": [8080, ...]}` | Abre un puerto en `127.0.0.1` (elegido por el sistema) por cada puerto del invitado y contesta `{"forwards": {"8080": "127.0.0.1:61234", ...}}`. Se puede llamar en cualquier momento tras crear la red; repetir un puerto devuelve la misma dirección. |
| `GET /kling/probe?port=N` | `{"open": bool}`: si algo acepta conexiones en ese puerto DENTRO del invitado (conecta como un reenvío: el 8080 por vsock si la máquina lo lleva, y si no por la pila de usuario, que espera ≤1 s al SYN). El reenvío no sirve para saberlo: el puerto de loopback lo abre el ayudante y acepta siempre. El núcleo lo usa para esperar al agente, `wait_ms` y `probe_only`. |
| `GET /kling/stats` | `{"footprint_mib": N}`: memoria que ocupa esta máquina en el host, sumando el `phys_footprint` del ayudante y del proceso auxiliar `com.apple.Virtualization.VirtualMachine` que la aloja. Es lo que en Linux es el RSS de firecracker. |
| `PUT /kling/cpu` `{"pct": N}` | Techo de CPU de la máquina en porcentaje de un núcleo (0 lo quita): lo que en Linux es el cgroup. El núcleo lo manda tras abrir los reenvíos, con `cpu_pct` o el valor por defecto. Mismo significado que `cpu.max`: % de un núcleo para toda la VM, sumen lo que sumen sus vCPU. `kling-vz` lleva un cubo de fichas con la CPU del auxiliar de Apple y, cuando se vacía, para el auxiliar con SIGSTOP 20 ms (lo manda el proceso freno, `kling-vz --cpu-brake`, lanzado antes del sandbox y encerrado en uno que solo deja señalizar; solo toca al auxiliar que tiene la tubería de consola que se le pasa). El primer `PUT /kling/cpu` sin techo efectivo (0 o todas las vCPU, lo normal en macOS) suelta el freno: un proceso menos por máquina; un techo que llegara después regula con pausas del framework. Si el freno falla, pausa la VM por el framework, que para también el reloj del invitado. Hasta que el agente escucha, el techo es `max(pct, 100)`. Ver `vz/internal/server/cpu.go`. `GET /kling/cpu` da `{"pct", "throttled_ms"}`. |

### Por qué puertos en loopback y no la IP del invitado

En Linux cada invitado es `172.16.0.2` dentro de su namespace, y el host lo
alcanza por la IP del veth (`Machine.IP`). En el Mac no hay namespaces: todos los
invitados tienen la misma IP y viven en redes de espacio de usuario separadas. El
host los alcanza por los puertos que abre su ayudante en `127.0.0.1`, que el núcleo
guarda en `Machine.Forwards` y resuelve con `Machine.Addr(puerto)`. Quien hoy
construye `mc.IP + ":" + puerto` (el proxy del daemon, exec, shell, el scheduler y
kindling-mcp) pasa a usar `Addr`, que en Linux sigue devolviendo `IP:puerto`.

En `127.0.0.1` de un Mac puede conectar cualquier usuario del Mac, y detrás del
puerto 8080 está el agente del invitado, con exec y ficheros. Por eso `kling-vz`
solo acepta en un reenvío las conexiones que abre un proceso **de su mismo
usuario**: macOS no da las credenciales del otro extremo de un socket TCP, así que
busca con `libproc`, entre los procesos de su usuario, cuál tiene abierto ese
socket (mismas direcciones y puertos en los dos extremos, IPv4), y si ninguno lo
tiene corta la conexión (`vz/internal/peercred`). Cuesta
~0,15 ms la primera vez, ~5 µs después (recuerda los últimos procesos dueños,
normalmente el daemon) y ~2,5 ms en el peor caso con 800 procesos.

Los puertos que se reenvían son `api.GuestPort` más los de la etiqueta
`kling.ports`. Se piden tras cada arranque o descongelación, porque un proceso
nuevo abre puertos nuevos.

### El canal con el agente: vsock

Las máquinas que ARRANCAN con un `kling-vz` nuevo llevan un
`VZVirtioSocketDevice` y `kling.vsock=1` en la línea de comandos; el agente
(`pkg/guest.ServeVsock`) escucha entonces también por `AF_VSOCK` en el 8080,
antes de abrir su TCP. Por fuera nada cambia: el núcleo sigue usando el reenvío
de loopback y `/kling/probe`. Por dentro, cada conexión al reenvío del 8080 (y la
sonda del 8080) prueba primero `connect` por vsock (≤500 ms) y, si falla, la
pila de usuario como siempre. Los demás puertos (`kling.ports`) van por TCP.

- **Agente anterior** (imágenes construidas antes): ignora `kling.vsock`
  (aviso en la consola) y solo escucha TCP. Cuando vsock falla y TCP contesta,
  `kling-vz` deja de probar vsock en esa máquina, y lo apunta en el snapshot
  (`agent_tcp_only`) para no pagar en cada descongelación el connect fallido
  (~6 ms medidos en el `/resync`).
- **`kling-vz` anterior con un agente nuevo**: sin `kling.vsock`, el agente no
  abre vsock; todo por TCP como antes.
- **Snapshots**: `ValidateSaveRestoreSupport` acepta la configuración con
  vsock en el M4 (macOS 26) y guardar/restaurar funciona. El framework reinicia
  el dispositivo al restaurar (las conexiones vivas mueren; el listener del
  invitado sigue), y como cada conexión al reenvío abre una nueva, no hay nada
  que reconectar. El snapshot dice si la máquina lleva vsock (`"vsock": true`,
  formato `kling_vz` 3): uno guardado sin él se restaura sin él, y un
  `kling-vz` anterior se niega a restaurar uno con él (`kling upgrade
  -rollback` lo comprueba antes).
- `KLING_VZ_VSOCK=0` en el entorno del daemon arranca las máquinas sin vsock.

El reenvío de loopback del 8080 **sigue haciendo falta**: lo usan, además del
daemon, el CLI (`kling` con `Machine.Addr`), el scheduler del gateway y
kindling-mcp, que no hablan con `kling-vz`. Por eso sigue el filtro por usuario
(`peercred`). Lo que cambia es el tramo de dentro: el agente ya no depende de la
red del invitado ni de la pila de usuario (comprobado: tras `ip link set eth0 down`
en el invitado, `kling exec` sigue funcionando; con TCP se queda sin respuesta).

Cifras (M4, `kling-vz` aislado con `KLING_VZ_MTU=0`, n=300 por medida): una
conexión nueva al agente cuesta ~0,4 ms por vsock frente a ~0,22 ms por la pila
de usuario (el `connect` de vsock es una ida y vuelta por XPC al auxiliar de
Apple); por una conexión ya abierta (keep-alive, lo que hace el daemon) y en un
`/exec/stream` de `true`, lo mismo (~0,1 y ~0,22 ms). En `kling exec`, `kling
run`, thaw y `/resync` la diferencia queda dentro del ruido del CLI y del
arranque.

## 4. La red de cada máquina

Cada `kling-vz` monta una red de espacio de usuario propia
(`VZFileHandleNetworkDeviceAttachment` + pila TCP/IP en el proceso) que imita la
del namespace de Linux, para que un snapshot sirva igual en los dos sistemas:

- invitado `172.16.0.2/30`, pasarela `172.16.0.1`, MAC `06:00:AC:10:00:02` (la
  que manda el núcleo), sin DHCP (el `ip=` de la línea de comandos la fija);
- MTU 16384 en las máquinas que arrancan (`KLING_VZ_MTU`, de 1500 a 65535; `0`
  = 1500): `kling-vz` la pone en el adjunto del framework
  (`setMaximumTransmissionUnit`), que la anuncia al invitado por
  `VIRTIO_NET_F_MTU` y el `virtio_net` de Linux la adopta solo; la pila de
  usuario usa la misma, y las dos viajan en el snapshot (`"mtu"`), así que uno
  guardado con 1500 se restaura con 1500. Hacia internet no cambia nada: la pila
  termina el TCP del invitado y el socket del Mac segmenta con la MTU de su red.
  Caudal invitado↔host por un reenvío (256 MiB, M4): ~600→~1900 MiB/s de bajada
  y ~950→~2650 de subida. No es 65535 porque el framework no ofrece
  `VIRTIO_NET_F_MRG_RXBUF` y el invitado reserva cada búfer de recepción del
  tamaño de la MTU: con 65535 son ~17 MiB de RAM del invitado por máquina, con
  16384 ~4,5, y el caudal es el mismo (9000, 16384, 32768 y 65535 dentro del
  ruido);
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
| jailer, bajada de privilegios | el invitado vive en el proceso auxiliar de Apple; `kling-vz` se encierra en su perfil de sandbox (`kling-vz.sb`) al crear la VM, con la raíz que el daemon le pasa en `KLING_VZ_CONFINE_ROOT` y solo los ficheros de esa VM (`SECURITY.md` §22) |
| RSS de `/proc/<pid>` | `GET /kling/stats` |
| clon del overlay con `cp --reflink` | `clonefile` de APFS (`cp -c`) |
| admisión por PSI (`/proc/pressure/memory`) | presión de memoria del sistema (`kern.memorystatus_level`) |
| barrido de huérfanos por `/proc` | por la tabla de procesos del sistema |
| raíz `/var/lib/kindling`, daemon como root | raíz en el directorio del usuario, daemon sin root |
| `mkfs.ext4`, `debugfs` de e2fsprogs | los mismos, de Homebrew (`brew install e2fsprogs`) |
| construir imágenes (`POST /images`) | no disponible (501): se construyen en Linux y se copian (`kling image copy`, `GET/PUT /images/{name}/blob`) |
| arranques simultáneos: 2 anidado, núcleos/2 en hierro | 4 (`KLING_MAX_PARALLEL_BOOT`) |
