# De Docker a kindling, orden por orden

Qué teclear en kindling para lo que harías con `docker` y `docker compose`, y
qué pasa con cada clave de un `compose.yaml` que aquí no tiene equivalente.
`kling compose up` avisa de cada una (una línea por clave) y enlaza esta
página. Para el bucle de desarrollo en el Mac y sus cifras, ver
[desarrollo-local.md](desarrollo-local.md); para las imágenes, [docker.md](docker.md).

La diferencia de fondo: cada servicio es una **microVM** con su propio núcleo,
no un contenedor. Lo que se arranca se restaura de una plantilla caliente (su
memoria ya con el servicio levantado), y "parar" es **congelar**: la memoria
va a disco y despertar tarda milisegundos.

## `docker`

| Docker | kindling | Notas |
|---|---|---|
| `docker run -d -p 8080:80 nginx` | `kling run -image nginx -p 8080:80` | El puerto lo publica el **daemon** en `127.0.0.1` mientras la máquina exista; kling se queda siguiendo la salida (Ctrl-C deja máquina y puerto). |
| `docker run -p 80 nginx` (puerto al azar) | `kling run -image nginx -p :80` | `kling ps` dice cuál tocó (columna `PORTS`). |
| `docker run -p 0.0.0.0:80:80` | — | Solo `127.0.0.1`: nunca se publica hacia la red. |
| `docker run -p 53:53/udp` | `-p 53:53/udp` | En Linux. En el Mac aún no (el VMM no reenvía UDP): `501`. |
| `docker run -e K=V`, `--env-file` | `-e K=V`, `-env-file F` | Van en la máquina (MMDS), no en la imagen. |
| `docker run -v datos:/data` | `kling volume create datos` + `-v datos:/data` | Volumen con nombre, sobrevive a la máquina. |
| `docker run -v $PWD:/app` | `-share $PWD:/app:rw` | Carpeta viva; tiene que estar bajo `daemon.share_roots`. |
| `docker ps` | `kling ps` | Con `PORTS` si alguna publica. |
| `docker logs -f x` | `kling logs -service -f x` | |
| `docker exec -it x sh` | `kling shell x` (o `kling exec x -- cmd`) | Necesita `-allow-exec` en el `run` (las de compose lo tienen). |
| `docker stop x` / `docker start x` | `kling freeze x` / `kling thaw x` | Congelar conserva la memoria: `thaw` vuelve en milisegundos al punto exacto. `kling stop`/`start` es parar de verdad y arrancar en frío. |
| `docker restart x` | `kling stop x && kling start x` | Arranque en frío sobre su disco. |
| `docker rm -f x` | `kling rm x` | Se lleva sus puertos publicados. |
| `docker build` | — | Se construye fuera y se importa: `kling image import <ref>` (o `-archive` de un `docker save`). |
| `docker pull` | `kling image import <ref>` | Una vez; después `run` restaura. |
| `docker commit` | `kling save x nombre` | Guarda también la memoria: es una plantilla. |

## `docker compose`

| Docker Compose | kindling | Notas |
|---|---|---|
| `docker compose up` | `kling compose up` | En primer plano con la salida junta; Ctrl-C quita máquinas y puertos (los volúmenes se quedan). |
| `docker compose up -d` | `kling compose up -d` | Levanta, deja los puertos publicados por el daemon y vuelve. |
| `docker compose down` | `kling compose down` | Máquinas y puertos fuera. |
| `docker compose down -v` | `kling compose down -v` | Y los volúmenes con nombre del proyecto. |
| `docker compose ps` | `kling compose ps` | Servicio, máquina, estado y puertos. |
| `docker compose logs -f [svc]` | `kling compose logs -f [svc]` | `-t` con hora, `-tail N`. |
| `docker compose exec svc cmd` | `kling compose exec svc cmd` | Con terminal si la hay (`kling shell`); `-T` sin ella. `-w`, `-e`. |
| `docker compose stop` / `start [svc]` | `kling compose stop` / `start [svc]` | **Congela** y despierta (ver arriba): `start` no relanza el proceso, lo continúa. |
| `docker compose restart [svc]` | `kling compose restart [svc]` | Para y arranca en frío sobre su disco, con el `environment:` del compose. |
| `-f fichero`, `-p nombre` | igual, delante o detrás del subcomando | También `--file`, `--project-name`, `--detach`, `--volumes`, `--follow`. |
| `docker compose up --build` | — | Sin `build:` (abajo). |
| `.env` junto al compose | igual | El entorno de la shell manda sobre el fichero. `$VAR`, `${VAR}`, `${VAR:-def}`, `${VAR-def}`, `${VAR:+alt}`, `${VAR:?msg}` (que falla, con `msg`) y `$$`. |
| Un proyecto por nombre | igual, y **de un solo fichero** | Las máquinas llevan `kling.compose.file`; un `up` o `down` desde otro fichero con el mismo nombre de proyecto se niega (usa `-p`). Dos `up` del mismo proyecto a la vez, tampoco. |

