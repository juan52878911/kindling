# kindling nativo en macOS (backend `vz`)

Desde v0.9 el daemon corre **directamente en un Mac con Apple Silicon**, sin VM
Linux de por medio. En lugar de Firecracker (que es KVM, y KVM es Linux) lanza un
**`kling-vz`** por microVM: un ayudante que habla el mismo API que Firecracker y
por debajo usa Virtualization.framework. El contrato entre los dos está en
[`backend-vz.md`](backend-vz.md); los números que justifican el diseño, en
[`vz-mac-prototipo.md`](vz-mac-prototipo.md).

Lo que ya funcionaba en el Mac sigue igual: el CLI puede seguir hablando con un
host Linux por SSH (`kling context add`), y la receta con Lima y virtualización
anidada de [`mac-arm64.md`](mac-arm64.md) sigue siendo válida. Esto es una tercera
opción: el runtime en el propio Mac.

## Requisitos

| Qué | Por qué |
|---|---|
| Apple Silicon (arm64) | los invitados son aarch64; en un Mac Intel no hay backend `vz` |
| macOS 14 (Sonoma) o superior; 15.2 para `kling-hv` | congelar y descongelar usa el guardado y restauración de estado de la VM, que llega en 14; `kling-hv` usa el GIC de Hypervisor.framework (ver la tabla de abajo) |
| `kling-vz` firmado con `com.apple.security.virtualization` | sin ese permiso macOS no deja crear la VM; una firma ad-hoc basta |
| e2fsprogs (`brew install e2fsprogs`) | cada microVM lleva un overlay ext4 y las imágenes se inspeccionan con `debugfs` |

No hace falta root: el daemon corre como tu usuario.

`kling up -check` comprueba todo lo anterior y dice qué teclear para cada fallo.

### Qué VMM según la versión de macOS

Hay dos VMM: `kling-vz` (Virtualization.framework) y `kling-hv`
(Hypervisor.framework, [`backend-hv.md`](backend-hv.md)). La versión se compara
entera (15.1 no llega a 15.2):

| macOS | VMM |
|---|---|
| 13 o anterior | ninguno |
| 14.x (Sonoma) | `vz` |
| 15.0 – 15.1 | `vz` |
| 15.2 o posterior | `hv` (con `daemon.vmm auto`, si `kling-hv` pasa su sonda) |

`kling-vz` se enlaza para macOS 14.0 y `kling-hv` para 15.2
(`MACOSX_DEPLOYMENT_TARGET` en `make vz` y en la release: sin fijarlo el binario
pide la versión del SDK con que se compiló y dyld no lo carga en un macOS
anterior).

Por qué 15.2 y no 15.0: la API del GIC de Hypervisor.framework (`hv_gic_create`,
`hv_gic_state_*`) es de macOS 15.0 según el SDK, y `kling-hv` no usa nada más
nuevo (lo de 15.2 es SME, que no toca). Pero `vz/internal/hvvm` se compila con
`-mmacosx-version-min=15.2` y nadie lo ha probado en 15.0 ni 15.1; bajar el
mínimo no gana casi a nadie (15.2 salió en diciembre de 2024) y arriesga un GIC
sin probar. Bajarlo es cambiar ese flag, `macOSMinHV` en
`cmd/kling/version_macos.go` y el objetivo de `make vz`/release, tras probarlo
en un Mac con 15.0.

## Instalación

Con el instalador de la release (la línea de `curl` del [README](../README.md)),
`kling`, `kling-vz` y `kling-hv` quedan en el mismo directorio y sin la
cuarentena de la descarga (van firmados ad-hoc, sin notarizar). Desde el repo:

```sh
make vz                 # compila y firma kling-vz y kling-hv (módulo aparte en vz/)
make install            # el CLI (y el daemon: es el mismo binario) y los VMM de vz/bin
brew install e2fsprogs
kling up -check
```

Con **Homebrew**: cada release adjunta su fórmula (`kling.rb`, con la versión y
el sha256 de cada binario). Instala los mismos binarios firmados (no compila
nada), depende de e2fsprogs y trae un servicio que corre `kling daemon` como el
agente de `kling up -install` (no instales los dos):

```sh
curl -fsSLO https://github.com/juan52878911/kindling/releases/latest/download/kling.rb
brew install --formula ./kling.rb
brew services start kling     # o: kling up -install, sin brew services
```

Así instalado, se actualiza con `brew upgrade kling`; `kling upgrade` lo detecta
(binarios en el `Cellar`) y lo dice en vez de pisarlos.

