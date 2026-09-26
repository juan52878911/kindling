# Chispa serverless: una tarea, un dorado, cero coste ocioso

[Chispa](chispa.md) vive normalmente **dentro** del proceso del gateway
(`kind: "chispa"`, sin daemon, microsegundos). Esta página documenta la
alternativa: cada tarea Chispa como su **propia imagen y su propio dorado
congelado** en una microVM, exactamente como un modelo VON
([ai-gateway.md](ai-gateway.md)), pero sin llama.cpp ni GPU de por medio —el
invitado es un binario estático de un par de decenas de milisegundos de
arranque que solo sabe cargar un `.chispa` y contestar `/v1/classify`—.

Por qué existe esto si Chispa en proceso ya es rapidísimo: **empaquetado y
aislamiento por tarea**. Cien tareas Chispa en proceso comparten el mismo
`kling ai serve`: un modelo corrupto, una fuga de memoria o un cliente que
agota el presupuesto de `-chispa-mem` afectan a las demás. Como tarea serverless,
cada una es su propia microVM —su propio `mem.file`, su propio dorado, su
propio ciclo de vida—, despertada por la primera petición y congelada al
quedarse ociosa: el mismo modelo de "muchos modelos listos, ninguno encendido
24/7" que ya usa VON, aplicado a Chispa. El coste es el que mide esta página: una
vuelta de red y, si estaba congelada, un thaw.

```
cliente ──HTTP──> kling ai serve ── /v1/classify ──┬── backend inprocess (por defecto)
                                                     │     chispaCache en este proceso, µs, sin daemon
                                                     │
                                                     └── backend microvm
                                                           │
                                                           pkg/scheduler ──socket──> daemon
                                                           thaw (réplica congelada) o restore (desde el dorado)
                                                           │
                                                           kling-chispa, dentro de la microVM
                                                           carga UN .chispa (+ .chispas opcional) al arrancar
                                                           POST /v1/classify, GET /healthz
```

## Cuándo usar cada backend

