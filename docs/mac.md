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
kling config set daemon.vmm vz     # "vz" o "firecracker"
kling config show                  # sin valor muestra el de la plataforma
```

Sin valor, el daemon usa el de su plataforma: `vz` en macOS, `firecracker` en
Linux. Se valida al escribirla y al arrancar el daemon: `vz` exige macOS en Apple
Silicon y `firecracker` exige Linux con KVM, y el error dice cuál vale aquí.
`KLING_VMM` la sustituye sin tocar el fichero: con un nombre (`vz`,
`firecracker`) elige el backend; con una ruta, el binario.

`kling status -v` dice qué backend usa un daemon (`backend: vz`) y su arquitectura.

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

**Imágenes de Docker**: `kling image import <ref>` (y `-archive`, y los
registros privados de `kling registry login`) funciona en el Mac: su
constructor es Go puro, sin root ni loop. Mete en la imagen el agente del
invitado, que es un binario de **Linux**: `make install` lo compila y lo deja en
`lib/` de la raíz de datos (`~/Library/Application Support/kindling/lib/kling-guest`),
donde lo busca, y `kling upgrade` lo cambia con lo demás. Para usar otro,
`KLING_GUEST_AGENT=/ruta` (o `make guest GOARCH=arm64` y apuntarlo ahí). Un
puerto de `EXPOSE` no se publica solo en el host: el gateway o `-label
kling.ports=N` (ver [`docker.md`](docker.md)).

El resto de imágenes (`min`, MCP, chispa...) **no se construyen en macOS**:
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
contexto activo o el socket local). Si tienes un contexto activo que apunta al
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

A mano basta con `kling daemon` en una terminal. Para que arranque solo, un
agente de launchd en `~/Library/LaunchAgents/dev.kindling.daemon.plist`
(launchd no expande `~` ni `$HOME`: rutas absolutas):

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
leen al arrancar el daemon. En `auto` el daemon pregunta a `pmset -g ps` si el
Mac va con batería, como mucho una vez por minuto (sin cgo no hay
`IOPSCopyPowerSourcesInfo`). Con batería:

1. **Baja la QoS** (`PRIO_DARWIN_BG`, lo de `taskpolicy -b`: núcleos de
   eficiencia, E/S en segundo plano) del `kling-vz` y del auxiliar de Apple de
   cada máquina ociosa, y se la devuelve en cuanto llega una petición.
2. **Pausa** (no congela) la que lleva `battery_pause_after_seconds` ociosa. En
   pausa el auxiliar no gasta nada; despertarla es reanudar (~50 ms medido el
   primer `exec`). Exec, shell, ficheros y las llamadas al invitado
   (`POST /machines/{ref}/guest`) la despiertan solos. Al volver al enchufe se
   reanudan las que pausó este modo (no las que pausaste tú).
3. **Espacia el vigilante**: una vuelta cada 30 s en vez de cada 10.

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
pausadas y congeladas se ponen en hora al despertarlas, como siempre.

## Límites frente a Linux

| | Linux (firecracker) | macOS (vz) |
|---|---|---|
| memoria de una restauración | se comparte el `mem.file` del dorado por copia en escritura: una copia más cuesta lo que diverge | **~350 MiB por VM**: el framework copia el estado a memoria al restaurar; la densidad de dorados es mucho menor |
| construir imágenes | sí (`kling image build`, `toolchain`) | **no**: se copian (`kling image copy`) |
| capas con `verity` (constructores `debian`, `android`) | con un núcleo con `CONFIG_DM_VERITY` | igual, con el mismo `Image` arm64 de `scripts/builders/kernel` o el de Android: probado en un M4 (montada por `/dev/mapper`, `sha256-ce`, un byte cambiado para el arranque; [imagenes.md](imagenes.md#verity-en-kling-vz-mac-m4-2026-10-01)). Con un núcleo sin device-mapper la capa no se monta y el invitado entra en pánico; en vz eso es un bucle de reinicios con la máquina `running`, no `failed` |
| `kling image put`, `mcp refresh-bridge` | montan la imagen con un loop | sin montar, con `debugfs -w` (e2fsprogs de Homebrew): mismo contrato (modo pedido, dueño root, copia al lado y cambio de entrada solo con la copia comprobada, crece la imagen si no cabe, e2fsck antes y después). Los enlaces simbólicos de los directorios se siguen siempre dentro de la imagen. Una ruta con comillas, barras invertidas o caracteres de control se rechaza |
| techo de CPU por microVM (`-cpu-pct`) | cgroup v2 | lo aplica `kling-vz`, con el mismo significado que `cpu.max`: % de UN núcleo para la VM entera (2 vCPU con `-cpu-pct 50` gastan medio núcleo entre las dos; `cpu_pct_per_vcpu` de la receta es por vCPU). No hay forma de tocar un hilo de vCPU suelto (el auxiliar de Apple no da su puerto de tarea) y ni el nice ni `taskpolicy -b` son un techo, así que se para el auxiliar entero: un cubo de fichas que, al vaciarse, lo para con SIGSTOP 20 ms (un proceso freno aparte, en su propio sandbox, manda las señales). El invitado ve pasar ese tiempo, como bajo `cpu.max`; pausar la VM por el framework, lo de antes, le paraba también el reloj (23 s de retraso en 30 s regulado). Medido en un M4 con `yes` dentro: el techo se cumple (50 % con `-cpu-pct 50`, 1 y 2 vCPU) y `kling exec true` pasa de p99 117-324 ms a 34-78 ms. Hasta que el agente escucha, el techo es `max(pct, 100)`, como el impulso de arranque de Linux. Cuesta un proceso más por VM (~8 MiB) **Por defecto, sin techo** (todas las vCPU) si la máquina no pide `cpu_pct`: regular una VM de vz multiplica la pérdida de páginas de RAM del invitado que hace macOS bajo presión (#87, `prototypes/android/docs/sigill.md`); en Linux el defecto sigue siendo media vCPU |
| jailer, bajada de privilegios | sí | el invitado vive en el proceso auxiliar de Apple, y `kling-vz` (que procesa su tráfico de red) se encierra en un perfil de sandbox al crear la VM: su directorio y los ficheros exactos de su VM (kernel, discos, el estado que restaura), nada de `secrets/` ni de otras máquinas; los dorados los coloca un proceso custodio aparte; y sale a la red solo si la máquina tiene egress, nunca a los reenvíos de otras máquinas. `KLING_VZ_NO_SANDBOX=1` lo apaga para diagnosticar |
| admisión por memoria | PSI (`KLING_MAX_MEM_PRESSURE`) | `kern.memorystatus_level`: por debajo del 15 % libre, 507 (`KLING_MIN_MEM_LEVEL`, 0 lo apaga) |
| arranques simultáneos | 2 anidado, núcleos/2 (hasta 8) en hierro; restaurar y arrancar en frío comparten la puerta | dos compuertas: **3 restauraciones** (`thaw`, `run -from`; cada una copia su estado a memoria, ~324 MiB en el M4) y **4 arranques en frío**, que ya no esperan detrás de las restauraciones. Con `kern.memorystatus_level` en el 25 % o menos, una restauración espera hasta 2 s a que suba y luego se restaura de una en una. Reanudar una pausada no pasa por ninguna. `KLING_MAX_PARALLEL_BOOT` fija las dos. El prototipo vio fallos con ~20 restauraciones a la vez |
| primer `exec` tras un arranque en frío | sondeo con dial a la IP | `GET /kling/probe` a `kling-vz` en intentos de 150 ms: un SYN mandado antes de que suba la red del invitado se pierde y la pila de usuario no lo repite hasta 1 s, justo el plazo del sondeo, que contestaba "cerrado" aunque el agente ya escuchara. Medido en el M4 (redis, 256 MiB): `run` + primer `exec` de 1,37–1,41 s a 0,52–0,59 s, y `run -wait-ready` de 1,19 s a 0,54 s. Lo que queda (~350 ms desde que `run` vuelve) es el arranque del núcleo invitado y del agente |
| nivel pausada (`kling ai serve`) | `-paused-mib 256` | sin `-paused-mib`, derivado de la memoria del host en cada vuelta y 0 con el nivel de memoria en el 25 % o menos (ver [despertar.md](despertar.md#cómo-está-hecho)); antes de pausar se infla el globo y se deja inflado (`KLING_SQUEEZE_BEFORE_PAUSE=0` lo apaga) |
| `squeeze` | reclama lo libre del invitado, según sus estadísticas | el framework no da estadísticas del invitado: aprieta a ciegas hasta dejarle la mitad de su memoria (nunca menos de 128 MiB) y espera a que la huella deje de bajar. Si bajó, **el globo se queda inflado**: al desinflarlo el framework repuebla las páginas y la huella vuelve entera en segundos; `deflate_on_oom` se lo devuelve al invitado si lo necesita. Solo devuelve memoria en máquinas **restauradas** (`thaw`, `run -from`): en una arrancada en frío el framework no suelta las páginas del globo (medido en macOS 26.5) y el globo vuelve a la línea base |
| memoria por VM (`kling top`, `/procstats`) | PSS de firecracker | `phys_footprint` del ayudante y del proceso auxiliar (`GET /kling/stats`) |
| red | netns + veth + iptables | red de espacio de usuario en cada `kling-vz`, misma semántica de `egress` |
| carpetas compartidas (`-share`) | copia con `mkfs.ext4`; vivas con un techo de 16 MiB/s por el limitador de red | copia con el `mke2fs` que haya (Homebrew o android-platform-tools; tiene que aceptar `-d`); vivas servidas desde APFS, que por defecto no distingue mayúsculas (`A` y `a` son el mismo fichero para el invitado), sin limitador de red. Ver [compartir.md](compartir.md) |
| ruta del socket de cada VMM | holgada | puede pasar de los 104 bytes de `sun_path` con la raíz en `Application Support`: `kling-vz` se ata con `chdir` y el daemon conecta por un enlace corto en `/tmp/kling-<uid>/`. El cliente hace lo mismo con un `unix://` largo (`-H`, `$KLING_HOST`, contexto) y `dial-stdio` con su `KLING_SOCKET` |
