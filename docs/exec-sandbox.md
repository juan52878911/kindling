# Exec y sandboxes

Un agente de código escribe un script y necesita ejecutarlo sin tocar tu máquina.
kindling le da una microVM de usar y tirar: arranca en milisegundos desde un
snapshot, no tiene red salvo que se pida, ejecuta lo que se le mande con la salida
en streaming, y se destruye sola al vencer su tiempo de vida.

```sh
kling images toolchain                          # una imagen con node, npm, python3 y pip
kling sandbox create -image toolchain -name sb  # ~5 s en frío
kling cp ./analisis.py sb:/tmp/
kling exec sb -- python3 /tmp/analisis.py       # la salida llega según sale
kling cp sb:/tmp/resultado.json .
kling sandbox rm sb
```

## La puerta: allow_exec

Ejecutar comandos y tocar ficheros dentro de una microVM exige que se creara con
`allow_exec`. No es un permiso que se conceda después:

- viaja en la línea de comandos del kernel (`kling.exec=1`), que solo escribe el
  host, así que el invitado no puede dárselo a sí mismo;
- sin él, las rutas `/exec` y `/files` del agente no existen, ni siquiera
  desactivadas;
- se congela con la memoria: las máquinas restauradas de un snapshot con
  `allow_exec` la tienen siempre, y un snapshot sin ella no puede dar sandboxes
  (`409`).

Por eso una microVM de servicio —las que despierta el gateway de kindling-mcp— no
ejecuta nada aunque alguien alcance su puerto.

`kling sandbox create` la enciende siempre. Para una máquina normal:
`kling run -allow-exec`.

## Sandboxes

Un sandbox es una máquina con `allow_exec`, sin red por defecto
(`-egress internet` o `-egress allowlist -allow dominio` si hace falta), con la
etiqueta `kind=sandbox` y que **se destruye** al vencer su TTL en vez de
congelarse (por defecto 10 minutos, máximo 24 horas).

| | |
|---|---|
| `kling sandbox create -image I` | arranque en frío, unos segundos |
| `kling sandbox create -from S` | restaurado de un snapshot con exec: ~300 ms, con el estado de la plantilla |
| `kling sandbox ls` | los vivos y cuánto les queda |
| `kling sandbox renew sb -ttl 30m` | vence 30 minutos a partir de ahora |
| `kling sandbox rm sb` | destruir ya |

La imagen tiene que llevar el agente de invitado (`kling-guest`): la de
`kling images toolchain`, o cualquiera construida con
`kling images build -builder base`. El daemon lo comprueba antes de arrancar.

### Plantillas: preparar una vez, restaurar muchas

Instalar dependencias en cada sandbox es lento. Se prepara una máquina, se
congela como snapshot y los sandboxes nacen de ella:

```sh
kling run -image toolchain -name plantilla -allow-exec -egress internet -mem 1024
kling exec plantilla -- npm install -g typescript
kling commit plantilla ts
kling sandbox create -from ts -name sb            # ~300 ms, tsc ya dentro
kling cp ./a.ts sb:/tmp/
kling exec sb -- sh -c 'cd /tmp && tsc a.ts && node a.js'
```