| | `inprocess` (por defecto) | `microvm` |
|---|---|---|
| Latencia | 1-15 µs | red + microVM: cientos de µs a milisegundos si está despierta; ~27 ms si hay que despertarla congelada y ~2,2 ms si estaba pausada (ver [Latencia de despertar](#latencia-de-despertar)) |
| Aislamiento | Ninguno: todas las tareas comparten proceso y el presupuesto `-chispa-mem` | Total: cada tarea es su propia microVM, su propia memoria, su propio `mem.file` |
| Empaquetado | Un `.chispa` más en el registro | Una imagen propia (el modelo horneado dentro, como el GGUF de un VON) |
| Coste ocioso | Un modelo cargado ocupa RAM del gateway hasta que el LRU lo saca | Cero: congelada, la réplica no gasta CPU (y en Linux su memoria vuelve al fichero) |
| Necesita daemon | No | Sí (una microVM) |
| Encaja con | La mayoría de las tareas: Chispa ya es µs, no hay nada que aislar | Muchas tareas de terceros o de distintos inquilinos, una tarea con un modelo pesado que no debe compartir presupuesto con las demás, o cuando el resto del catálogo (VON, el codificador) ya vive en microVMs y conviene el mismo modelo operativo para todo |

Las dos conviven en el mismo registro; no hay que elegir una para todo el
gateway. Ver [ai-gateway.md](ai-gateway.md) para el resto de la tarea
(cascada a VON, recalibración): funcionan igual con las dos, porque una
réplica `microvm` contesta con la misma forma (etiqueta, probabilidad,
umbral, distribución completa y evidencia cuando duda) que `chispa.Model` en
proceso.

## `kling chispa deploy`

```sh
kling chispa deploy commits -model commits.chispa -mem 64 -vcpus 1
# Building image "commits" (kling-chispa + commits.chispa)...
# Making the golden snapshot (1 vCPU, 64 MiB)...
#   booted commits-golden-2yek in 46 ms; waiting for kling-chispa...
#   kling-chispa ready in 2.063s; warming up...
#   warm-up answered {"label":"docs","prob":0.372,...}
#   returned 0 MiB of free memory to the host before freezing
# ✓ commits  golden snapshot of task commits  (44M of memory, 4s in total)
#
# Add it to the ai gateway's registry as a microvm-backed model:
#   {"models": {"commits": {"kind": "chispa", "backend": "microvm", "snapshot": "commits"}}}

kling chispa ls
# TASK      SNAPSHOT   MEMORY   REPLICAS RUNNING
# commits   commits    44M      0

kling chispa rm commits          # borra el dorado y la imagen (-keep-image: solo el dorado)
```

`deploy` necesita un daemon (construir la imagen monta un loopback y hace
`chroot`: en Linux, como root; en macOS hay que traer la imagen ya construida
con `kling images copy <name> -from ssh://usuario@host-linux` y hacer el
dorado ahí con `kling chispa deploy <name> -model m.chispa -reuse-image`, igual
que con un modelo VON; `-reuse-image` no puede mirar dentro de la imagen, así
que el `-model` tiene que ser el mismo con el que se construyó). El `.chispa` (y el `.chispas` de huecos,
opcional) viajan dentro de la petición al constructor `chispa`: no hace falta que
el daemon vea el disco de quien manda la orden, así que un daemon remoto por
SSH funciona igual. `-model`/`-slots` se validan (`chispa.Load`/`slots.Load`)
antes de escribirse en la imagen: un fichero corrupto no llega a congelarse en
un dorado inservible.

Dentro de la imagen, `kling-chispa` (`cmd/kling-chispa`) es el invitado entero: un
binario estático (~10 MB, sin cgo, `CGO_ENABLED=0`) que carga el `.chispa` al
arrancar y sirve:

- `GET /healthz` — 200 en cuanto el modelo está cargado (siempre, porque se
  carga antes de escuchar).
- `POST /v1/classify` — `{"text", "fields", "explain"}` → `{"label", "prob",
  "threshold", "confident", "decision", "candidates", "evidence"}`. Camino
  rápido sin reservas cuando Chispa contesta seguro; si duda o se pide `explain`,
  manda la distribución **entera** (no un top-3) y la evidencia, en una sola
  vuelta — así el gateway trata una réplica microvm exactamente como
  `chispa.Model.PredictFull` en proceso, sin perder etiquetas cuando `top_k` no
  acota la escalada.

El invitado no es de fiar: el gateway (`pkg/aigw/chispaguest.go`) valida cada
campo de esa respuesta (etiqueta dentro del modelo, probabilidades y umbral
finitos y en rango, número de candidatos y de evidencia acotado) antes de
usarla, y nunca se fía del `"confident"` que mande kling-chispa —lo recalcula del
`prob` ya validado y el umbral del lado del gateway—. Para saber cuáles son
las etiquetas válidas sin necesitar el `.chispa` de origen (que puede vivir en
otra máquina), `kling chispa deploy` graba las etiquetas del modelo y su sha256
como anotación del dorado; una respuesta que no encaja con ese registro es un
502, no una decisión.

## Registrarla en el gateway

```json
{
  "models": {
    "commits": {"kind": "chispa", "backend": "microvm", "snapshot": "commits"},
    "urgent":  {"kind": "chispa", "path": "urgent.chispa"}
  },
  "tasks": {
    "commit-type": {"chispa": "commits"},
    "is-urgent":   {"chispa": "urgent"}
  }
}
```

`backend` solo se aplica a `kind: "chispa"` (von y embed siempre son una
réplica). Sin `backend`, o con `"inprocess"`, es el `path` de siempre. Con
`"microvm"` hace falta `snapshot` (y ningún `path`); el gateway la despierta,
la congela y la escala igual que a un modelo VON —réplicas por concurrencia,
TTL de red de seguridad, todo lo de [ai-gateway.md](ai-gateway.md#cómo-está-hecho)—.
Si el daemon no contesta (o no hay registro de despliegue que validar, ver
arriba), `/v1/classify` de esa tarea da 503 con un mensaje claro (`chispa model
"commits" (backend microvm) unavailable: ...`); si la réplica contesta pero no
pasa la validación, da 502 (`... sent an invalid answer: ...`). Las tareas
`inprocess` del mismo registro siguen funcionando (ver
[chispa.md](chispa.md#modo-sin-daemon) sobre cuándo hace falta daemon y cuándo no).

## Domótica: la capa 2 serverless

Una tarea de intención del gateway (`/v1/decide`, [intent.md](intent.md); la
de la habitación de demo, [domotica.md](domotica.md)) usa el mismo camino: si
su `model` es un modelo `"backend": "microvm"`, la
capa 2 pregunta a la réplica del dorado (`askChispaGuest` en
`pkg/aigw/chispaguest.go`, lo mismo que `/v1/classify`: etiquetas validadas
contra el registro de despliegue, confianza recalculada en el gateway). Con
`kling chispa deploy <tarea> -model intent.chispa -slots slots.chispas` el
registro guarda también los huecos del `.chispas` (y su sha256), y la réplica
marca los huecos en la misma ida y vuelta: el gateway comprueba cada uno
(nombre conocido, dentro del texto, en orden, como mucho 64) y rehace su
texto del de la petición. Un dorado sin huecos en el registro (sin `-slots`,
o desplegado antes) no puede mandar ninguno; entonces los marca el `.chispas`
de `"slots"` de la tarea, en proceso. Si la tarea pone `"slots"`, gana ese.

La decisión trae `chispa_replica`: `state` (`frozen` = se descongeló,
`paused` = se reanudó, `warm` = ya estaba despierta, `new` = se restauró del
dorado), `wake_ms` y `request_ms`; la habitación de demo lo enseña en el paso
de Chispa de la traza. Si la réplica no contesta o miente, la orden escala
como una duda (`reason: "chispa_error"`), no da un 503.

Medido en el CT 105 (i7-8700T, KVM sin anidar), la habitación de demo desde
el Mac por la red de casa: 11 órdenes, dos pasadas. «Chispa» es el paso de la
capa 2 visto desde el gateway (despertar incluido); «gateway» es
`/v1/decide` entero más la capa 4 si hizo falta; «Mac» es de punta a punta
(la red de casa añade 8–25 ms).

| orden | capa | Chispa | estado (despertar) | gateway | Mac |
|---|---|---|---|---|---|
| enciende la luz del salón | plantilla | — | no la toca | 0,22 ms | 12,5 ms |
| baja un poco las persianas del salón | Chispa | 34,0 ms | **congelada** (thaw 29,1 ms) | 34,3 ms | 44,4 ms |
| ¿puedes poner el salón en azul? | Chispa | 0,70 ms | despierta | 0,92 ms | 8,9 ms |
| calefacción a veintiuno | Chispa | 0,65 ms | despierta | 0,85 ms | 8,6 ms |
| kill the lights | Chispa | 0,61 ms | despierta | 0,84 ms | 8,2 ms |
| crank up the volume | Chispa | 0,59 ms | despierta | 0,76 ms | 9,9 ms |
| heat to 22 please | LLM (escala) | 0,57 ms | despierta | 4 676 ms | 4 689 ms |
| no veo nada | Chispa | 0,54 ms | despierta | 0,72 ms | 18,2 ms |
| pon una alarma a las siete | nadie (escala) | 0,65 ms | despierta | 10,5 ms | 25,1 ms |
| order a pizza | nadie (escala) | 0,42 ms | despierta | 7,1 ms | 19,9 ms |
| put the blinds halfway | LLM (escala) | 0,81 ms | despierta | 3 454 ms | 4 049 ms |
| *tras 3 min ociosa:* baja un poco las persianas del salón | Chispa | 3,34 ms | **pausada** (resume 0,76 ms) | 3,58 ms | 15,8 ms |

Con Chispa en proceso las mismas órdenes daban 0,02–0,07 ms en la capa 2 y
0,2–0,4 ms en `/v1/decide`: la microVM despierta cuesta ~0,4–0,6 ms más por
orden (la ida y vuelta HTTP a la réplica), congelada ~30 ms la primera. Las
decisiones son las mismas: `kling ai eval room` sobre las 9 794 filas de
prueba da exactamente las mismas cifras con los dos backends (contestadas
bien 0,311 → 0,321 con el codificador, +104, McNemar p = 4,9·10⁻³²). Ociosa,
`kling ps` la enseña `paused` (a los 2 min de `-idle`) y luego `warm`
(congelada, a los 10 × `-idle`); el dorado ocupa 75 MiB en disco.

## Cifras

Medido en dos sitios muy distintos: un Intel **i7-8700T** de verdad (Proxmox
CT 105, Debian 12, KVM **sin anidar**, backend Firecracker — la misma máquina
de [von.md#x86-sin-anidar-i7-8700t](von.md#x86-sin-anidar-i7-8700t)) y, para el
backend en proceso, un **Mac mini M4** con el mismo binario. El modelo de
prueba es un `.chispa` de 3 clases, 2^18 cubos (el tamaño por defecto: ~1,5 MB en
memoria, igual de grande da igual el corpus de entrenamiento).

### Una tarea, backend microvm (i7-8700T, KVM sin anidar)

| | |
|---|---|
| Imagen en disco (capa sobre `min`) | 13 MB (24 MB lógicos) |
| Dorado (`mem.file` + metadatos) | 45 MB |
| `kling chispa deploy` de punta a punta | ~4 s (arranque 46 ms + kling-chispa listo 2,06 s + calentamiento + congelar) |
| Primer arranque desde el dorado, sin réplica (`restore`) | 1,41 s |
| **Thaw de una réplica congelada + primera decisión** | **135-140 ms** (tres medidas: 135, 138, 140 ms, vistas por el cliente HTTP, congelando cada vez con `-idle 15s`); hoy 27 ms, ver abajo |
| Réplica despierta, cliente serie (`kling_ai_latency_seconds`) | ver la tabla de concurrencia |

Decisiones/segundo por el gateway (HTTP + socket Unix + un salto de red hasta
la microVM), con `loadtool` (un cliente HTTP mínimo hecho para esta medida,
`N` clientes en bucle cerrado durante 5 s):

| Clientes | Decisiones/s | p50 | p90 | p99 |
|---|---|---|---|---|
| 1 | 757 | 573 µs | 690 µs | 52,6 ms |
| 4 | 2 779 | 1,05 ms | 1,64 ms | 17,9 ms |
| 8 | 3 471 | 1,68 ms | 3,00 ms | 23,1 ms |
| 16 | 4 231 | 2,98 ms | 5,86 ms | 21,9 ms |

La cola de p99 (17-53 ms) por encima de esos pocos miles por segundo es de
esperar: `-max-inflight 1` por defecto sirve una petición a la vez por
réplica y con más clientes que réplicas el resto espera en la cola del
planificador, no de kling-chispa (que contesta en microsegundos, igual que en
proceso). Más réplicas (`max_replicas` en el registro) reparten la cola; esta
tabla es con el valor por defecto (2) y una sola tarea machacándola.

**El mismo modelo, el mismo host, backend `inprocess`** (para comparar sin
cambiar de máquina): con el registro cargado, sin ninguna microVM de por
medio:

| Clientes | Decisiones/s | p50 |
|---|---|---|
| 1 | 9 801 | 91 µs |
| 4 | 26 924 | 123 µs |

(La medida de 8 y 16 clientes en proceso en este host se cortó por una
desconexión de red a media prueba — ver «Lo que falta» abajo — pero la
tendencia con 1 y 4 clientes ya dice lo que hace falta: **sin microVM de por
medio, Chispa es 10-13× más rápido de punta a punta** en esta misma máquina y
esta misma carga, coherente con que el camino en proceso no paga red ni thaw.)

Lectura honesta: el salto a una microVM cuesta **una red y, si estaba
congelada, un thaw de ~135 ms**; despierta, sigue siendo un HTTP normal (el
propio Chispa tarda los mismos 1-5 µs que en proceso, ver [chispa.md](chispa.md), la
diferencia entera es el viaje). Para una tarea que ya vive con otras
réplicas VON o de codificador —memoria, aislamiento y ciclo de vida ya
pensados para microVMs—, ese coste es razonable a cambio de un paquete propio
por tarea. Para una tarea que solo necesita clasificar rápido y no compartir
proceso con nadie más importa, `inprocess` sigue siendo la opción por
defecto y, con estos números delante, la más barata con diferencia.

### Carga bajo demanda en proceso, medida (Mac mini M4)

Esta parte no usa ninguna microVM: es `chispaCache`
([chispa.md](chispa.md), `pkg/aigw/chispacache.go`) por sí sola, con un registro de 0,
10 y 100 modelos (mismo fichero `.chispa` de 1,5 MB en memoria copiado a rutas
distintas, para que cada entrada del registro sea un modelo distinto a
efectos de la caché).

| Modelos en el registro | RSS del proceso nada más arrancar (nada cargado aún) |
|---|---|
| 0 | 15,5 MiB |
| 10 | 18,3 MiB |
| 100 | 28,3 MiB |

Cero modelos cargados: el coste de tener 100 tareas en el registro es de
~13 MiB (config parseada, `http.ServeMux`, un `*ring` de muestras por
tarea) antes de que nadie pida nada. Carga real, con un presupuesto pequeño a
propósito para forzar desalojos (`-chispa-mem 8`, 20 modelos usados uno detrás de
otro):

```
kling_ai_chispa_models_loaded 5           # el LRU desaloja: solo caben 5 de los 20 pedidos
kling_ai_chispa_bytes 7885760             # 7,5 MiB, justo bajo el presupuesto de 8
kling_ai_chispa_loads_total 20
kling_ai_chispa_evictions_total 15
```

Tiempo de carga por modelo (fichero de 1,7 KB, primera vez): 0,3-1,0 ms
(lee, valida el CRC, comprueba topes, reconstruye la tabla de pesos densa);
una vez en la caché, la latencia interna que reporta el gateway baja a **13
µs** — del mismo orden que los 1-5 µs de [chispa.md](chispa.md), con el resto siendo
JSON y HTTP.

Decisiones/s del camino HTTP en proceso, mismo `loadtool`, un solo modelo ya
caliente:

| Clientes | Decisiones/s | p50 | p99 |
|---|---|---|---|
| 1 | 21 956 | 27 µs | 150 µs |
| 4 | 53 499 | 57 µs | 331 µs |
| 8 | 67 971 | 83 µs | 566 µs |
| 16 | 89 457 | 119 µs | 875 µs |
| 32 | 113 024 | 180 µs | 1,32 ms |

Para comparar con Chispa puro sin HTTP ni JSON de por medio, en la misma
máquina (`go test ./pkg/chispa -bench Predict -cpu 1,4`): 1,8 µs/predicción a un
núcleo, 529 ns/op agregado a 4 núcleos (~1,9 M/s; con los 10 núcleos del M4,
del orden de los ~3,4 M/s de [chispa.md](chispa.md)). La diferencia entre esto y los
113 000/s de la tabla de arriba es JSON + HTTP + el socket Unix, no Chispa: por
eso un cliente que puede permitirse hablar Go directo con `pkg/chispa`
(embebiéndolo, no por HTTP) sigue siendo la opción más barata de todas.

## Latencia de despertar

Los 135-140 ms de arriba no eran el thaw (`LoadSnapshot`, 2-3 ms): eran montar
la red otra vez (47 ms), leer la memoria del disco a golpe de fallo de página
(54 ms de resync más 12 de primera petición), esperar el socket del VMM con
pasos de 10 ms y moverlo a su cgroup. Instrumentado por fases y atacado palanca
a palanca en [despertar.md](despertar.md), en el mismo i7-8700T y con una tarea
de 128 MiB:

| Réplica dormida → decisión (cliente, p50) | Antes | Ahora |
|---|---|---|
| Congelada (warm) | 152 ms | **27 ms** |
| Pausada (nivel nuevo, `kling ai serve -paused-mib`, 256 MiB por defecto) | — | **2,2 ms**, a cambio de ~36 MiB de RSS por réplica |

Con el nivel pausada, una tarea Chispa pequeña y popular vuelve a estar en el
orden de milisegundos aunque lleve minutos sin usarse; el segador reparte el
presupuesto por popularidad / memoria, así que un VON grande no se lo come.

## Lo que falta (pendiente de este trabajo)

- **Barrido de 1/10/50 tareas microvm simultáneas** (disco y RAM agregados,
  reparto de página entre réplicas del mismo dorado en Linux) y las mismas
  cifras en el backend `vz` de macOS: la sesión que escribió esta página
  perdió el acceso de confianza al host de pruebas (CT 105) a media medición
  —la clave de host SSH cambió sin más aviso que el propio SSH; no se aceptó
  la clave nueva ni se insistió— y no había un segundo host Linux disponible
  para construir la imagen del lado de macOS (construir una imagen necesita
  Linux: `chroot` y `loop`, ver arriba). Antes de repetir esto hace falta
  confirmar con quien administra ese host que el cambio de clave fue
  legítimo.
- La medida de concurrencia con backend microvm se quedó en 16 clientes: con
  más réplicas por tarea (`max_replicas`) la cola de p99 debería acortarse;
  no medido.
- **Auditoría contra VON** (`audit` en la tarea) funciona igual con los dos
  backends: solo necesita el `chispa.Prediction` que ya sale de `Classify`, venga
  de `chispaCache` o de una réplica.
- **Recalibración en línea (`kling ai calibrate`) NO funciona con un modelo
  `backend: microvm`**: calibrar reescribe el `.chispa` en disco (guarda
  `<ruta>.prev` y escribe el nuevo), y un modelo microvm no tiene ese fichero
  en la máquina del gateway —vive horneado dentro de la imagen de la réplica,
  que puede correr en otro host—. `kling ai calibrate` de esa tarea falla con
  un mensaje claro en vez de intentarlo a medias; la manera de recalibrar es
  reentrenar y volver a desplegar:
  `kling chispa train ... -o new.chispa && kling chispa deploy <tarea> -model new.chispa -replace`.