**El kernel y `min` vienen con la release** (desde v0.20): `kling up` los baja
si faltan en `images/` (`vmlinux-arm64.zst` y `min-arm64.ext4.zst`),
verificados contra el `SHA256SUMS` de la release de ese `kling`, igual que
`install.sh`. Ya no hace falta un host Linux para el primer `kling run -image
min`; para el resto de imágenes, ver [Conseguir imágenes](#conseguir-imágenes).

`kling-vz` vive en este repositorio, en `vz/`, como **módulo Go aparte**
(`github.com/juan52878911/kindling/vz`): necesita cgo y dependencias que el
núcleo no tiene. Por ser otro módulo, `go test ./...` desde la raíz no entra ahí.

El daemon busca `kling-vz` **junto al ejecutable de `kling`** y, si no, en el
`PATH`. Para usar otro, `KLING_VMM=/ruta/a/kling-vz`.

e2fsprogs de Homebrew es "keg-only" (no se enlaza al `PATH`); no importa: kling
busca `mkfs.ext4`, `debugfs`, `e2fsck` y `resize2fs` también en
`/opt/homebrew/opt/e2fsprogs` y `/usr/local/opt/e2fsprogs`.

### El backend es una opción de configuración

```sh
kling config set daemon.vmm auto   # "auto", "hv", "vz" o "firecracker"
kling config show                  # sin valor muestra el de la plataforma
```

Sin valor, el daemon usa el de su plataforma: `auto` en macOS, `firecracker` en
Linux. Se valida al escribirla y al arrancar el daemon: `auto`, `hv` y `vz`
exigen macOS en Apple Silicon y `firecracker` exige Linux con KVM, y el error
dice cuál vale aquí. `KLING_VMM` la sustituye sin tocar el fichero: con un
nombre (`auto`, `hv`, `vz`, `firecracker`) elige el backend; con una ruta, el
binario.

Con `auto` el daemon elige **al arrancar**, una vez, y no cambia hasta que se
reinicia:

1. `hv` si macOS es 15.2 o posterior (la tabla de arriba), `kling-hv` está
   junto a `kling` o en el `PATH`, va firmado con
   `com.apple.security.hypervisor` y su sonda (`kling-hv --probe`: crea y
   destruye una VM vacía con su GIC, menos de 10 ms) contesta `ok`;
2. si no, `vz`.

La elección y su motivo salen en el log (`vmm: hv (auto: macOS 26.5,
/Users/…/kling-hv signed and probed)`), en `GET /info` (`backend_reason`) y en
`kling status -v` (`backend: hv (auto: …)`); `kling up -check` hace la misma
elección y, si se queda en `vz`, avisa de por qué. `daemon.vmm hv` o `vz`
explícitos no sondean nada: `hv` falla al arrancar la primera microVM si
`kling-hv` no vale aquí. Para probar la caída a `vz` en un Mac donde `hv`
funciona, `KLING_HV_PROBE_FAIL=1` da la sonda por fallida.

Los dorados son de un VMM: al pasar de `vz` a `hv` (o al revés) los que había
salen obsoletos y hay que rehacerlos (`kling status` lo dice con la orden).

## Dónde vive

| | macOS | Linux |
|---|---|---|
| raíz de datos | `~/Library/Application Support/kindling` | `/var/lib/kindling` |
| socket | `~/Library/Application Support/kindling/kling.sock` | `/run/kling.sock` |
| usuario del daemon | tú | root (el VMM baja a `kindling`) |

`KLING_ROOT` y `KLING_SOCKET` (o `-root` y `-socket`) siguen mandando. El socket
por defecto no depende de `KLING_ROOT`, para que `kling` sin `-H` lo encuentre
arranque el daemon con la raíz que arranque.

## Conseguir imágenes

Una imagen que solo existe para amd64 se importa **emulada** con qemu-user
(`kling image import -arch amd64 <ref>` o `kling run -platform linux/amd64`):
ver [`imagenes-amd64.md`](imagenes-amd64.md).

**Imágenes de Docker**: `kling image import <ref>` (y `-archive`, y los
registros privados de `kling registry login`) funciona en el Mac: su
constructor es Go puro, sin root ni loop. Mete en la imagen el agente del
invitado, que es un binario de **Linux**: `make install` lo compila y lo deja en
`lib/` de la raíz de datos (`~/Library/Application Support/kindling/lib/kling-guest`),
donde lo busca, y `kling upgrade` lo cambia con lo demás. Para usar otro,
`KLING_GUEST_AGENT=/ruta` (o `make guest GOARCH=arm64` y apuntarlo ahí). Un
puerto de `EXPOSE` no se publica solo en el host: el gateway o `-label
kling.ports=N` (ver [`docker.md`](docker.md)).

Los constructores `debian` y `oci` (y `android`) también corren en el Mac: son Go
puro, y el daemon los ejecuta como `kling builder <nombre>`. El resto de imágenes
(`min`, MCP, chispa..., de los constructores de shell) **no se construyen en macOS**:
necesitan root, dispositivos loop y chroot de Linux. `POST /images` contesta 501
y dice qué hacer. Se construyen en un host Linux y se **copian**:

```sh
kling image copy min  -from ssh://juan@lab-arm64
kling image copy fetch -from ssh://juan@lab-arm64     # una imagen por capas
```

`copy` mueve lo necesario para que `kling run -image <nombre>` funcione en el
destino: el **kernel** (`vmlinux`), la **base** si la imagen va por capas (antes
que la capa), la **receta** y la imagen o capa. Lo que el destino ya tiene
idéntico (mismo sha256) no se vuelve a mandar. Va en flujo de un daemon a otro,
sin pasar por el disco del Mac, y el destino verifica el sha256 antes de
renombrar; nunca sustituye una imagen en uso por un contenido distinto.

`-to` es opcional: por defecto, el daemon de siempre (`-H`, `$KLING_HOST`, el
contexto activo, `$KLING_SOCKET` o el socket local). Si tienes un contexto activo que apunta al
host Linux, di el destino explícitamente o desactívalo:

```sh
kling image copy min -from ssh://juan@lab-arm64 -to "unix://$HOME/Library/Application Support/kindling/kling.sock"
# o
kling context use -
```

**El origen tiene que ser arm64.** Una imagen y un kernel son de una
arquitectura; `copy` compara la de los dos daemons y se niega antes de mover
nada. Sirve la VM Lima de [`mac-arm64.md`](mac-arm64.md), un servidor arm64 o
cualquier Linux arm64 con KVM.

## Arrancar el daemon al iniciar sesión (launchd)

A mano basta con `kling daemon` en una terminal. Para que arranque solo:

```sh
kling up -install
```

comprueba lo de `kling up -check`, escribe el agente de launchd
`~/Library/LaunchAgents/dev.kindling.daemon.plist` (el `kling` que lo ejecuta,
con rutas absolutas; `KLING_ROOT`, `KLING_SOCKET`, `KLING_VMM` y `KLING_CONFIG` si están
puestos, porque launchd no hereda la shell) y lo arranca con `launchctl
bootstrap`. Si el plist ya existe igual, no lo toca; si es distinto, no lo pisa
y dice cómo quitarlo. `KLING_LAUNCH_AGENT_LABEL` y `KLING_LAUNCH_AGENT_PLIST`
cambian la etiqueta y la ruta (el log va entonces junto al plist): sirven para
probarlo con un daemon privado sin tocar el agente de verdad.

Lo que escribe es esto (launchd no expande `~` ni `$HOME`: rutas absolutas):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.kindling.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/TU_USUARIO/.local/bin/kling</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <!-- SIGTERM al pararlo: el daemon escribe su estado y sale. -->
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>/Users/TU_USUARIO/Library/Logs/kindling.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/TU_USUARIO/Library/Logs/kindling.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.kindling.daemon.plist
launchctl kickstart -k gui/$(id -u)/dev.kindling.daemon      # reiniciarlo
launchctl bootout gui/$(id -u)/dev.kindling.daemon           # pararlo
```

Con el plist instalado, `kling up` lo arranca si no está corriendo.

Con el plist instalado, `kling upgrade` también actualiza el daemon: baja la
release, verifica `kling`, `kling-vz` y `kling-hv` (y que cada VMM lleva el
permiso de su framework), los cambia en su sitio (si junto a `kling` está
`kling-vz` pero no `kling-hv`, lo instala; volver atrás lo quita), lo reinicia con `launchctl bootout` y
`bootstrap` y vuelve atrás solo si no contesta con la versión nueva
([`actualizar.md`](actualizar.md) §3.4). Sin el agente de launchd no sabe
reiniciarlo: `kling upgrade -cli` cambia solo el binario. Lo prueba de punta a
punta `scripts/95-e2e-upgrade-mac.sh`, con un agente suyo para un daemon privado.

## Cómo se llega a cada máquina

En Linux cada invitado es `172.16.0.2` dentro de su namespace y el host lo
alcanza por la IP de su veth. En el Mac no hay namespaces: todos los invitados
tienen la misma IP, cada uno en la red de espacio de usuario de su `kling-vz`, y
el host llega a ellos por **puertos de loopback** que abre su ayudante. Se
guardan en la máquina (`forwards` en `GET /machines/{ref}`) y `kling ps` los
enseña en vez de la IP:

```
running  127.0.0.1:61234
```

Se reenvían el puerto del agente (8080) y los de la etiqueta `kling.ports`. Se
piden tras cada arranque y cada descongelación. El proxy del daemon, exec, shell
y `pkg/scheduler` ya los usan (`api.Machine.Addr`); quien construya
`IP + ":" + puerto` a mano no llegará en macOS.

Solo los procesos de tu usuario pasan por esos puertos: otra cuenta del Mac que
conecte a `127.0.0.1:<puerto>` ve la conexión cerrada sin llegar al invitado (ver
[`backend-vz.md`](backend-vz.md#por-qué-puertos-en-loopback-y-no-la-ip-del-invitado)).

## Servicios del Mac desde el invitado: `-host-port`

Lo contrario: que el invitado llegue a un servicio que corre en el Mac. El caso
es un modelo local con Metal (Ollama, `llama-server`), que dentro de una VM no
tendría GPU. En vez de una GPU virtual, un puente a ese puerto:

```
ollama serve                                   # escucha en 127.0.0.1:11434
kling run -name agente -allow-exec -host-port 11434
kling exec agente -- wget -qO- http://host.kindling.internal:11434/api/tags
```

- `-host-port PUERTO` deja el `127.0.0.1:PUERTO` del Mac en `172.16.0.1:PUERTO`
  del invitado (la pasarela de su red); `-host-port HOST:GUEST` lo pone en otro
  (`-host-port 8080:18080`: el `llama-server` del 8080 en el 18080).
  `host.kindling.internal` resuelve a `172.16.0.1` en el DNS de la máquina, y
  solo si tiene algún puente. Se repite (o lleva comas), hasta 16.
- Solo TCP y solo los puertos listados. Lo fija quien crea la máquina (`kling
  run`, `POST /machines` con `host_ports`); nada del invitado lo pide ni lo
  cambia. Viaja con la máquina: un freeze/thaw o un stop/start lo rehacen. No
  pasa a sus snapshots ni a sus forks.
- **Egress**: abre esos puertos de la pasarela en todos los modos, también con
  `-egress none`, y nada más; la política no cambia (una máquina `none` con
  `-host-port 11434` sigue sin poder salir a ninguna otra parte, ni a otro
  puerto del Mac). Sin la opción el loopback del Mac no se alcanza por ningún
  camino: `127.0.0.1` es el del invitado y la pasarela no reenvía al Mac.
- Cada conexión la marca el daemon, no el VMM: `kling-hv` y `kling-vz` corren
  confinados y su perfil de sandbox no abre el loopback. La red de la máquina
  la pide por el broker de enlaces (como las aristas de los grafos y el
  `-upstream 127.0.0.1` del proxy de credenciales), y el daemon comprueba que
  ese puerto es de esa máquina. Queda una línea por conexión en su registro de
  auditoría (`kling machine audit`, `kind: link`, `host.kindling.internal:PUERTO`)
  y, mientras dura, cuenta como uso: una máquina con una petición al modelo en
  curso no es ociosa para la [presión de memoria](#presión-de-memoria).
- No valen como puerto del invitado el 53, el 80 ni el 443 (DNS y proxy de
  credenciales en la pasarela) y, en el Mac, ninguno del rango de reenvíos
  (29000-29999), donde están los invitados de las demás máquinas. El 8080
  (el de `llama-server`) sí vale.
- El servicio tiene que escuchar en `127.0.0.1` (o en todas): lo que solo
  escuche en otra interfaz no se alcanza. Si no escucha, el invitado recibe un
  RST, como contra un puerto cerrado.

Un `state.json` con alguna máquina con puentes se escribe con el esquema 4: un
kling anterior se niega a arrancar sobre él en vez de perderlos.

En Linux no hay puente (el invitado vive en un namespace y llegar al loopback
del host pediría un proxy por puerto dentro de él): `-host-port` da error.

## Energía y batería

En un portátil cada despertar de la CPU cuesta batería, también con las
máquinas quietas. Lo que gasta kindling con todo ocioso, medido en un M4 con
`top -stats pid,command,cpu,idlew,csw,power` (cambios de contexto por segundo,
la medida más estable con el Mac ocupado; redis 7 sin clientes, 1 vCPU):

| proceso | estado | antes | ahora |
|---|---|---|---|
| `kling-vz` | running o pausada | ~65 csw/s, POWER 0,1-0,2 | 0,1-2,6 csw/s, POWER 0,0 |
| auxiliar de Apple | running | ~75-100 csw/s, POWER 0,5 | igual (es el invitado: ver abajo) |
| auxiliar de Apple | pausada o congelada | 0 | 0 |
| daemon | con máquinas, enchufado | `ps` cada 10 s (fork+exec, ~40 ms) | sysctl, ~3 ms cada 10 s |
| daemon | en batería | — | ~0 csw/s (vuelta cada 30 s) |

Qué despertaba y qué se hace:

- **El regulador de CPU de `kling-vz`** medía cada 100 ms aunque no hubiera
  techo (en vz no lo hay por defecto) y también con la VM en pausa: ahora
  espera a que algo cambie (techo, estado de la VM) sin despertar. Con techo y
  la VM ociosa el sueño entre medidas crece hasta 500 ms (antes 50-100 ms); lo
  que una VM que despierta gaste de más se cobra como deuda, así que la media
  se cumple. Tampoco se pregunta al agente cada 20 ms en el arranque si no hay
  techo.
- **El registro de auditoría del proxy de credenciales** tenía un ticker de 1 s
  en cada `kling-vz`, con o sin credenciales: ahora solo se arma con algo
  pendiente.
- **El vigilante del daemon** (cada 10 s) lanzaba `ps -axww` para saber qué
  `kling-vz` siguen vivos: ahora lee la tabla de procesos con `proc_listpids` y
  `kern.procargs2`, solo los de tu usuario.
- **El invitado**: con `HZ=100` y `NO_HZ_IDLE` (lo que ya tenía el núcleo) un
  invitado sin carga despierta su vCPU ~7 veces por segundo; dos sysctl en la
  línea de órdenes del núcleo (`vm.compaction_proactiveness=0`,
  `vm.stat_interval=10`) lo bajan a ~3,7. Lo demás es de la imagen: redis
  despierta 10 veces por segundo por su cuenta (`hz 10`) y, con ello, RCU otras
  ~20; eso no lo arregla el núcleo, lo arregla pausarla.

### Modo batería

```
kling config set daemon.battery_mode auto              # auto (defecto), on, off
kling config set daemon.battery_pause_after_seconds 60 # 0 = 60; -1 = no pausar
```

`KLING_BATTERY_MODE` y `KLING_BATTERY_PAUSE_AFTER` mandan sobre el fichero; se
leen al arrancar el daemon. En `auto` el daemon se entera de si el Mac va con
batería por el vigía de energía (`kling-vz --power-events` o `kling-hv
--power-events`, el mismo que avisa del sueño), que lo dice al arrancar y en
cada cambio de corriente a batería o al revés (`IOPSCreateLimitedPowerNotification`):
el cambio vale en el acto y no se lanza ningún proceso. Si el vigía no arranca
o es de un VMM anterior que no lo dice, pregunta él a `pmset -g ps`, como mucho
una vez por minuto (sin cgo no hay `IOPSCopyPowerSourcesInfo`). Con batería:

1. **Baja la QoS** (`PRIO_DARWIN_BG`, lo de `taskpolicy -b`: núcleos de
   eficiencia, E/S en segundo plano) del `kling-vz` y del auxiliar de Apple de
   cada máquina ociosa, y se la devuelve en cuanto llega una petición.
2. **Pausa** (no congela) la que lleva `battery_pause_after_seconds` ociosa. En
   pausa el auxiliar no gasta nada; despertarla es reanudar (~50 ms medido el
   primer `exec`). Exec, shell, ficheros y las llamadas al invitado
   (`POST /machines/{ref}/guest`) la despiertan solos. Al volver al enchufe se
   reanudan las que pausó este modo (no las que pausaste tú).
3. **Espacia el vigilante**: una vuelta cada 30 s en vez de cada 10, y cada
   60 s si no corre ninguna máquina (las pausadas y congeladas no gastan ni
   tienen QoS que vigilar). Sus vueltas caen siempre en la misma rejilla del
   reloj (múltiplos de su intervalo), así que las de varios daemons despiertan
   al Mac a la vez en lugar de repartirse por el minuto.

Una petición que llega justo mientras el modo batería pausa una máquina no se
queda colgada: al terminar la pausa el daemon mira si entró alguna y, si es
así, la reanuda en el acto.

Ociosa es: ninguna petición en curso contra ella (un `kling shell` abierto, un
`exec` largo, `kling logs -f`) ni terminada hace menos de ese plazo, y su
invitado gastando menos del 2 % de un núcleo entre dos vueltas (`cpu_ms` de
`GET /kling/cpu`). Nunca se pausan las que alguien puede usar sin pasar por el
daemon: las que publican puertos (`kling.ports`), las del planificador y los
fondos (`kling ai serve`, `template pool`), las de un grafo, las de `kling db`
y las que tienen carpetas vivas. El planificador ya duerme las suyas con su
propio plazo.

### Al dormir el Mac

Con el Mac dormido no corre nada, tampoco los invitados, y su reloj (el
contador de ARM no avanza dormido) se quedaba atrás todo lo que durmió. El
daemon lo detecta sin notificaciones del sistema: el reloj monotónico no cuenta
el sueño y el de pared sí. Al despertar, en la siguiente vuelta del vigilante o
en la primera petición contra una máquina (lo que llegue antes), pone en hora
las que corren (`POST /resync` del agente, lo mismo que tras un thaw). Las
congeladas se ponen en hora al despertarlas, como siempre, y las pausadas al
reanudarlas si la pausa es de antes del sueño: reanudar una pausada es solo
`Resume`, sin resync, salvo entonces (antes se quedaban con la hora de cuando
se cerró la tapa, también las que el vigía pausa justo antes de dormir).

## Presión de memoria

Cuando macOS se queda sin memoria, jetsam mata procesos, y un VMM es de los
que más ocupan: la máquina moría sin aviso con lo que tuviera sin guardar. La
admisión ya decía que no a arrancar otra con poca memoria; ahora el daemon
también hace sitio con las que corren. En cada vuelta del vigilante (cada
10 s; 30 s en batería) lee `kern.memorystatus_vm_pressure_level` (un sysctl,
sin cgo ni procesos: 1 normal, 2 aviso, 4 crítico) y:

- **en aviso**, aprieta el globo (`kling machine squeeze`) de las máquinas ociosas:
  devuelve al Mac la RAM que su invitado tiene libre o en caché, sin parar
  nada;
- **en crítico**, además congela (`kling freeze`) la ociosa que más ocupa: su
  RAM va a disco y la siguiente petición la despierta.

```
kling config set daemon.memory_pressure auto   # auto (defecto) u off
```

`KLING_MEMORY_PRESSURE` manda sobre el fichero; se lee al arrancar el daemon.

Y antes de arrancar, un techo a lo **prometido**: la suma de `mem_mib` (o
`mem_max_mib`) de las máquinas que corren o están en pausa, más las que están
arrancando, no pasa de 4 veces la RAM física. Una VM del Mac ocupa solo lo que
su invitado toca, así que sin esto siempre parece que cabe otra, hasta que los
invitados usan lo suyo. Si no cabe, el daemon congela ociosas y reintenta, y
si aun así no, contesta 507. `KLING_MEM_OVERCOMMIT` (en el entorno del daemon)
cambia el factor; `0` lo apaga. En Linux no aplica: el cgroup de cada VM ya
la acota.

Ociosa es lo del modo batería, más estricto: ninguna petición en curso (exec,
shell, ficheros, `logs -f`) ni terminada hace menos de 2 minutos (ni arrancada
hace menos), ninguna sesión del broker de aristas, y su VMM gastando menos del
2 % de un núcleo entre dos vueltas. Nunca se tocan las que alguien puede usar
sin pasar por el daemon (puertos publicados, servicios del planificador,
fondos, grafos, `kling db`, carpetas vivas): esas las duerme
`daemon.idle_freeze` o el planificador con su propio plazo. Las copias de un
dorado que comparten su memoria no se aprietan (subiría la memoria del host,
ver `kling machine squeeze`), pero sí se pueden congelar. Antes de congelar se vuelve a
mirar con el cerrojo de la máquina tomado: si entre medias le llegó una
petición, no se toca.

Primero las de más huella y, a igualdad, las que llevan más sin uso. Con freno
para no apretar en bucle mientras el nivel no baja:

| freno | valor |
|---|---|
| por vuelta que actúa | 2 apretones y 1 congelación |
| entre dos vueltas que actúan | 30 s (lo que tarda el nivel en notar lo devuelto) |
| antes de volver a apretar la misma máquina | 10 min (también si la apretó `makeRoom` al arrancar otra) |
| para dar el nivel por bajado | 30 s seguidos por debajo; subir es inmediato |

La primera vuelta con presión solo toma la referencia de CPU de cada VMM, así
que lo primero se hace a los ~20 s. Cada cambio de nivel y cada acción quedan
en el log del daemon (`memory pressure: warn (was normal)`, `memory pressure
(warn): squeezed idle web, 312 MiB back to the host …`), el apretón y la
congelación salen en el bus de eventos como cualquier otro (`machine.frozen`)
y `/metrics` lleva `kling_memory_pressure_level`,
`kling_memory_pressure_actions_total{action="squeeze"|"freeze"}` y
`kling_memory_pressure_reclaimed_mib_total`.

En Linux se hace lo mismo con PSI (`some avg10` de `/proc/pressure/memory`):
aviso desde el 10 %, crítico desde el 40 % (por encima del 20 % con el que la
admisión deja de arrancar máquinas). Sin PSI en el núcleo no hay señal y no se
hace nada.

## Qué gasta cada máquina: `kling top` y `/metrics`

En macOS no hay `/proc` ni PSS. `kling top` enseña la memoria de cada máquina
(la huella, `phys_footprint` del VMM, del auxiliar de Apple con vz y de sus
hijos: lo que Monitor de Actividad llama memoria), su %CPU entre dos fotos (en
una foto suelta toma dos a un segundo) y, en Apple Silicon, qué parte corrió en
núcleos de rendimiento:

```
$ kling top            # M4, kling-hv, un redis recién arrancado y otro congelado
ID             SERVICE   STATE     MEM      CPU    ON P-CORES
e526ecd3a3da   —         running   96 MiB   1.3%   47%

1 alive · 1 frozen · total memory 96 MiB
host: ~4915 MiB available of 16384 MiB (macOS memory level)
```

Lo lee el daemon sin lanzar procesos: `proc_pid_rusage` (v6) de cada VMM y de
su auxiliar da la CPU total, la de núcleos P y la energía que macOS le
atribuye. `/metrics` añade las mismas cifras:

| serie | qué es |
|---|---|
| `kling_machine_footprint_mib{id,name,state}` | la huella de cada máquina viva |
| `kling_machine_cpu_seconds_total{id,name,core}` | CPU desde que se lanzó su VMM; `core="p"` y `"e"` en Apple Silicon, `"all"` en Linux |
| `kling_machine_energy_joules_total{id,name}` | energía de sus procesos (`ri_energy_nj`), en julios |
| `kling_machine_state_seconds{id,name,state}` | cuánto lleva en su estado, también las congeladas y paradas |
| `kling_machine_wakeups_total{id,name,kind}` | despertares (`ri_pkg_idle_wkups`, `ri_interrupt_wkups`), los de `powermetrics --show-process-wakeups` sin sudo; el daemon tiene los suyos en `kling_daemon_wakeups_total` |
| `kling_machine_disk_bytes_total{id,name,op}` · `kling_machine_cpu_throttled_seconds_total{id,name}` | disco real (`ri_diskio_*`) y lo frenado por el regulador de CPU |
| `kling_daemon_footprint_mib` · `kling_battery_on` | la huella del daemon y si el modo batería está en vigor; el resto, en [api.md](api.md#métricas-de-recursos) |
| `kling_host_mem_available_mib` | lo que el host aún puede dar: `kern.memorystatus_level` sobre `hw.memsize` en macOS, `MemAvailable` en Linux |
| `kling_memory_pressure_level` | el nivel de presión con el que actúa el daemon (1, 2, 4; 0 = no se sabe o apagado): ver [Presión de memoria](#presión-de-memoria) |
| `kling_memory_pressure_actions_total{action}` | apretones y congelaciones por presión |

El banco de energía reproducible (`scripts/94-energia-mac.sh`, sin sudo) y lo
medido están en [benchmarks.md](benchmarks.md#energy-on-the-mac).

## Límites frente a Linux

| | Linux (firecracker) | macOS (vz) | macOS (hv) |
|---|---|---|---|
| memoria de una restauración | se comparte el `mem.file` del dorado por copia en escritura: una copia más cuesta lo que diverge | **~350 MiB por VM**: el framework copia el estado a memoria al restaurar; la densidad de dorados es mucho menor | como Firecracker: la RAM es un `mmap(MAP_PRIVATE)` del `mem.file` del dorado, ~16 MiB por copia en el M4 ([backend-hv.md](backend-hv.md)) |
| construir imágenes | sí (`kling image build`, `toolchain`) | con los constructores sin root (`debian`, `oci`, `android`, y `kling image import`); las de los constructores de shell se copian (`kling image copy`) | como vz (lo hace el daemon) |
| capas con `verity` (constructores `debian`, `android`) | con un núcleo con `CONFIG_DM_VERITY` | igual, con el mismo `Image` arm64 de `scripts/builders/kernel` o el de Android: probado en un M4 (montada por `/dev/mapper`, `sha256-ce`, un byte cambiado para el arranque; [imagenes.md](imagenes.md#verity-en-kling-vz-mac-m4-2026-10-01)). Con un núcleo sin device-mapper la capa no se monta y el invitado entra en pánico; en vz eso es un bucle de reinicios con la máquina `running`, no `failed` | el mismo núcleo, con la línea de comandos intacta; sin probar con hv |
| `kling image put`, `mcp refresh-bridge` | montan la imagen con un loop | sin montar, con `debugfs -w` (e2fsprogs de Homebrew): mismo contrato (modo pedido, dueño root, copia al lado y cambio de entrada solo con la copia comprobada, crece la imagen si no cabe, e2fsck antes y después). Los enlaces simbólicos de los directorios se siguen siempre dentro de la imagen. Una ruta con comillas, barras invertidas o caracteres de control se rechaza | como vz (lo hace el daemon) |
| techo de CPU por microVM (`-cpu-pct`) | cgroup v2 | lo aplica `kling-vz`, con el mismo significado que `cpu.max`: % de UN núcleo para la VM entera (2 vCPU con `-cpu-pct 50` gastan medio núcleo entre las dos; `cpu_pct_per_vcpu` de la receta es por vCPU). No hay forma de tocar un hilo de vCPU suelto (el auxiliar de Apple no da su puerto de tarea) y ni el nice ni `taskpolicy -b` son un techo, así que se para el auxiliar entero: un cubo de fichas que, al vaciarse, lo para con SIGSTOP 20 ms (un proceso freno aparte, en su propio sandbox, manda las señales). El invitado ve pasar ese tiempo, como bajo `cpu.max`; pausar la VM por el framework, lo de antes, le paraba también el reloj (23 s de retraso en 30 s regulado). Medido en un M4 con `yes` dentro: el techo se cumple (50 % con `-cpu-pct 50`, 1 y 2 vCPU) y `kling exec true` pasa de p99 117-324 ms a 34-78 ms. Hasta que el agente escucha, el techo es `max(pct, 100)`, como el impulso de arranque de Linux. Cuesta un proceso más por VM (~8 MiB) **Por defecto, sin techo** (todas las vCPU) si la máquina no pide `cpu_pct`: regular una VM de vz multiplica la pérdida de páginas de RAM del invitado que hace macOS bajo presión (#87, `prototypes/android/docs/sigill.md`); en Linux el defecto sigue siendo media vCPU | lo aplica `kling-hv` sacando los vCPU a un punto seguro, con el mismo significado y el reloj del invitado corriendo. **Por defecto, sin techo**, como vz; aquí la RAM es un mapeo de un fichero inmutable y la pérdida de páginas de #87 no se ha visto ([estabilidad](backend-hv.md#estabilidad-de-kling-hv)) |
| jailer, bajada de privilegios | sí | el invitado vive en el proceso auxiliar de Apple, y `kling-vz` (que procesa su tráfico de red) se encierra en un perfil de sandbox al crear la VM: su directorio y los ficheros exactos de su VM (kernel, discos, el estado que restaura), nada de `secrets/` ni de otras máquinas; los dorados los publica el daemon; y sale a la red solo si la máquina tiene egress, nunca a los reenvíos de otras máquinas. `KLING_VZ_NO_SANDBOX=1` lo apaga para diagnosticar | no hay auxiliar: la VM vive en `kling-hv`, que se encierra en `kling-hv.sb` al crear o restaurar (el mismo reparto que `kling-vz`, sin `mach-lookup` ni IOKit) |
| admisión por memoria | PSI (`KLING_MAX_MEM_PRESSURE`) | `kern.memorystatus_level`: por debajo del 15 % libre, 507 (`KLING_MIN_MEM_LEVEL`, 0 lo apaga) | como vz |
| arranques simultáneos | 2 anidado, núcleos/2 (hasta 8) en hierro; restaurar y arrancar en frío comparten la puerta | dos compuertas: **3 restauraciones** (`thaw`, `run -from`; cada una copia su estado a memoria, ~324 MiB en el M4) y **4 arranques en frío**, que ya no esperan detrás de las restauraciones. Con `kern.memorystatus_level` en el 25 % o menos, una restauración espera hasta 2 s a que suba y luego se restaura de una en una. Reanudar una pausada no pasa por ninguna. `KLING_MAX_PARALLEL_BOOT` fija las dos. El prototipo vio fallos con ~20 restauraciones a la vez | **8 restauraciones** a la vez (no copian la memoria); el resto como vz |
| primer `exec` tras un arranque en frío | sondeo con dial a la IP | `GET /kling/probe` a `kling-vz` en intentos de 150 ms: un SYN mandado antes de que suba la red del invitado se pierde y la pila de usuario no lo repite hasta 1 s, justo el plazo del sondeo, que contestaba "cerrado" aunque el agente ya escuchara. Medido en el M4 (redis, 256 MiB): `run` + primer `exec` de 1,37–1,41 s a 0,52–0,59 s, y `run -wait-ready` de 1,19 s a 0,54 s. Lo que queda (~350 ms desde que `run` vuelve) es el arranque del núcleo invitado y del agente | como vz, por vsock primero |
| nivel pausada (`kling ai serve`) | `-paused-mib 256` | sin `-paused-mib`, derivado de la memoria del host en cada vuelta y 0 con el nivel de memoria en el 25 % o menos (ver [despertar.md](despertar.md#cómo-está-hecho)); antes de pausar se infla el globo y se deja inflado (`KLING_SQUEEZE_BEFORE_PAUSE=0` lo apaga) | como vz |
| `squeeze` | reclama lo libre del invitado, según sus estadísticas | el framework no da estadísticas del invitado: aprieta a ciegas hasta dejarle la mitad de su memoria (nunca menos de 128 MiB) y espera a que la huella deje de bajar. Si bajó, **el globo se queda inflado**: al desinflarlo el framework repuebla las páginas y la huella vuelve entera en segundos; `deflate_on_oom` se lo devuelve al invitado si lo necesita. Solo devuelve memoria en máquinas **restauradas** (`thaw`, `run -from`): en una arrancada en frío el framework no suelta las páginas del globo (medido en macOS 26.5) y el globo vuelve a la línea base | reclama lo libre de verdad: el globo tiene cola de estadísticas (`GET /balloon/statistics`); soltar una página es volver a mapear su tramo del fichero de RAM |
| memoria por VM (`kling top`, `/procstats`) | PSS de firecracker | `phys_footprint` del ayudante y del proceso auxiliar (`GET /kling/stats`); CPU por núcleo P/E y energía con `proc_pid_rusage` | `phys_footprint` del propio `kling-hv`, sin viaje HTTP |
| red | netns + veth + iptables | red de espacio de usuario en cada `kling-vz`, misma semántica de `egress` | como vz (la misma `vnet`) |
| carpetas compartidas (`-share`) | copia con `mkfs.ext4`; vivas con un techo de 256 MiB/s por sentido por el limitador de red (una máquina sin carpetas se queda en `daemon.net_mibps`, 16 por defecto) | copia con el `mke2fs` que haya (Homebrew o android-platform-tools; tiene que aceptar `-d`); vivas servidas desde APFS, que por defecto no distingue mayúsculas (`A` y `a` son el mismo fichero para el invitado), sin limitador de red. Ver [compartir.md](compartir.md) | como vz |
| ruta del socket de cada VMM | holgada | puede pasar de los 104 bytes de `sun_path` con la raíz en `Application Support`: `kling-vz` se ata con `chdir` y el daemon conecta por un enlace corto en `/tmp/kling-<uid>/`. El cliente hace lo mismo con un `unix://` largo (`-H`, `$KLING_HOST`, contexto) y `dial-stdio` con su `KLING_SOCKET` | como vz |

Lo que `kling-hv` no tiene frente a `kling-vz`: **hasta 8 vCPU y 8 discos**
por máquina (`MAX_CPUS` y `KHV_MAX_DISKS`), ni pantalla ni ventana
(`KLING_VZ_GRAPHICS`, `KLING_VZ_WINDOW` y `KLING_VZ_DISK_*` llegan a su entorno
y no hacen nada). Y si `daemon.vmm auto` vuelve a vz (la sonda falla, falta
`kling-hv`), lo que congeló hv queda `stale` hasta que vuelva hv. Los detalles,
en [backend-hv.md](backend-hv.md#estabilidad-de-kling-hv).