Cada sandbox restaurado despierta con la hora del host y su propia entropía,
no con las de la plantilla: el daemon las resincroniza antes de devolverlo (ver
[api.md](api.md#tras-restaurar-reloj-y-entropía-del-invitado)). Lo que un
proceso de la plantilla ya hubiera sacado de su generador en espacio de usuario
—el pool de OpenSSL de un node ya arrancado, por ejemplo— sí se comparte:
arranca esos procesos después de restaurar si necesitan aleatorios propios.

Medido en el laboratorio (Lima arm64 con virtualización anidada):

| | |
|---|---|
| sandbox en frío sobre `toolchain` | 5,6 s |
| sandbox desde una plantilla | 0,13–0,32 s |
| cinco sandboxes desde la misma plantilla, en paralelo | 0,57 s |

La red de la plantilla no pasa a los sandboxes que se piden sin ella: cada sandbox
elige su egress (sin red por defecto).

## kling exec

```sh
kling exec [-i] [-e K=V] [-w DIR] [-timeout 5m] [-max-output N] <máquina> [--] <cmd> [args...]
```

- `cmd` es argv, sin shell. Para tuberías: `kling exec sb -- sh -c '...'`.
- stdout y stderr llegan por separado y según salen; también a través de SSH.
- kling termina con el **código del comando remoto**: se puede usar en scripts.
- `-i` manda la entrada estándar (hasta 1 MiB; para más, `kling cp`).
- `-timeout` mata el comando y a todos sus hijos al vencer (por defecto 5 min,
  máximo 1 h). El código es 137, como en una shell.
- La salida se corta a 8 MiB por flujo (`-max-output`, máximo 64 MiB). El proceso
  no muere por eso: se descarta lo que sobra y kling avisa.
- Una máquina congelada se descongela sola; una recién arrancada se espera hasta
  que su agente escucha.

## kling shell

```sh
kling shell [-e K=V] [-w DIR] [-t TERM] <máquina|sandbox> [--] [cmd args...]
```

Una terminal de verdad dentro de la microVM: `vim`, edición de línea, historial,
colores y control de trabajos. Sin argumentos abre `/bin/sh -l`.

- **Ctrl-C interrumpe lo de dentro, no la sesión.** El CLI apaga las señales de
  su propia terminal, así que el `^C` viaja como un byte y es el pseudoterminal
  remoto quien genera la interrupción sobre el proceso en primer plano, como en
  `ssh`.
- **Redimensionar la ventana llega al programa de dentro** (`SIGWINCH`).
- `kling` termina con el código de la shell remota.
- Sin terminal en la entrada y la salida se niega y remite a `kling exec -i`: una
  shell sin terminal no es una shell.
- Misma puerta que exec: solo en máquinas creadas con `allow_exec`.

Por dentro no es una respuesta en streaming sino un cambio de protocolo: la
petición pide `Upgrade`, el daemon contesta 101 y la conexión pasa a transportar
tramas en los dos sentidos, como hace `docker exec -it`. Se eligió así porque los
plazos de lectura del daemon y del agente cortarían una sesión larga, y porque el
túnel SSH cierra el socket entero en cuanto una de sus dos direcciones termina.

El límite es de 8 sesiones simultáneas por microVM. Al colgar, el agente cierra el
pseudoterminal, lo que manda `SIGHUP` a la sesión, y dos segundos después se lleva
lo que siga vivo.

## kling cp

```sh
kling cp [-mode 0755] <local|-> <máquina>:<ruta>
kling cp <máquina>:<ruta> <local|->
```

Un fichero por llamada (hasta 64 MiB de subida y 256 MiB de bajada). Para un
directorio, copiar un tar y deshacerlo con `kling exec`. La escritura es atómica
(se escribe al lado y se renombra) y el último componente de la ruta no se sigue si
es un enlace simbólico.

## API

Las rutas, los límites y el formato del flujo están en [`api.md`](api.md#exec-y-ficheros).
Desde Go, `pkg/api`: `Client.Exec`, `ReadFile`, `WriteFile`, `StatFile`,
`RemoveFile`, `CreateSandbox`, `Sandboxes`, `RenewSandbox`, `RemoveSandbox`.

## Bajo carga

Medido en el laboratorio (Lima arm64 anidado, 6 vCPU, 8 GiB), con 24 sandboxes de
una plantilla y 60 ejecuciones a la vez:

| | |
|---|---|
| Crear, con el fondo de precalentadas vacío | p50 422 ms, p95 15,6 s |
| Ejecutar, 60 a la vez sobre 24 sandboxes | p50 3,4 s, p95 12,3 s |
| Despertar, 24 a la vez | p50 333 ms, p95 373 ms |

Las colas largas son del anfitrión, no del daemon: seis vCPU repartidos entre
decenas de microVMs que arrancan a la vez. Lo que importa es que no hubo ni una
ejecución perdida ni un proceso huérfano.

**kindling sobreasigna memoria a propósito.** Una microVM no toca toda la RAM que
declara, y de ahí sale la densidad. La consecuencia es que en un host saturado el
rechazo puede no llegar como "no cabe" sino como "el invitado no llegó a
escuchar": el arranque se comió su plazo compitiendo por CPU. El mensaje lo dice y
sugiere mirar `kling top`.

## Lo que hay que saber

- **El TTL no espera a que acabe un comando.** Un sandbox de 10 minutos que ejecuta
  algo de 20 se destruye a los 10. Renuévalo antes, o créalo con más TTL.
- **Dentro se es root.** La máquina es de usar y tirar y la frontera es el
  hipervisor, no los permisos de dentro.
- **Imágenes anteriores a v0.7** llevan un agente sin exec en streaming, ficheros
  ni shell: el daemon contesta `501` y dice que se reconstruya la imagen.
- **La shell abre un pseudoterminal**, y para eso hace falta `devpts` montado en
  el invitado. El init de las imágenes de kindling no lo monta, así que lo monta
  el agente la primera vez: no hay que reconstruir nada por este motivo.
- `kling volume populate` sigue usando la ruta de ejecución agregada de siempre,
  que también entienden los agentes antiguos.
