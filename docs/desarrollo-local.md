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
- la salida de cada servicio va junta, con el nombre delante (`app   | ...`);
- **Ctrl-C** quita las máquinas; los volúmenes con nombre se quedan. `-keep`
  las deja corriendo y `kling compose down` las quita.

Un `up` de un proyecto que ya estaba lo sustituye. Las imágenes se reimportan
con `-replace` en cada `up` (con las capas en caché, 1-3 s), para que un cambio
de `environment:` o `command:` llegue.

Con una sola máquina, sin compose: `kling run -image node:22-alpine -share
$PWD:/app:rw -label kling.ports=3000`, y el puerto de `kling inspect` (campo
`forwards`), que no es fijo.

## Lo que se ve al editar

Con un daemon y un agente que hablan `WATCH` ([compartir.md](compartir.md#cambios-hechos-en-el-host)):

- un cambio hecho en el host se ve en el invitado en milisegundos (antes, al
  caducar la caché de 1 s);
- los vigilantes del invitado (`node --watch`, nodemon, vite, webpack) se
  enteran: modificar, crear, borrar, `mkdir` y el guardado atómico de un editor
  (escribir aparte y renombrar encima) disparan inotify dentro.

Hay que reimportar la imagen para que lleve el agente nuevo
(`kling image import <ref> -replace`, o `compose up`, que ya lo hace).

## Medidas (Mac con M4, kling-vz, 2026-10-27)

Docker Desktop 29.6.1 en el mismo Mac. Desde nada a la primera respuesta en
`127.0.0.1` (imágenes ya bajadas; en kindling, ya importadas):

| | kindling | Docker Desktop |
|---|---|---|
| redis:7-alpine hasta `PONG` | 1,4 s | 0,96 s |
| postgres:16-alpine hasta aceptar el arranque de sesión | 5,0 s | 1,5 s |
| app de Node con su código en carpeta viva | 1,7 s (1 s es el reinicio: ver abajo) | 0,6 s |
| compose de 3 (app + postgres + redis), todo contestando en sus puertos | 4,8 s (`compose up`, con reimport) | 1,2 s |
| cambio de un fichero en el host leído por el servidor del invitado | 5 ms (p50; 9 ms máx.) | 3 ms (31 ms máx.) |
| editar en el host → `node --watch` sirve lo nuevo | **254 ms** (antes: nunca) | 257 ms |
| `npm install` de express (caché caliente) en la carpeta | 1,26 s (antes: fallaba, EPERM en `.bin`) | 0,77 s |
| `npm install` en el disco de la máquina | 0,49-0,82 s | 0,55 s |
| escritura secuencial 1 MiB en la carpeta | 336 MB/s | 604 MB/s |
| escrituras de 4 KiB en la carpeta | 3 060/s | 26 600/s |
| lectura secuencial en la carpeta | 242 MB/s | 3,2 GB/s |

Lo que pesa en kindling: cada operación de la carpeta es un viaje de ida y
vuelta al daemon (~0,3 ms) por el reenvío de `kling-vz`, y las escrituras van
directas (sin write-back del kernel). Para el bucle de editar y recargar no se
nota; para un `npm install` o un build que escribe miles de ficheros
pequeños, sí: mejor dejarlos en el disco de la máquina (`node_modules` en un
volumen) cuando importe.

## Lo que falta para dejar Docker Desktop

- **El servicio arranca antes de que la carpeta esté montada.** El agente
  arranca el `ENTRYPOINT` en cuanto arranca, y el daemon conecta las carpetas
  vivas justo después: `node /app/server.js` falla una vez (`Cannot find
  module`) y la política de reinicio lo levanta 1 s después; un `node --watch`
  que arranca sin el fichero no se recupera. Arreglo: un parámetro del kernel
  (`kling.shares=N`, como `kling.env`) para que el agente espere a sus N
  carpetas antes de arrancar el servicio (`internal/machine` + `pkg/guest/service.go`).
- **`docker run IMAGEN CMD`**: el comando se hornea al importar
  (`image import ... -- cmd`); otro comando es otra imagen.
- **El entorno del compose se hornea** en la imagen de cada servicio.
- **postgres tarda 5 s en arrancar** frente a 1,5 s en Docker (initdb en frío);
  una plantilla (`kling save`) lo deja en milisegundos, pero `compose up` aún
  no las usa.
- **Caudal de la carpeta** (tabla de arriba) y, en Linux, el limitador de red
  de 16 MiB/s de Firecracker.
- **`healthcheck`** se aproxima con una sonda TCP; `build:` no existe.
