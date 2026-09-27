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
| macOS 14 (Sonoma) o superior | congelar y descongelar usa el guardado y restauración de estado de la VM, que llega en 14 |
| `kling-vz` firmado con `com.apple.security.virtualization` | sin ese permiso macOS no deja crear la VM; una firma ad-hoc basta |
| e2fsprogs (`brew install e2fsprogs`) | cada microVM lleva un overlay ext4 y las imágenes se inspeccionan con `debugfs` |

No hace falta root: el daemon corre como tu usuario.

`kling up -check` comprueba todo lo anterior y dice qué teclear para cada fallo.

## Instalación

```sh
make install            # el CLI (y el daemon: es el mismo binario)
make vz                 # compila y firma kling-vz (módulo aparte en vz/)
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

En macOS **no se construyen imágenes**: construir necesita root, dispositivos
loop y chroot de Linux. `POST /images` contesta 501 y dice qué hacer. Se
construyen en un host Linux y se **copian**:

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

## Límites frente a Linux

| | Linux (firecracker) | macOS (vz) |
|---|---|---|
| memoria de una restauración | se comparte el `mem.file` del dorado por copia en escritura: una copia más cuesta lo que diverge | **~350 MiB por VM**: el framework copia el estado a memoria al restaurar; la densidad de dorados es mucho menor |
| construir imágenes | sí (`kling image build`, `toolchain`) | **no**: se copian (`kling image copy`) |
| `kling image put`, `mcp refresh-bridge` | sí | **no**: montan la imagen con un loop |
| techo de CPU por microVM (`-cpu-pct`) | cgroup v2 | **no se aplica**: no hay cgroups |
| jailer, bajada de privilegios | sí | **no**: el aislamiento es el proceso auxiliar de Apple que aloja cada VM |
| admisión por memoria | PSI (`KLING_MAX_MEM_PRESSURE`) | `kern.memorystatus_level`: por debajo del 15 % libre, 507 (`KLING_MIN_MEM_LEVEL`, 0 lo apaga) |
| arranques simultáneos | 2 anidado, núcleos/2 (hasta 8) en hierro | **4** (`KLING_MAX_PARALLEL_BOOT`): el prototipo vio fallos con ~20 restauraciones a la vez |
| `squeeze` | reclama lo libre del invitado, según sus estadísticas | el framework no da estadísticas del invitado: aprieta a ciegas hasta dejarle la mitad de su memoria (nunca menos de 128 MiB) y espera a que la huella deje de bajar. Si bajó, **el globo se queda inflado**: al desinflarlo el framework repuebla las páginas y la huella vuelve entera en segundos; `deflate_on_oom` se lo devuelve al invitado si lo necesita. Solo devuelve memoria en máquinas **restauradas** (`thaw`, `run -from`): en una arrancada en frío el framework no suelta las páginas del globo (medido en macOS 26.5) y el globo vuelve a la línea base |
| memoria por VM (`kling top`, `/procstats`) | PSS de firecracker | `phys_footprint` del ayudante y del proceso auxiliar (`GET /kling/stats`) |
| red | netns + veth + iptables | red de espacio de usuario en cada `kling-vz`, misma semántica de `egress` |
| carpetas compartidas (`-share`) | copia con `mkfs.ext4`; vivas con un techo de 16 MiB/s por el limitador de red | copia con el `mke2fs` que haya (Homebrew o android-platform-tools; tiene que aceptar `-d`); vivas servidas desde APFS, que por defecto no distingue mayúsculas (`A` y `a` son el mismo fichero para el invitado), sin limitador de red. Ver [compartir.md](compartir.md) |
| ruta del socket de cada VMM | holgada | puede pasar de los 104 bytes de `sun_path` con la raíz en `Application Support`: `kling-vz` se ata con `chdir` y el daemon conecta por un enlace corto en `/tmp/kling-<uid>/` |