### Claves del servicio

| Clave | Qué pasa | Qué hacer |
|---|---|---|
| `image` | La imagen se importa (`kling image import`) una vez. | |
| `build` | **No existe**: el servicio se salta. | `docker build` y `kling image import` (o `image: ` a lo que importaste). |
| `command`, `entrypoint`, `user`, `restart` | Como en Docker. `user:` vale también para el healthcheck. | |
| `environment` | En la máquina; cambiarla no reimporta. Un `0123` no pierde el cero. | |
| `env_file` | **No se lee.** | Pásalas a `environment:` (con `${VAR}` del `.env`), o `kling image import -env-file`. |
| `ports` | Publicados por el daemon en `127.0.0.1`. `"80"` → uno libre; `0.0.0.0:` → se avisa y es `127.0.0.1`; `/udp` solo en Linux; un puerto ocupado falla **antes** de importar. Rangos (`8000-8010`): no. | |
| `expose` | Puertos que los demás servicios alcanzan. | |
| `volumes` con nombre | Volúmenes de kling (`<proyecto>-<nombre>`), montados al restaurar. | |
| `volumes` de un directorio (`./src:/app`) | Carpeta viva `rw` (`:ro`, `ro`). | Bajo `daemon.share_roots`. |
| `volumes` de un fichero suelto | **No**: se avisa y se salta. | Monta su directorio. |
| `volumes` anónimos (`/var/log/x`) | **No**. | Declara uno con nombre. |
| `depends_on` (y `service_healthy`) | Orden de arranque; `service_healthy` ejecuta el `test` dentro, con su `interval`/`timeout`/`retries`/`start_period`. | |
| `service_completed_successfully` | **No.** | |
| `healthcheck` | Se ejecuta dentro, con el usuario del servicio; `disable: true` lo quita. | |
| `networks` | Quién alcanza a quién (cada servicio, a todos los de sus redes). Opciones de una red (`driver`, `aliases`, `ipv4_address`): no. | |
| `network_mode` | `none` aísla; `host` y los demás, **no**. | |
| `links`, `extra_hosts`, `dns`, `hostname`, `domainname` | **No**: un servicio alcanza a los demás por su nombre (`db` → `db.graph`). | |
| `privileged`, `cap_add`, `cap_drop`, `devices`, `security_opt`, `pid`, `ipc` | **No**: es una microVM con su núcleo; no hay privilegios del host que dar. | Lo que necesite el núcleo del invitado, en la imagen. |
| `ulimits`, `sysctls`, `tmpfs`, `shm_size` | No van en el nodo. | `kling image import -ulimit` / `-sysctl`. |
| `working_dir` | **No.** | El `WORKDIR` de la imagen. |
| `cpus`, `mem_limit`, `deploy.resources` | vCPU y memoria de la máquina. | |
| `platform` | `linux/amd64` en un host arm64 va emulado ([imagenes-amd64.md](imagenes-amd64.md)). | |
| `container_name` | Se ignora: la máquina es `<proyecto>-<servicio>`. | |
| `labels` | No se llevan (las etiquetas del nodo son del daemon). | |
| `secrets`, `configs` | **No.** | Variables en `environment:`, o `kling machine secret`. |
| `profiles`, `extends`, `scale` | **No.** | |
| `init`, `stdin_open`, `tty`, `stop_signal`, `stop_grace_period`, `logging`, `pull_policy`, `read_only`, `group_add` | **No.** | |

## Lo que kindling hace y Docker no

- **Despertar en milisegundos**: con `daemon.idle_freeze` (10 min por defecto en
  el Mac), un proyecto con puertos publicados y sin conexiones se congela solo
  y la primera conexión lo despierta. `kling.idle=keep` lo excluye.
- **Plantillas calientes**: un `up` repetido restaura cada servicio ya
  arrancado (postgres sin initdb ni arranque).
- **Un núcleo por servicio**: lo que haga un servicio no ve al host ni a los
  demás salvo por las aristas del grafo.
