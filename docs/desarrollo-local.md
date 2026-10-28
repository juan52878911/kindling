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
importa las imágenes en paralelo, levanta el grafo y se queda en primer plano:

- un **bind mount** de un directorio (`./src:/app`, `../app:/app:ro`) es una
  [carpeta compartida](compartir.md) viva `rw` (o `ro`) del nodo. Tiene que
  estar bajo `daemon.share_roots`. Un fichero suelto no se puede compartir (se
  avisa y se salta);
- los **`ports:`** (`"5432:5432"`, `"8080:80"`) se publican en `127.0.0.1` del
  host mientras corre. Los publica el propio `kling`, que resuelve en cada
  conexión el reenvío de la máquina (en el Mac cambia con cada arranque o
  descongelación) y despierta la máquina si está congelada. Nunca en otras
  interfaces: Docker publica en todas por defecto;
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
- **Ctrl-C** quita las máquinas; los volúmenes con nombre se quedan. `-keep`
  las deja corriendo y `kling compose down` las quita.

Un `up` de un proyecto que ya estaba lo sustituye.

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
$PWD:/app:rw -p 3000:3000` se queda en primer plano publicando
`127.0.0.1:3000`, con la salida del servicio, y despierta la máquina si está
congelada. Ctrl-C deja de publicar; la máquina se queda.

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
| `npm install` de express (caché caliente) en la carpeta | 1,26 s (antes: fallaba, EPERM en `.bin`) | | 0,77 s |
| `npm install` en el disco de la máquina | 0,49-0,82 s | | 0,55 s |
| escritura secuencial 1 MiB en la carpeta | 336 MB/s | | 604 MB/s |
| escrituras de 4 KiB en la carpeta | 3 060/s | | 26 600/s |
| lectura secuencial en la carpeta | 242 MB/s | | 3,2 GB/s |

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

La red del proyecto no cuesta: el mismo compose con cada servicio en su red
(0 aristas `link` frente a 6) deja postgres en 0,99 s frente a 0,94 s (n=10):
por debajo del ruido, < 10 ms por arista.

Un fallo intermitente sigue abierto: en 3 de unos 45 `up` la copia de postgres
no contestó a su agente tras restaurar (`guest agent not reachable`), el `up`
se deshizo y el siguiente fue bien. No se reprodujo en 35 restauraciones de la
misma plantilla fuera del grafo (solas o con otras dos a la vez).

Los tiempos de kindling incluyen el CLI (`kling run`/`compose up` es un proceso
nuevo cada vez) y la sonda. En el arranque en frío, ~0,7 s de los 1,4 s de la
app son la primera conexión del host al agente por el reenvío de `kling-vz`,
antes de poder conectar la carpeta.

Lo que pesa en kindling: cada operación de la carpeta es un viaje de ida y
vuelta al daemon (~0,3 ms) por el reenvío de `kling-vz`, y las escrituras van
directas (sin write-back del kernel). Para el bucle de editar y recargar no se
nota; para un `npm install` o un build que escribe miles de ficheros
pequeños, sí: mejor dejarlos en el disco de la máquina (`node_modules` en un
volumen) cuando importe.

## Lo que falta para dejar Docker Desktop

- **`build:`** no existe: la imagen se construye fuera (`docker build`) y se
  importa con `kling image import`.
- **Los puertos los publica el CLI** en primer plano: no hay `compose up -d`
  que deje publicado `127.0.0.1:5432` sin un proceso delante (`-keep` deja las
  máquinas, pero no los puertos).
- **Una conexión a un servicio que aún no escucha no recibe un RST**: espera
  (y el invitado reintenta el SYN a 1 s, 3 s…). En Docker falla en seguida. Una
  app que conecta antes de que postgres esté listo tarda un segundo de más en
  ver la base, no falla; con `depends_on: {db: {condition: service_healthy}}`
  no pasa.
- **Claves sin equivalente** (se avisa por cada una): `privileged`,
  `cap_add`/`cap_drop`, `devices`, `network_mode` salvo `none`, `extra_hosts`,
  `dns`, `links`, `hostname`, `env_file`, `working_dir`, `tmpfs`, `shm_size`,
  `ulimits`/`sysctls` (van en `image import`), `secrets`/`configs`, `profiles`,
  `extends`, `scale`, `labels`, las opciones de una red (`driver`, `aliases`,
  `ipv4_address`), UDP y rangos de puertos, volúmenes anónimos, un fichero suelto
  como bind mount, y `service_completed_successfully`.
- **Caudal de la carpeta** (tabla de arriba) y, en Linux, el limitador de red
  de 16 MiB/s de Firecracker.
- **El arranque en frío** paga ~0,7 s en la primera conexión al agente por el
  reenvío de `kling-vz`.
