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
  `healthcheck:` y versión del daemon. Cambiar algo prepara otra plantilla solo
  para ese servicio y borra la vieja; cambiar el comando reimporta su imagen.
  Una etiqueta que se mueve en el registro no se ve (Docker tampoco vuelve a
  bajarla): `kling compose up -fresh` rehace las plantillas.
- **`environment:`** va en la máquina (`RunRequest.Env`, por MMDS), no en la
  imagen: cambiar una variable no reimporta, y la contraseña no queda en el disco
  de la imagen. `command:` sigue horneado en la imagen (el núcleo no tiene aún un
  comando por máquina).
- Un servicio con **bind mounts** se guarda con su agente esperando a sus
  carpetas y el servicio sin lanzar ([compartir.md](compartir.md#cambios-hechos-en-el-host)):
  al restaurar se conectan las carpetas y el servicio arranca con el código de
  ahora, no con el de cuando se guardó.
- Un servicio con **volúmenes con nombre** arranca en frío, como antes (con el
  entorno horneado en su imagen): congelar un postgres con su directorio de datos
  en un volumen dejaría en la plantilla la caché de un disco que después cambia.
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
(`kling image import <ref> -replace`, o `compose up -fresh`).

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

- **Volúmenes con nombre** (el `pgdata:` de casi todos los compose): esos
  servicios arrancan en frío cada vez. Haría falta guardar la plantilla antes de
  lanzar el servicio (como con las carpetas) para no congelar un disco en uso.
- **`command:` por máquina**: se hornea al importar (`image import ... -- cmd`);
  otro comando es otra imagen (una plantilla nueva lo resuelve en el día a día,
  pero cuesta un import).
- **El arranque en frío** paga ~0,7 s en la primera conexión al agente por el
  reenvío de `kling-vz` (sin medir la causa; apunta al primer paquete en su pila
  de red), y `depends_on` serializa
  restauraciones (~0,3 s del `up` repetido).
- **Caudal de la carpeta** (tabla de arriba) y, en Linux, el limitador de red
  de 16 MiB/s de Firecracker.
- **`healthcheck`** se ejecuta al preparar la plantilla; entre nodos,
  `service_healthy` sigue siendo una sonda TCP. `build:` no existe.
