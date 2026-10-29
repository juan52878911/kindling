# Desarrollo local en el Mac: kindling en lugar de Docker Desktop

Levantar postgres, redis y la aplicación desde imágenes de Docker, con el
código del host montado en vivo, los puertos en `localhost` y el cambio visible
al guardar. Esta página dice cómo, cuánto cuesta frente a Docker Desktop
(medido en un M4) y qué falta.

## Cómo

```sh
kling config set daemon.share_roots ~/code        # qué carpetas se pueden montar en vivo
cd ~/code/mi-app
kling compose up                                  # compose.yaml, compose.yml, docker-compose.y(a)ml
```

`kling compose up` traduce el compose con [`graph from-compose`](grafos.md#desde-un-composeyaml),
importa las imágenes en paralelo, levanta el grafo y se queda en primer plano
(`-d`: lo deja levantado y vuelve). La equivalencia orden por orden con Docker,
y qué pasa con cada clave que aquí no existe, está en
[docker-a-kindling.md](docker-a-kindling.md).

- un **bind mount** de un directorio (`./src:/app`, `../app:/app:ro`) es una
  [carpeta compartida](compartir.md) viva `rw` (o `ro`) del nodo. Tiene que
  estar bajo `daemon.share_roots`. Un fichero suelto no se puede compartir (se
  avisa y se salta);
- los **`ports:`** (`"5432:5432"`, `"8080:80"`) se publican en `127.0.0.1` del
  host. Los publica el **daemon** (`POST /machines/{ref}/publish`, ver
  [api.md](api.md#puertos-publicados)): siguen ahí al cerrar el CLI y tras
  reiniciar el daemon, resuelven en cada conexión el reenvío de la máquina (en
  el Mac cambia con cada arranque o descongelación) y despiertan la máquina si
  está congelada. `"80"` sin puerto del host toma uno libre (`kling compose ps`
  dice cuál); un puerto ocupado falla antes de importar nada. Nunca en otras
  interfaces: Docker publica en todas por defecto (un `0.0.0.0:` se avisa);
- **la red del proyecto** es la de Docker Compose: cada servicio alcanza a
  todos los demás (una arista `link` a cada puerto de cada otro), no solo a los
  de `depends_on`, y un nombre a secas (`db`, `proxy_pass http://api:8081` en un
  `nginx.conf`) se resuelve como `db.graph`. `networks:` lo restringe a los que
  comparten red, y `network_mode: none` a ninguno. Un nodo alcanza **un** nodo
  por puerto: si dos exponen el mismo, gana el de `depends_on` (se avisa);
- `depends_on` con `condition: service_healthy` espera a que el `test` del
  `healthcheck:` del otro pase **dentro** de él, con su `interval`, `timeout`,
  `retries` y `start_period` (se intenta cada 200 ms; enfermo cuando lo diría
  Docker);
- la salida de cada servicio va junta, con el nombre delante (`app   | ...`);
  `-t` pone la hora de cada línea;
- **Ctrl-C** quita las máquinas y sus puertos; los volúmenes con nombre se
  quedan. `-keep` las deja corriendo (con sus puertos) y `kling compose down`
  las quita (`-v`, también los volúmenes);
- `kling compose ps`, `logs [-f] [servicio]`, `exec servicio cmd`, `stop`,
  `start` y `restart` hacen lo de Docker Compose, salvo que `stop` **congela**
  (libera la RAM; `start` despierta en milisegundos con su memoria) y
  `restart` arranca en frío sobre su disco;
- el **`.env`** del directorio del compose da las variables que la shell no
  tiene (la shell manda), con `$VAR`, `${VAR:-def}` y `${VAR:?msg}`, que falla
  como en Docker.

Un `up` de un proyecto que ya estaba lo sustituye. Un proyecto es de **un**
fichero: sus máquinas llevan `kling.compose.file` y un `up` o `down` desde
otro compose con el mismo nombre de proyecto se niega (dale otro con `-p`);
dos `up` del mismo proyecto a la vez, tampoco.

### Congelar lo que no se usa (`daemon.idle_freeze`)

Con puertos publicados, un proyecto que lleva `daemon.idle_freeze` (10 min por
defecto en el Mac; `0` = nunca, lo de Linux) sin conexiones por ellos, sin
peticiones al daemon contra sus máquinas (un `exec`, unos `logs -f`: el `up`
en primer plano cuenta) y sin sesiones por sus aristas se **congela** entero:
su RAM vuelve al Mac. La primera conexión a un puerto publicado despierta esa
máquina (las demás, cuando ella las llame por su arista). La etiqueta
`kling.idle=keep` en una máquina la excluye, y con ella a su grafo.

```sh
kling config set daemon.idle_freeze 30m     # o KLING_IDLE_FREEZE en el entorno del daemon
```

### Plantillas calientes

La primera vez, cada servicio se arranca suelto, se espera a que esté listo (la
sonda de su imagen y, si lo declara, su `healthcheck:`, que se **ejecuta dentro**
con `exec`) y se guarda como plantilla `<proyecto>-<nodo>-<clave>`. Los `up`
siguientes restauran los nodos de ahí: ni arranque en frío, ni initdb de
postgres, ni reimportar imágenes (la importación reconoce la misma receta y no
hace nada).

- La **clave** es un hash de la receta: imagen, `command:`, `entrypoint:`,
  `user:`, `restart:`, `environment:`, `cpus:`/`mem_limit:`, bind mounts,
  volúmenes, `healthcheck:`, y versión y VMM del daemon (pasar de `kling-vz` a
  `kling-hv` rehace las plantillas sin `-fresh`). Cambiar algo prepara otra
  plantilla solo para ese servicio y borra la vieja. Una etiqueta que se mueve
  en el registro no se ve (Docker tampoco vuelve a bajarla): `kling compose up
  -fresh` rehace las plantillas.
- **`environment:`, `command:` y `entrypoint:`** van en la máquina
  (`RunRequest.Env`, `Cmd` y `Entrypoint`, por MMDS), no en la imagen: cambiar
  una variable o el comando no reimporta, y la contraseña no queda en el disco
  de la imagen. El comando cuenta en la clave: se aplica al arrancar en frío, y
  la plantilla es la memoria de una máquina ya arrancada (o a punto de lanzar el
  servicio con lo que su agente leyó entonces), así que otro comando es otra
  plantilla, preparada en ~1 s sin reimportar.
- Un servicio con **bind mounts** se guarda con su agente esperando a sus
  carpetas y el servicio sin lanzar ([compartir.md](compartir.md#cambios-hechos-en-el-host)):
  al restaurar se conectan las carpetas y el servicio arranca con el código de
  ahora, no con el de cuando se guardó.
- Un servicio con **volúmenes con nombre** (el `pgdata:` de un postgres) se
  guarda con los volúmenes puestos pero **sin montar** y el servicio sin lanzar
  (`RunRequest.VolumeWait`, `kling.volwait=1`): congelarlo montado dejaría en la
  plantilla la caché de un disco que después cambia. Cada copia los monta al
  restaurarse y entonces arranca el servicio, sobre los datos de ahora (el
  initdb, la primera vez; luego, solo el arranque de postgres).
- Las copias de una plantilla tienen `exec` encendido (`kling exec <proyecto>-<nodo> -- sh`)
  y CPU entera, como un contenedor.

Con una sola máquina, sin compose: `kling run -image node:22-alpine -share
$PWD:/app:rw -p 3000:3000` pide al daemon `127.0.0.1:3000` y se queda en
primer plano con la salida del servicio. Ctrl-C deja de seguirla; la máquina
y su puerto se quedan (`kling rm` quita los dos).

### `exec` y los procesos en segundo plano

`kling exec x -- sh -c 'servidor &'` tarda **30 s** en volver: el exec acaba
cuando el comando sale *y* se cierran su stdout y su stderr, y el nieto los
hereda. Pasados 30 s el agente deja de esperar. Redirige su salida:
`sh -c 'servidor >/tmp/servidor.log 2>&1 </dev/null &'` vuelve al momento
(ver [api.md](api.md#exec-y-ficheros)).

## Lo que se ve al editar

Con un daemon y un agente que hablan `WATCH` ([compartir.md](compartir.md#cambios-hechos-en-el-host)):

- un cambio hecho en el host se ve en el invitado en milisegundos (antes, al
  caducar la caché de 1 s);
- los vigilantes del invitado (`node --watch`, nodemon, vite, webpack) se
  enteran: modificar, crear, borrar, `mkdir` y el guardado atómico de un editor
  (escribir aparte y renombrar encima) disparan inotify dentro.

Hay que reimportar la imagen para que lleve el agente nuevo
(`kling image import <ref> -replace`, o `compose up -fresh`, que ahora sí
reimporta).

## Medidas (Mac con M4, kling-vz, 2026-10-27)

Docker Desktop 29.6.1 en el mismo Mac. Desde nada a la primera respuesta en
`127.0.0.1` (imágenes ya bajadas; en kindling, ya importadas). «Repetido» es el
día a día: la plantilla ya existe.

| | kindling, en frío | kindling, repetido (plantilla) | Docker Desktop |
|---|---|---|---|
| redis:7-alpine hasta `PONG` | 1,4 s | 0,56-0,70 s | 0,96 s |
| postgres:16-alpine hasta aceptar el arranque de sesión | 5,0 s | 0,55-0,72 s | 1,5 s |
| app de Node con su código en carpeta viva | 1,4 s (antes 1,7: arrancaba sin la carpeta y se reiniciaba) | 0,46-0,67 s | 0,6 s |
| compose de 3 (app + postgres + redis), todo contestando | 8,6 s la primera vez (importa y prepara plantillas); 6,2 s con `-fresh` | 1,11-1,32 s (la app, la última, contesta a los 0,86-1,02 s) | 1,2 s |
| cambio de un fichero en el host leído por el servidor del invitado | 5 ms (p50; 9 ms máx.) | | 3 ms (31 ms máx.) |
| editar en el host → `node --watch` sirve lo nuevo | **254 ms** (antes: nunca) | | 257 ms |
| `npm install` de express (caché caliente) en la carpeta | 0,92 s (v0.20; con el agente anterior 0,89 s: lo domina el metadato, no el caudal) | | 0,77 s |
| `npm install` en el disco de la máquina | 0,49-0,82 s | | 0,55 s |
| escritura secuencial 1 MiB en la carpeta | **1,08 GB/s** (v0.20; antes 702 MB/s\*) | | 604 MB/s |
| escrituras de 4 KiB en la carpeta | **26 000/s** (v0.20; antes 6 900/s\*) | | 26 600/s |
| lectura secuencial en la carpeta, en frío | **1,15 GB/s** (v0.20; antes 560 MB/s\*) | | 3,2 GB/s |

\* Las filas de la carpeta, remedidas el 2026-10-28 (p50, n=10) con el daemon
de v0.20 en A/B, alternando en la misma tanda una máquina con el agente
anterior y otra con el nuevo (`node:22-alpine`, 2 vCPU, 1 GiB): 20 000
`write()` de 4 KiB y `fsync`; 200 MiB en trozos de 1 MiB y `fsync`; leerlos
tras `drop_caches`. La primera medida de esta tabla (3 060/s, 336 y 242 MB/s)
era con el daemon anterior y otra carga del Mac. Lo que cambia en v0.20: el
agente contesta cada `write()` en cuanto tiene los datos y junta los
contiguos (escritura diferida, [compartir.md](compartir.md#coherencia-y-límites)),
lee adelantado 4 MiB en vez de 128 KiB, y el daemon deja 8 operaciones
grandes en vuelo por sesión en vez de 4. En kling-hv, igual medido: 4 KiB
7 200 → 27 000/s, escritura 651 → 959 MB/s, lectura 532 → 967 MB/s, `npm
install` 0,96 → 0,90 s. `node --watch` sigue viendo un cambio del host en
~260 ms (p50, n=10, en los dos), y congelar y descongelar 10 veces con un
proceso escribiendo da 0 errores.

### Con volúmenes y la red del proyecto (M4, kling-vz, 2026-10-28)

Dos compose: `app` (Node, código en carpeta viva) + `db` (postgres:16-alpine
con `pgdata:` y `healthcheck`) + `cache` (redis:7-alpine), **sin
`depends_on`** (la app los alcanza por la red del proyecto); y `web` (nginx con
`./nginx:/etc/nginx/conf.d:ro` y `proxy_pass http://api:8081`) + `api` (Node).
Desde lanzar `kling compose up` hasta que cada servicio contesta por su puerto
publicado en `127.0.0.1` (redis `PONG`, postgres responde al `SSLRequest`, la
app sirve una petición que habla con los dos). n=10, p50 (mín–máx), con el
cerrojo del banco.

| | antes (2026-10-27) | ahora |
|---|---|---|
| postgres con `pgdata:`, `up` repetido | 5,0 s (en frío siempre: sin plantilla) | **0,94 s** (0,86–1,75) |
| app + postgres(`pgdata`) + redis, todo contestando, `up` repetido | no medido (postgres en frío, ~5 s) | **0,98 s** (0,90–1,80) |
| lo mismo con `app: depends_on: {db: {condition: service_healthy}}` | sonda TCP | 1,6 s (n=1): `pg_isready` pasa dentro de `db` y entonces se restaura la app |
| nginx + api, `/api` por nginx, `up` repetido | — (no arrancaba: `conf.d` bajo `/etc` y `api` sin resolver) | **0,59 s** (0,55–0,68) |
| `up -fresh` (reimporta las 3 imágenes y rehace las plantillas) | 6,2 s | 2,0–2,5 s (n=3) |
| `up` tras cambiar el `command:` de la app | reimportaba la imagen y rehacía la plantilla | 2,0–2,3 s (n=2): solo la plantilla de la app, sin reimportar |
| primera vez (imágenes ya bajadas a la caché de capas) | 8,6 s | 7,3 s (n=1) |

De nuevo el 2026-10-28 tras fusionar `hv-paridad`, `hv-distribucion`,
`compose-paridad`, `mac-huella` y `amd64-en-arm` (M4, daemon privado, sin
otros agentes midiendo, `up` repetido con la plantilla hecha; p50 / p90).
Cada servicio, desde lanzar `compose up` hasta que contesta por su puerto
publicado; las sondas van en paralelo. La app de prueba habla con `db` y
`cache` al recibir la petición, así que paga el SYN que el invitado reintenta
a 1 s si postgres aún no escuchaba (ver abajo): su cifra es ~1 s más que la de
`db`.

| | kling-hv | kling-vz |
|---|---|---|
| `cache` (redis) contesta `PONG` (n=60) | **0,29 s** / 0,64 | 0,94 s / 1,08 |
| `db` (postgres con `pgdata:`) responde al `SSLRequest` (n=60) | **0,36 s** / 0,76 | 1,01 s / 1,16 |
| `app` sirve una petición que habla con los dos (n=60) | 1,31 s / 1,61 | 1,96 s / 2,11 |
| nginx + api, `/api` por nginx (hv n=12, vz n=100) | **0,16 s** / 0,38 | 0,59 s / 0,87 |
| `up` colgados por una copia que no contesta | 0 de 135 | 4 de 275 antes del arreglo, 1 de 359 después (abajo) |

La red del proyecto no cuesta: el mismo compose con cada servicio en su red
(0 aristas `link` frente a 6) deja postgres en 0,99 s frente a 0,94 s (n=10):
por debajo del ruido, < 10 ms por arista.

El fallo intermitente de antes (en 3 de unos 45 `up` la copia de postgres no
contestaba a su agente tras restaurar y el `up` se deshacía) no es de
VolumeWait: es el vsock de `kling-vz`, que en algunas copias deja de entregar
al invitado (el agente sigue vivo por TCP; ver
[backend-vz.md](backend-vz.md#el-canal-con-el-agente-vsock)). VolumeWait solo
lo hacía fatal, porque `/volume/acquire` es obligatorio; en una copia sin
volúmenes se colgaban el `/resync`, el attach de la carpeta y la sonda de
listo. Medido el 2026-10-28 con kling-vz: 4 `up` colgados de 115 del compose
nginx + api (las dos con carpetas vivas) y 0 de 160 del de postgres; con
kling-hv, 0 de 135. Con la comprobación del canal (c46f0f8b) 0 de 150 del
de nginx + api y 0 de 60 del de postgres; en otra tanda de 149 con la misma
versión hubo 1 `up` colgado en el que el `/resync` sí contestó y lo que no
llegó fue la carpeta, sin que la comprobación viera el vsock mudo: queda
abierto. Fuera del grafo no se reproduce (60 restauraciones de la plantilla,
solas o de dos en dos). Una versión intermedia que solo comprobaba el canal
al conectar dejó 2 colgados en 349: el vsock enmudece también después de
contestar la primera petición, y por eso se vigila cada conexión.

### Puertos del daemon, `up -d` y congelar por ociosidad (M4, kling-hv, 2026-10-28)

El mismo compose de 3 (app + postgres con `pgdata:` y `healthcheck` + redis),
daemon privado con `kling-hv`, `up -d` repetido con las plantillas hechas y
`down` entre medias. Cada servicio, desde lanzar `compose up -d` hasta que
contesta por su puerto publicado (ahora por el **daemon**: el CLI ya ha
acabado); postgres, hasta que contesta al `StartupMessage` (la primera
respuesta de un `psql`). n=20 (n=15 el congelado), p50 / p90, con el cerrojo
del banco.

| | antes (`kling-hv` sin RST en las aristas) | ahora |
|---|---|---|
| `compose up -d` hasta que el CLI vuelve | 0,20 s / 0,24 | 0,20 s / 0,22 |
| `cache` (redis) contesta `PONG` | 0,21 s / 0,26 | 0,21 s / 0,23 |
| `db` contesta al arranque de sesión | 0,25 s / 0,31 | 0,25 s / 0,32 |
| `app` sirve una petición que habla con los dos | **2,24 s** / 2,28 | **0,25 s** / 0,31 |
| proyecto congelado (`compose stop`) → primera sesión de postgres | | **50 ms** / 61 (30–82) |
| congelado por `idle_freeze` (1 min sin conexiones) → primera sesión | | 145 ms (n=1) |

- La app de prueba conecta con `db` y `cache` en cuanto recibe la petición y
  da 503 si no contestan (el banco reintenta). Antes, una conexión a un
  servicio que aún no escuchaba se aceptaba y se cerraba sin más, y la app
  esperaba su plazo de 2 s; ahora recibe un RST en seguida (el reenvío de
  `kling-hv` corta con RST, y la arista lo pasa al invitado, al SYN si ya
  llegó o justo después) y reintenta. La cifra de 1,31 s de la tabla de
  arriba era la misma espera con otra app.
- Tras `up -d` el CLI acaba y `psql` sigue conectando; tras reiniciar el
  daemon los puertos vuelven a estar publicados (con las máquinas vivas o
  congeladas).
- Congelar por ociosidad despierta solo la máquina a la que se conecta (la
  app y redis siguen congeladas hasta que alguien las llame).

Los tiempos de kindling incluyen el CLI (`kling run`/`compose up` es un proceso
nuevo cada vez) y la sonda. En el arranque en frío, ~0,7 s de los 1,4 s de la
app son la primera conexión del host al agente por el reenvío de `kling-vz`,
antes de poder conectar la carpeta.

Lo que pesa en kindling: cada operación de metadatos de la carpeta (crear,
`stat`, abrir, cerrar) es un viaje de ida y vuelta al daemon (~0,3 ms) por el
reenvío de `kling-vz`; las escrituras, desde v0.20, ya no (el agente las
difiere). Para el bucle de editar y recargar no se nota; para un `npm
install` o un build que crea miles de ficheros pequeños, sí: mejor dejarlos en el disco de la máquina (`node_modules` en un
volumen) cuando importe.

## Lo que falta para dejar Docker Desktop

- **`build:`** no existe: la imagen se construye fuera (`docker build`) y se
  importa con `kling image import`.
- **UDP en el Mac**: `ports: "53:53/udp"` se publica en Linux (el daemon
  reenvía un flujo por cliente); en el Mac el VMM aún no reenvía UDP y se
  avisa. Entre servicios, solo TCP.
- **Claves sin equivalente** (se avisa por cada una y el aviso enlaza
  [docker-a-kindling.md](docker-a-kindling.md), que dice qué hacer con cada
  una): `privileged`, `cap_add`/`cap_drop`, `devices`, `network_mode` salvo
  `none`, `extra_hosts`, `dns`, `links`, `hostname`, `env_file`,
  `working_dir`, `tmpfs`, `shm_size`, `ulimits`/`sysctls` (van en `image
  import`), `secrets`/`configs`, `profiles`, `extends`, `scale`, `labels`, las
  opciones de una red (`driver`, `aliases`, `ipv4_address`), rangos de
  puertos, volúmenes anónimos, un fichero suelto como bind mount, y
  `service_completed_successfully`.
- **Caudal de la carpeta** (tabla de arriba) y, en Linux, el limitador de red
  de 16 MiB/s de Firecracker.
- **El arranque en frío** paga ~0,7 s en la primera conexión al agente por el
  reenvío de `kling-vz`.
