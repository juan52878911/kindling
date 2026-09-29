# kling phone — teléfonos Android en microVMs

Extensión oficial de kling (`ext/phone`, binario `kling-phone`, issue #91): teléfonos
Android (Redroid 13) que arrancan en un segundo desde un dorado, cada uno con su
identidad, un muro web con todas las pantallas y un servidor MCP que da un teléfono por
sesión. Todo por la API del daemon (`pkg/api`), sin `allow_exec`: el teléfono se opera
por la API de `kling-phoned` en el 8091 del invitado
([`prototypes/android/docs/phoned.md`](../../prototypes/android/docs/phoned.md)).

```sh
make -C ext/phone install                 # o: kling plugin install phone -file ./kling-phone
kling phone golden build                  # Android en frío, comprobado y guardado (una vez)
kling phone up -n 2                       # dos teléfonos, cada uno con su identidad
kling phone ls
kling phone view -open                    # el muro: pantallas, tocar, deslizar, teclas, salud
kling phone adb 1 shell getprop ro.serialno
kling phone api 1 GET /v1/tree
kling phone mcp                           # e importarlo:  kling mcp link phone <url que imprime>
```

Necesita una imagen Android con `kling-phoned` (`prototypes/android/image/build-image.sh`,
`PHONED=1`, que es el valor por defecto) y el kernel Android en el daemon
(`prototypes/android/README.md`). En el Mac, un daemon privado para el teléfono (el kernel
es uno por daemon).

## Órdenes

| Orden | Qué hace |
|---|---|
| `up [-n N] [-golden G]` | N clones del dorado (lo construye si falta). Cada uno: `run -from -wait-ready`, un token de la API nuevo, la identidad por MMDS con los ganchos de la imagen (`android_id`, serie, SSAID, claves de adb, el sha256 del token) y la comprobación por la API **con ese token** de que la serie y el `android_id` son los que se le dieron |
| `ls [-json]` | nombre, estado, dirección de adb (el reenvío del Mac o la IP de la VM), salud de la API (`ok`, `locked`: sin token, `unhealthy`), dorado y fondo. Poda los tokens de máquinas que ya no existen |
| `view [-listen 127.0.0.1:8765] [-open]` | el muro (el `wall.py` del prototipo, en Go y por la API): capturas con caché de 250 ms y una en curso por teléfono, tocar, deslizar, BACK/HOME/RECENTS y la salud cada 15 s |
| `adb <tel> [args]` | resuelve la dirección de adb y ejecuta `adb -s`; sin args la imprime |
| `api <tel> MÉTODO ruta [fichero\|-]` | una llamada a `kling-phoned` con el token; `/v1/screen` sale en PNG y un APK a `/v1/install` va en base64 solo. `-no-token`: lo que ve una arista sin él |
| `pause`/`resume <tel>...` | pausa en RAM / reanuda (o descongela) y espera a la API |
| `rm <tel>... \| -a` | la máquina y su token |
| `pool N [-watch] [-freeze-after 30m]` | N repuestos pausados con identidad y token, listos para un resume de milisegundos; `-watch` rellena y congela los que llevan pausados más de `-freeze-after` (pausado gasta RAM, congelado nada) |
| `token <tel> [-read \| -rotate]` | el token de control (para dárselo a quien deba controlar el teléfono, p. ej. un nodo de grafo), uno nuevo de solo lectura, o rotarlos todos |
| `adopt <máquina>` | identidad y token para un teléfono que no hizo `kling phone`: un nodo de grafo `from: <dorado>`. Hasta entonces su API está cerrada |
| `golden build [-image I] [-name G] [-replace]` | arranque en frío con `-cpu-pct vCPU×100` y `-wait-ready`; salud; con un token de un solo uso, `POST /v1/verify-cache` (la caché de páginas contra el disco, `O_DIRECT`) y el búfer de fallos (SIGILL, caídas de system_server); revoca el token; `save`; y **otra vez en un clon del dorado guardado**: si el clon no pasa, el dorado se borra |
| `golden verify` · `golden inspect [-json]` | la misma comprobación en un clon de usar y tirar; lo que quedó anotado |
| `mcp [-listen 127.0.0.1:8095] [-phone P] [-pool N]` | el servidor MCP (abajo) |

`<tel>` es el nombre (`phone-3`) o el número (`3`). Configuración (`kling config set
phone.<clave>`): `image`, `golden`, `prefix`, `cpus`, `mem`, `egress`, `adb_pubkey`
(por defecto `~/.android/adbkey.pub`), `adb_keys_dir` (`<tel>.pub` por teléfono),
`min_memlevel` (macOS: no arranca otro teléfono por debajo de ese
`kern.memorystatus_level`; 35).

### Lo que guarda y dónde

| Qué | Dónde |
|---|---|
| que una máquina es un teléfono, su dorado, su papel en el fondo | etiquetas `kindling.phone`, `kindling.phone.golden`, `kindling.phone.pool`, `kindling.phone.owner` (las del teléfono y `kling.ports=5555,8091` las hereda cada clon del dorado) |
| el token de la API de cada clon | store del daemon, `phone/<id de la máquina>` (con authz y rol de inquilino, un fichero 0600 del usuario) |
| cómo se construyó el dorado y sus comprobaciones | anotación `phone` del snapshot: imagen, arquitectura, kernel, `kling-phoned`, estado y hash de verity, red, adb seguro, uidump, tiempos, resultado de la comprobación al construir y de la última `verify` |

## El servidor MCP: un teléfono por sesión

`kling phone mcp` habla MCP por HTTP en el anfitrión y cada herramienta es una llamada
a `kling-phoned` por el proxy del daemon con el token del clon. La extensión `mcp` lo
importa como servidor externo enlazado y su gateway lo expone como `phone.screen`,
`phone.tree`, `phone.tap`, `phone.swipe`, `phone.text`, `phone.key`, `phone.install` y
`phone.launch`:

```sh
kling phone mcp -pool 1                    # imprime la URL (con una ruta secreta al azar)
kling mcp link phone http://127.0.0.1:8095/<secreto>/mcp
kling connect ...                          # o cualquier cliente del gateway: call_tool phone.screen
```

Las dos extensiones no comparten código: se hablan por el núcleo (el enlace vive en el
store del daemon) y por MCP. El gateway abre una sesión propia contra el enlace por cada
conversación, y aquí cada sesión recibe **su** teléfono cuando la primera herramienta
lo necesita: un repuesto del fondo (resume de milisegundos) o uno nuevo del dorado con
identidad y token propios. `initialize` y `tools/list` no crean nada (es lo que hace
`kling mcp link` para leer el catálogo). Sin uso durante `-idle` (2 min) se pausa y
vuelve solo en la siguiente llamada; a los `-session-ttl` (15 min), o con un `DELETE`
de la sesión, se borra: lo que hizo una conversación en su teléfono no lo hereda otra.
`-phone P` da el mismo teléfono a todas las sesiones.

Por qué así y no `kling-phoned` sirviendo MCP en el invitado e importado con `kling mcp
import -isolation session`: el gateway habla MCP solo con el 8080 del invitado (el
agente de kindling, no `kling-phoned`), y su máquina por sesión es un `run -from` sin la
identidad ni el token de cada clon. Y por qué no `pkg/scheduler` para el fondo: sus
instancias nacen con `runFresh` (sin gancho para la identidad) y su segador congela por
el tráfico que él mismo reenvía; aquí el tráfico es la API del teléfono. El fondo es
pequeño y va por la API del daemon (`Pause`, `Thaw`, `Freeze`, etiquetas).

## Seguridad

La API del teléfono exige un token (#110, `SECURITY.md` §21): el muro y el servidor MCP
lo añaden solos, y escuchan en loopback con defensas contra otras webs del navegador
(`Host` de loopback, cabecera propia en los POST del muro, `Content-Type:
application/json` y ruta secreta en el MCP). Quien llega a ellos controla los teléfonos.

### Con la autorización del daemon (`docs/authz.md`)

| Operación de `kling phone` | Rutas del daemon | admin | tenant |
|---|---|---|---|
| `ls`, resolver un teléfono | `GET /machines`, `GET /machines/{ref}` (`list`, `machine`) | sí | los suyos |
| `up`, `pool`, `golden build/verify`, `mcp` (teléfono nuevo) | `POST /machines` (`create`); del dorado: `GET /snapshots/{name}` (`snapshot.read`) | sí | sí, con dorado suyo o compartido (`shared_templates`) |
| identidad y tokens | `POST /machines/{ref}/mmds`, `POST /machines/{ref}/hooks` (`machine`) | sí | los suyos |
| la API del teléfono (`api`, `view`, `mcp`) | `POST /machines/{ref}/guest` (`machine`) | sí | los suyos |
| `pause`, `resume`, `rm`, fondo | `POST .../pause\|thaw\|freeze`, `DELETE /machines/{ref}`, `PUT .../labels` (`machine`) | sí | los suyos |
| guardar el dorado | `POST /machines/{ref}/commit` (`machine`) | sí | sí: el dorado es suyo |
| anotar el dorado | `PUT /snapshots/{name}/annotations/phone` (`snapshot.write`) | sí | solo su dorado |
| guardar los tokens | `/store/phone/*` (`admin`) | sí | **no**: 403, y los tokens van a `$XDG_CONFIG_HOME/kling/phone-tokens/<daemon>/` (0600) en la máquina del CLI |
| receta de la imagen (hash de verity en la anotación) | `GET /images/{name}/recipe` (`admin`) | sí | no: la anotación sale sin el hash |
| `adopt` de un nodo de grafo | las de identidad y tokens sobre la máquina del nodo (`machine`) | sí | la suya (de un grafo suyo) |

Un inquilino opera sus teléfonos desde la máquina donde los creó (sus tokens están ahí);
un admin, desde cualquiera.

## Cifras (2026-09-29)

Daemons privados, binarios de la rama. Mac: MacBook Air M4, vz, arm64, 2 vCPU, 1,5 GiB,
una copia de la imagen del 27-09 con `kling-phoned` puesto por `kling image put` (sin
uidump). Linux: el CT `phones` (x86_64, Firecracker), imagen del 28-09 igual (con uidump,
sin verity).

| | Mac arm64 (vz) | Linux amd64 (Firecracker) |
|---|---|---|
| `golden build`: listo en frío / caché (ficheros, MiB) / `save` / clon | 7,7–14,6 s / 3,3–6,1 s (1623, 716) / 1,7–4,3 s / 3,3 s | 12,6–13,5 s / 10,0 s (1711, 863) / 3,7 s / 10,6 s |
| `up`: restaurar / identidad / API | 1,0–1,7 s / 2,4 s / 0,2 s | 0,15–1,9 s / 5,7–7,4 s / 0,4–0,8 s |
| `resume` de un pausado, con la API contestando | ~10 ms | — |
| muro: captura (ida y vuelta del navegador) | 180–290 ms | ~0,5 s (1,1 s por el túnel ssh al Mac) |
| MCP por el gateway: primera herramienta con repuesto / sin él | 0,41 s (reclamar 2 ms) / 4,6 s | 1,0 s (6 ms) / 13 s (dos sesiones a la vez) |
| MCP: `screen` / `tree` / `tap` | 0,08 s / 1,9 s (uiautomator) / 0,02 s | 0,18–0,20 s / 0,01–0,12 s (uidump) / 0,01 s |

En los dos: dos teléfonos con serie, `android_id` y huella de SSAID distintas, adb con la
clave del usuario entra (`ro.serialno` = la de `/v1/identity`); dos sesiones MCP a la vez
con un teléfono cada una (screen → tree → tap en "Gallery" → la Galería abierta en ese
teléfono y no en el otro); el teléfono ocioso pausado y reanudado solo; el repuesto
del fondo reclamado y el viejo congelado por `pool -watch`. Una arista `link` al 8091
sin token recibe `401` (y `403` con uno de lectura en una ruta de control), en Linux y
en macOS (docs/phoned.md).

**Hallazgo en el Mac**: con el Mac cargado (otros agentes, `kern.memorystatus_level`
40–56), de cinco arranques en frío dos tenían páginas de la caché **a ceros**
(`core-oj.jar`, un APK de Wi-Fi; 3 páginas, las 3 a ceros), en otro `logcat` murió con
SIGILL al leer el búfer de fallos, y `golden build` se negó a guardarlos; y un dorado que
pasó la comprobación en frío tenía una página de `libart.so` rota en **todos** sus
clones (se rompió al volcar o al restaurar). Por eso `golden build` comprueba también un
clon del dorado guardado y lo borra si no pasa. El quinto arranque salió limpio en frío
y en el clon (`golden verify` después, también). Las pruebas de arriba en el Mac se
hicieron con el dorado anterior, el de la página de `libart.so` rota, sin que ningún
proceso se cayera a la vista. En Linux, limpio siempre.

## Pendiente

- La release no publica `kling-phone` todavía (como `kling-db`): se compila con
  `make -C ext/phone install`.
- El rol de inquilino con authz está probado con el daemon falso, no con un daemon con
  política.
- Una arista del grafo que reparta el token a los dos extremos (el núcleo no sabe de
  tokens de aplicación): hoy el operador se lo da al nodo (`kling phone token` →
  `kling machine secret`).
