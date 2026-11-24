# Gateway de IA: muchos modelos listos, ninguno encendido 24/7

`kling ai serve` sirve dos clases de modelo detrás de una API, cada una para lo
suyo:

- **Chispa** ([chispa.md](chispa.md)) **clasifica, enruta y filtra**: un clasificador
  lineal de ~1 MB que vive **dentro** del proceso del gateway y contesta en
  microsegundos, con una probabilidad calibrada y un umbral por clase.
- **VON** ([von.md](von.md)) **genera**: resume, redacta, contesta. Un LLM de
  0,36–1,5B parámetros en una microVM, restaurado de un dorado congelado con el
  modelo ya cargado. Se despierta con la primera petición y se **congela al
  quedarse ocioso** (0 CPU; en Linux su memoria vuelve a su fichero).

Cuando Chispa duda, la respuesta sale igual, marcada `escalate: true`, y **quien
llama decide**. Encadenar Chispa → VON (la **cascada**: lo que Chispa duda lo contesta
VON) existe, pero es opcional por tarea y **solo se activa si una evaluación con
datos de esa tarea demuestra que acierta más que Chispa solo** (`kling ai eval`). Por
qué: medido en la clasificación de commits, ninguna cascada probada —con LLM de
0,5B, 1,5B ni 3B— igualó a Chispa solo ([Cifras](#cifras-mac-mini-m4-backend-vz)).

```
cliente ──HTTP──> kling ai serve ──┬── /v1/classify, /v1/decide ── Chispa (en proceso, µs)
                                   │        seguro → respuesta
                                   │        duda   → escalate: true (o VON, si la
                                   │                 cascada de la tarea está respaldada)
                                   └── /v1/generate, /v1/chat/completions
                                            └── pkg/scheduler ──socket──> daemon ──> réplica VON
                                                thaw al llegar, freeze al quedarse ociosa,
                                                réplicas por concurrencia, tope por modelo
```

## Uso

El registro es un fichero JSON (`~/.config/kling/ai.json` por defecto):

```json
{
  "models": {
    "commits": {"kind": "chispa", "path": "commits.chispa"},
    "qwen":    {"kind": "von", "snapshot": "von-qwen15", "max_replicas": 2}
  },
  "tasks": {
    "commit-type": {"chispa": "commits"},
    "summarize": {
      "von": "qwen", "max_tokens": 64, "temperature": 0.2,
      "system": "You write very short summaries.",
      "prompt": "Summarize this {kind} in one sentence:\n{input}"
    }
  }
}
```

```sh
kling ai model add von-qwen15 -model qwen2.5-1.5b-instruct -quant q4_k_m   # el dorado (von.md)
kling ai chispa train -data train.jsonl -valid valid.jsonl -o commits.chispa

kling ai serve                          # socket Unix 0600 en ~/.config/kling/ai.sock
kling ai test commit-type "fix crash when the cache is cold"
kling ai generate summarize -var kind="commit message" "fix(parser): handle empty input"
kling ai ls                             # modelos, tareas, cascadas y muestras
```

En TCP, solo con `-listen` explícito y con token:

```sh
kling ai serve -listen 0.0.0.0:8090   # genera ~/.config/kling/ai.token (0600) si no existe
curl -H "Authorization: Bearer $(cat ~/.config/kling/ai.token)" -H 'Content-Type: application/json' \
     -d '{"task":"commit-type","text":"bump deps"}' http://host:8090/v1/classify
```

| Flag de `ai serve` | Por defecto | Qué hace |
|---|---|---|
| `-config` | `~/.config/kling/ai.json` | el registro; las evaluaciones van en `ai-evals/` a su lado |
| `-socket` | `~/.config/kling/ai.sock` | socket Unix 0600 (sin token: solo el propio usuario puede abrirlo) |
| `-listen` | — | TCP en vez del socket; exige token (`$KLING_AI_TOKEN` o `-token-file`) |
| `-no-auth` | — | TCP sin token, solo en loopback (desarrollo) |
| `-idle` | 2m | sin peticiones antes de dormir una réplica (borrarla, pausarla o congelarla, según el `idle` de su modelo) |
| `-max-replicas` | 2 | réplicas por modelo (`max_replicas` del registro manda) |
| `-max-inflight` | 1 | peticiones por réplica antes de pedir otra (`llama-server` atiende una a la vez con `-parallel 1`) |
| `-keepwarm` | 0 | N modelos más usados siempre despiertos (0 = escala a cero pura) |
| `-chispa-mem` | 256 | MiB de modelos Chispa cargados (LRU) |
| `-von-timeout` | 60s | plazo de una escalada |
| `-proxy-timeout` | 5m | plazo de una petición reenviada a un modelo (rutas OpenAI y `/v1/m/…`) |
| `-max-body` | 1 MiB | bytes del cuerpo de una petición |
| `-drain` | 30s | con `SIGTERM`, lo que tienen las peticiones en vuelo para acabar antes de cortarlas |
| `-id` | `default` | etiqueta `ai.gateway=<id>` de sus máquinas |

`SIGHUP` o `kling ai reload` releen el registro sin cortar nada, y dicen qué
cascadas quedan activas, forzadas o rechazadas. La recarga trae también los
modelos y los inquilinos: un token quitado de `tenants` deja de valer en la
siguiente petición y uno añadido entra sin reiniciar (con tokens, el fichero
sigue teniendo que ser 0600; si no lo es, la recarga se rechaza y se queda el
registro anterior). Un gateway que arrancó en el socket **sin token** conserva
ese modo para el dueño del socket aunque una recarga traiga inquilinos: sin
`Authorization` entra como el token principal; con un token, se comprueba contra
los inquilinos (uno desconocido es 401). En TCP no hay esa excepción.

Al recibir `SIGTERM` el gateway deja de aceptar, espera hasta `-drain` a que
acabe lo que está en vuelo y luego duerme sus réplicas: borra las de un modelo
con `idle: remove` y congela las demás. Una réplica que aún tiene una petición
dentro no se toca; la recoge el siguiente arranque, que además borra las
réplicas de modelos que ya no están en el registro.

### Modelos HTTP: `kind http`

Además de los atajos (`chispa`, `von`, `embed`), un modelo puede ser **cualquier
servidor HTTP**: un dorado que el gateway despierta y escala igual que a un VON,
o una URL fija (`upstream`) que kindling no arranca. Los atajos son un `kind
http` con los campos ya puestos (`von` = contrato `openai` en el puerto 8000,
`embed` = `openai-embeddings`).

```json
{
  "models": {
    "decide": {"kind": "http", "snapshot": "decide-v3", "port": 8090,
               "contract": "chispa-decide", "health": {"path": "/healthz"},
               "max_replicas": 4, "tenants": ["acme"]},
    "qwen3-gpu": {"kind": "http", "upstream": "http://10.0.0.20:8000",
                  "contract": "openai", "upstream_model": "Qwen/Qwen3-4B",
                  "auth": {"header": "Authorization", "scheme": "Bearer", "file": "vllm.key"},
                  "limits": {"max_tokens": 1024, "n": 1},
                  "health": {"path": "/health", "every": "10s"}}
  },
  "tenants": [{"name": "acme", "token": "cambia-este-token-de-acme"}]
}
```

(Con tokens de inquilino, `ai.json` tiene que ser 0600.)

| Campo | Qué es |
|---|---|
| `snapshot` / `upstream` | exactamente uno: el dorado del que salen las réplicas, o una URL `http(s)://host[:puerto][/prefijo]` sin credenciales, query ni fragmento |
| `contract` | `openai` (`/v1/chat/completions`, `/v1/completions`), `openai-embeddings` (`/v1/embeddings`), `chispa-decide` (`/v1/decide` de chispa-server) o `raw` (solo las `routes` declaradas) |
| `port` | puerto del servidor dentro de la réplica (8000 por defecto); no con `upstream` (va en la URL) |
| `routes` | `"MÉTODO /ruta"` que se reenvían; vacío = las del contrato; obligatorio con `raw` (hasta 64) |
| `health` | `{path, expect, timeout, every}`: la sonda tras cada restore (por defecto `/health` en `openai*`, `/healthz` en `chispa-decide`, y en `raw` el dial TCP); `every` es el periodo de sondeo de un `upstream` (1 s a 1 h) |
| `max_inflight`, `max_replicas` | peticiones por réplica y tope de réplicas; 0 = los del gateway |
| `idle` | `remove`, `pause` o `freeze` (abajo); no con `upstream` |
| `upstream_model` | lo que se escribe en `"model"` al reenviar (el nombre que espera un vLLM) |
| `auth` | `{header, scheme, file}`: la credencial que **añade el gateway**, leída del fichero (0600, relativo al `ai.json`) en cada petición, así que rotarla no pide recargar; la del cliente no llega nunca al backend |
| `limits` | `{max_tokens, n}` del contrato `openai`: `max_tokens`, `max_completion_tokens` y `n_predict` se recortan al tope (y si no viene ninguno se pone `max_tokens`); un `n` o `best_of` mayor es 400 |
| `tenants` | inquilinos que pueden usar el modelo; vacío = todos. Otro recibe 403 (el token principal siempre entra) |
| `replica_auth` | `network` o `key`: quién protege el puerto de la réplica (abajo). Vacío = `key` si el bloque lleva `auth`, `network` si no (lo de siempre). Solo con `snapshot` |
| `spare` | réplicas de repuesto ya restauradas y tocadas con el calentamiento, fuera del camino de la petición; 0 a `max_replicas` (o a 64). Solo con `snapshot` |
| `pin` | pide al daemon que ancle en RAM el volcado del dorado. Solo con `snapshot` |
| `load_wait` | plazo de carga del modelo al fabricar o rehacer su dorado (`kling ai prime`, `kling ai up`), p. ej. `"20m"`; vacío = automático por tamaño; hasta 6 h. El gateway no lo usa |
| `alias`, `shared` | publicar los bloques de varios inquilinos bajo un nombre común, y compartir a sabiendas un dorado `chispa-decide` (abajo, «Un dorado por inquilino») |
| `ca_file` | PEM (relativo al `ai.json`) con el que se verifica el TLS de un `upstream` `https://` firmado por una CA propia; vacío = raíces del sistema |
| `health.grace` | cuánto se tolera, tras cargar o recargar, que un `upstream` aún no conteste (un vLLM tarda minutos en cargar); hasta 1 h |
| `limits` (ronda 9) | además, `top_logprobs` (también el `logprobs` entero de completions) y `prompt_logprobs` de vLLM: por encima del tope, o negativos, 400; y `timeout`, el plazo de una petición a ese modelo en vez del `-proxy-timeout` (hasta 1 h; vale en cualquier contrato; con un dorado no alarga la espera a las cabeceras de la réplica, que sigue siendo el `-proxy-timeout`) |

`von` y `embed` admiten `health`, `max_inflight`, `idle` y `tenants` (y `von`,
`limits`); un campo que un atajo no usa es un error al cargar, no algo que se
ignora en silencio.

**Rutas.** Los contratos `openai*` se sirven en sus rutas de OpenAI
(`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`), por el
`model` del cuerpo: ahí el gateway aplica topes, semilla y nombre. El resto
(`chispa-decide`, `raw`) va bajo **`/v1/m/{modelo}/{ruta}`**, por ejemplo `POST
/v1/m/decide/v1/decide`: el cuerpo pasa byte a byte, con streaming en los dos
sentidos, y solo se aceptan las rutas declaradas (404 las demás). Así el
`/v1/decide` del gateway (`{task, text}`) y el de chispa-server (`{state,
questions}`) conviven sin pisarse. Del backend al cliente pasan el código, el
cuerpo (acotado) y un `Content-Type` de una lista blanca; ni cookies ni
cabeceras propias.

**Upstream con circuito.** Un `upstream` no tiene réplicas: el gateway le pone
un semáforo de `max_inflight` con cola acotada (4 por hueco; llena = 503) y un
cortacircuitos: tres fallos seguidos (sondas o peticiones sin respuesta) lo
abren y desde ahí cada petición recibe 503 con `Retry-After` al instante hasta
que una sonda pase. Las sondas van a remolque del tráfico (una petición que
encuentra la última sonda más vieja que `health.every` lanza otra en segundo
plano), así que un upstream sin tráfico no cuesta nada.

Al arrancar y en cada `kling ai reload`, el gateway lanza además una **sonda de
carga** por upstream. Si el bloque tiene `upstream_model`, mira también `GET
/v1/models` del upstream: si no lista ese modelo (un vLLM arrancado con otro
rechazaría cada petición), el circuito se abre ya, con un aviso en el log, y
se cierra cuando una sonda lo vea. Que la sonda de carga no tenga respuesta
solo se avisa: no cuenta como fallo. Con `health.grace`, mientras ninguna sonda
haya pasado y dure la gracia, el upstream «está cargando»: cada petición recibe
503 con `Retry-After` al instante, sin reenviarse, y los fallos no abren el
circuito; pasada la gracia, lo de siempre.

**Destinos vetados.** Un `upstream` no puede apuntar a link-local ni a los
metadatos del cloud (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`), a la red
propia de kindling (`172.16.0.0/30`, `172.30.0.0/16`) ni, en el loopback, al
rango de los reenvíos de las máquinas (puertos 29000-29999): son las mismas
reglas que el proxy de credenciales aplica a un upstream fijado. Una IP
literal se rechaza al cargar; un nombre se comprueba en cada conexión, ya
resuelto. Una VPC (`10.0.0.0/8`…) y el loopback en otro puerto valen. Un
`upstream` `http://` fuera del loopback que lleva `auth` da un aviso al cargar:
la clave cruza la red en claro.

**`replica_auth`: quién protege el puerto de una réplica.** El puerto de una
réplica no lo alcanza otra microVM ni la LAN. Pero **en Linux lo alcanza
cualquier proceso local del host, de cualquier usuario**, y **en macOS es un
reenvío en `127.0.0.1`** que alcanza cualquier proceso del Mac. Por eso hay
dos modos, los dos soportados:

| Modo | Qué es | Cuándo usarlo | Coste |
|---|---|---|---|
| `network` | la réplica corre sin clave de entrada; autentica el gateway | host **dedicado** a kindling (nadie más ejecuta código en él: ni otros usuarios, ni CI, ni contenedores ajenos); desarrollo en el Mac | ninguno; se calienta y se usa con repuesto sin más |
| `key` | la réplica exige una clave de entrada; el dorado lleva solo su verificador y el gateway añade la clave con `auth.file` | host compartido o multiusuario; macOS fuera de desarrollo; cualquier backend que sepa autenticar y sirva datos de un inquilino | rotar la clave pide un dorado nuevo; para calentar hace falta la clave de calentamiento |

`key` sin `auth` y `network` con `auth` son errores (contradicciones). El
gateway avisa al cargar (no falla) de cada modelo en `network` en macOS, y en
Linux si `ai.json` declara inquilinos. Lo que `network` no promete, escrito: un
proceso local (y root) del host puede hablar con la réplica sin pasar por el
gateway. Los secretos de salida (credencial del proxy) y los que el proceso
necesita en claro (`secrets`, abajo) no sustituyen a la clave de entrada.

**`secrets`.** Un `kind http` con `snapshot` puede declarar los secretos que
su servidor necesita, con el valor en un fichero 0600 (relativo al
`ai.json`), nunca en el bloque: `deliver: credential` para una clave de
**salida** (el proxy de credenciales la pone; la réplica solo lleva un
marcador, `env`, `domain` y `allow` obligatorios) y `deliver: mmds` para uno
que el proceso necesita **en claro** (se entrega a cada réplica tras
restaurarla; exige `idle` `remove` o `pause`).

**Un dorado por inquilino (Chispa).** chispa-server elige sus bancos por el texto de la pregunta, sin mirar quién
pregunta: dos inquilinos en el mismo proceso comparten bancos. Mientras eso no
cambie, el aislamiento es el proceso: **un dorado por inquilino**, cada uno con
sus bancos dentro, y un `alias` común para que todos usen la misma URL.

```json
{
  "tenants": [{"name": "acme", "token": "cambia-este-token-de-acme"},
              {"name": "globex", "token": "cambia-este-token-de-globex"}],
  "models": {
    "chispa-acme":   {"kind": "http", "snapshot": "chispa-acme-v3", "port": 8090, "contract": "chispa-decide",
                      "tenants": ["acme"], "alias": "chispa", "idle": "remove"},
    "chispa-globex": {"kind": "http", "snapshot": "chispa-globex-v1", "port": 8090, "contract": "chispa-decide",
                      "tenants": ["globex"], "alias": "chispa", "idle": "remove"}
  }
}
```

`POST /v1/m/chispa/v1/decide` con el token de acme llega a `chispa-acme`, y con
el de globex a `chispa-globex`. Un inquilino sin bloque bajo el alias recibe el
mismo 404 que un modelo que no existe (no se revela que el alias lo usan
otros); el token principal no es de ningún inquilino y recibe 400 con los
nombres concretos, que sigue pudiendo usar. Las métricas cuentan el modelo
concreto, no el alias.

Reglas al cargar, por defecto cerradas: con inquilinos declarados, un
`chispa-decide` con dorado sirve a **exactamente un** inquilino, salvo
`"shared": true` (quien lo pone asume bancos comunes, y queda escrito en el
fichero). Dentro de un alias cada inquilino aparece una vez, todo bloque dice a
quién sirve, el alias no puede ser el nombre de un modelo, y dos bloques no
comparten dorado con inquilinos distintos. `alias` es para los contratos de
`/v1/m` (`chispa-decide`, `raw`).

**Coste.** Los dorados de inquilinos distintos no comparten páginas: con las
cifras de EC2, ≈ 160 MiB de memoria (PSS) por inquilino activo (su primera
réplica) y ≈ 20-30 MiB por cada réplica más del mismo inquilino. Con `idle:
remove`, un inquilino inactivo cuesta disco (el volcado de su dorado), no RAM.

**`idle`: qué se hace con una réplica ociosa.**

| Valor | Qué hace | Por defecto en |
|---|---|---|
| `remove` | la borra; la siguiente petición restaura otra del dorado | `von`, `embed`, `kind http` con snapshot |
| `pause` | la pausa dentro del presupuesto de `-paused-mib`; si no cabe o pasa `-paused-for`, la borra. Nunca congela | — |
| `freeze` | la congela (lo de antes de esta versión); la siguiente la descongela | `chispa` con backend microvm |

`remove` es el defecto de los modelos grandes porque una réplica congelada y
descongelada deja de compartir la memoria del dorado con las demás (en Linux
cada thaw trae su propia copia de las páginas); restaurar del dorado cuesta poco
más y todas las réplicas vivas comparten el `mem.file`. Para volver a lo de
antes, `"idle": "freeze"` en el modelo. Una réplica de Chispa (128 MiB, thaw de
~2 ms) sigue congelando.

### Desplegar cualquier servidor: `kling ai model deploy` y `roll`

```sh
kling ai model deploy decide -image chispa-server -contract chispa-decide \
    -port 8090 -health /healthz -warmup warmup.jsonl -cpus 2 -mem 1G
# hace el dorado versionado decide-v1 y escribe su bloque kind http en ai.json
kling ai reload

kling ai model deploy decide -image chispa-server:nuevo -contract chispa-decide -port 8090
# el modelo ya existe: hace decide-v2 y no toca ai.json
kling ai model roll decide            # sirve decide-v2 y borra las réplicas de v1 al vaciarse
kling ai model roll decide -to 1      # vuelta atrás: el dorado viejo se queda
```

`deploy` arranca la imagen (ya construida o importada en el daemon, con los
ficheros dentro), espera a la sonda, manda las peticiones de `-warmup` (JSONL de
`{"method","path","body"}`, cada una tiene que dar 2xx) y congela el dorado
`<modelo>-v<n>`. El dorado se hace con el fabricante genérico
(`internal/dorado`), el mismo que usan `kling ai model add` y `kling ai chispa
deploy`: sin carpetas compartidas, sin secretos en `-e` (un nombre con pinta de
credencial se rechaza salvo `-allow-env`; la credencial la pone el gateway con
`auth`) y con `cpu_pct` = 100 por vCPU grabado en el dorado (`-cpu-pct` para
otro). `roll` apunta el bloque al dorado elegido, pide al gateway que relea
(`-no-reload` para solo reescribir `ai.json`) y borra las réplicas del dorado
viejo cuando dejan de trabajar, como mucho tras `-drain` (30 s).

### Alojar la Chispa nueva (chispa-server) y otros modelos

**chispa-server** (el Chispa de capas, repo `chispa`) es un binario HTTP con
`POST /v1/decide` y `GET /healthz`: contrato `chispa-decide`. Su modo CPU (kNN +
e5) es el caso ideal para un dorado: todo el coste de abrir las sesiones ONNX se
paga una vez al hacerlo, y cada réplica nace con él ya cargado. La imagen tiene
que llevar el binario, el ONNX, los bancos y la configuración, y arrancarlo
escuchando fuera de loopback (`-addr 0.0.0.0:8090`); el gateway es quien pone
el token hacia fuera.

```json
{
  "models": {
    "decide": {"kind": "http", "snapshot": "decide-v1", "port": 8090,
               "contract": "chispa-decide", "health": {"path": "/healthz"},
               "max_inflight": 4}
  }
}
```

```sh
curl --unix-socket ~/.config/kling/ai.sock -H 'Content-Type: application/json' \
     -d '{"state":{…},"questions":[…]}' http://ai/v1/m/decide/v1/decide
```

**Un llama-server o un vLLM OpenAI-compatibles.** En una microVM, con su
dorado, es un `kind http` con contrato `openai` (si es un llama-server con un
GGUF del catálogo, `kling ai model add` ya hace el atajo `von`). Uno con GPU
no cabe en una microVM (Firecracker no da GPU): va como `upstream`, con su
credencial en un fichero 0600 y topes de generación:

```json
{
  "models": {
    "llama":     {"kind": "http", "snapshot": "llama-v1", "port": 8080, "contract": "openai"},
    "qwen3-gpu": {"kind": "http", "upstream": "https://gpu.interna:8000", "contract": "openai",
                  "upstream_model": "Qwen/Qwen3-4B",
                  "auth": {"header": "Authorization", "scheme": "Bearer", "file": "vllm.key"},
                  "limits": {"max_tokens": 1024}, "max_inflight": 16}
  }
}
```

Los dos se piden por `/v1/chat/completions` con `"model": "llama"` o
`"model": "qwen3-gpu"`, y salen en `GET /v1/models`.

### Servicio systemd

`packaging/kling-ai.service` arranca `kling ai serve` después del daemon
(`kling.service`) y se para antes que él, para que el apagado aún pueda dormir
las réplicas. Las opciones van en `/etc/default/kling-ai`
(`KLING_AI_ARGS=-listen 127.0.0.1:8080 -config /etc/kling/ai.json -token-file
/etc/kling/ai.token`); `systemctl reload kling-ai` manda `SIGHUP`. Si subes
`-drain`, sube `TimeoutStopSec` con él (al menos `-drain` + 15 s).

### Prefijos precalculados: `kling ai prime`

El `system` de una tarea (y el texto fijo con el que empieza su `prompt`,
hasta la primera variable) es igual en todas sus peticiones, y en un modelo
pequeño en CPU evaluarlo es casi todo el coste de una respuesta corta: con un
system prompt de ~800 tokens, la primera petición de una réplica recién
restaurada de Qwen2.5-1.5B tarda 4,7 s, de los que ~4,5 son ese prefijo.

```sh
kling ai prime                 # todos los modelos VON del registro
kling ai prime qwen -dry-run   # qué haría
```

`ai prime` rehace el dorado de cada modelo VON con los prefijos de sus tareas
ya evaluados dentro: una réplica restaurada los tiene en la caché de prompts de
`llama-server` (`--cache-ram`, [von.md](von.md)) y solo evalúa lo que cambia.
Medido: la primera petición de la tarea pasa de 4,7 s a 0,4 s, y cambiar de
una tarea a otra en la misma réplica, de 2,8 s a 0,1 s ([von-cpu.md](von-cpu.md)).
El dorado lleva la etiqueta `von.prefixes` con el hash de sus prefijos: repetir
`ai prime` sin cambios en las tareas no hace nada (`-force` lo rehace igual).
No es automático: rehacer un dorado cuesta lo que cargar el modelo, y el daemon
no reemplaza un dorado con réplicas vivas (hay que quitarlas antes: `kling ps`,
`kling rm`). Un prefijo que cambia después solo pierde la ventaja: esa petición evalúa
el prefijo entero, como antes, y desde ahí queda en la caché de la réplica.

### Tareas

Una tarea es de **clasificación** (lleva `chispa`) o de **generación** (lleva
`von`), nunca las dos cosas: lo de clasificar no se acepta en una generación y
al revés, porque una clave que no hace nada es un error que no se ve.

| Campo | Clase | Qué es |
|---|---|---|
| `chispa` | clasificación | el modelo Chispa |
| `escalate_to` | clasificación | el modelo VON de la cascada; solo se activa con una evaluación que la respalde (abajo) |
| `escalate_force` | clasificación | activa la cascada sin ese respaldo; queda escrito en el registro y el gateway lo dice al arrancar |
| `labels` | clasificación | opcional; salen del modelo Chispa |
| `thresholds` | clasificación | umbral τ por clase que sustituye al del modelo (2 = esa clase escala siempre) |
| `top_k` | clasificación | en una escalada, VON solo elige entre las K etiquetas más probables según Chispa (un **reordenador**) |
| `grammar` | clasificación | por defecto `true`: la salida de VON se restringe con una gramática GBNF de `llama-server` a exactamente una etiqueta |
| `audit`, `samples`, `precision` | clasificación | muestras para recalibrar (ver abajo); `audit` exige `escalate_to` |
| `on_von_error` | clasificación | con la cascada activa y VON caído: `chispa` (por defecto: contesta Chispa marcado `degraded`) o `error` (503) |
| `von` | generación | el modelo VON |
| `temperature` | generación | 0,7 por defecto; el cliente puede cambiarla en [0, 2] |
| `system`, `prompt` | las dos | la pregunta a VON. En una escalada: `{labels}`, `{text}`, `{fields}`, `{candidates}` (top-3 de Chispa). En una generación: `{input}` y las `vars` del cliente. Un solo pase: lo que traiga el texto del usuario no se vuelve a expandir |
| `max_tokens` | las dos | 16 en una escalada (una etiqueta); 256 en una generación, y es el tope que puede pedir un cliente (máx. 4096) |
| `json_schema` | generación | un esquema JSON (objeto, hasta 16 KiB): la salida de VON se restringe a JSON que lo cumple (el `json_schema` de `llama-server`, que lo convierte en gramática). El gateway comprueba además que la salida sea JSON; si no (una respuesta cortada por `max_tokens`), 502 con el `finish_reason`. Medido en [von-cpu.md](von-cpu.md): de 19/21 a 21/21 respuestas válidas, ~10 % más lento al generar |

### La cascada, solo con pruebas

```sh
kling ai eval commit-type -data test.jsonl -von qwen      # JSONL etiquetado que Chispa NO vio al entrenar
```

`kling ai eval` pasa el conjunto por Chispa solo y por la cascada con el VON
candidato (una sola pasada: Chispa cuesta µs y solo lo escalado se pregunta a VON;
`-concurrency N` para usar varias réplicas, `-von-alone` para medir también a VON
solo), imprime las cifras y guarda el registro en `ai-evals/<tarea>.json`, junto
al registro de modelos. El gateway activa `escalate_to` solo si ese registro:

- dice que la cascada **gana**: la cascada y Chispa solo contestan lo mismo en todo
  lo que Chispa no escala, así que la diferencia sale entera de lo escalado; se
  cuentan las discrepancias (Chispa acierta y la cascada no, o al revés) y gana si
  acierta más veces donde discrepan con la **prueba de McNemar** exacta de una
  cola, p < 0,05. Con pocos datos no se puede demostrar nada, y eso también es
  una respuesta;
- es de **lo mismo que se va a servir**: el mismo modelo VON y dorado, el mismo
  `.chispa` (por su sha256: reentrenar o recalibrar lo invalida) y los mismos
  ajustes de la escalada (`system`, `prompt`, `top_k`, `grammar`, `max_tokens`,
  `thresholds`, `on_von_error`).

Si no, la tarea sigue funcionando con Chispa y `escalate: true`, y el gateway lo
explica al arrancar, en `kling ai reload`, en `kling ai ls` y en `GET /v1/tasks`:

```
task commit-type: escalate_to "qwen" refused: its eval does not show it beats Chispa alone
  (cascade 0.520 vs Chispa alone 0.640 on 861 examples: Chispa alone is as good or better);
  Chispa answers with escalate: true instead (run `kling ai eval commit-type -data <held-out.jsonl>`,
  or set "escalate_force": true to enable it anyway)
```

Una evaluación nueva que gane activa la cascada en caliente si la tarea ya
tenía `escalate_to`. Nunca hay una llamada a VON escondida: con la cascada
apagada, ni las escaladas ni las auditorías tocan VON.

### Tareas de intención: `/v1/decide` con la capa 3

Una tarea con un bloque `intent` decide una orden corta como intención y huecos
([intent.md](intent.md)): plantillas → Chispa + huecos y, para lo
que dudan, el **codificador de frases** ([codificador.md](codificador.md)): un
modelo `kind: "embed"` (el dorado de `kling ai model add enc-e5 -model
multilingual-e5-small`) que `pkg/scheduler` despierta y congela como a un VON, y
la cabeza `.jenc` que clasifica su vector aquí mismo. Lo que sabe del dominio
(plantillas, valores de los huecos, qué necesita cada intención) sale de un
esquema JSON (`"schema"`) o de un `intent.Domain` en Go que registra el programa
que embebe el gateway (`"domain"`; el ejemplo es la habitación de
[examples/domotica](../examples/domotica/README.md)).

```json
{
  "models": {
    "intent": {"kind": "chispa", "path": "intent.chispa"},
    "enc":    {"kind": "embed", "snapshot": "enc-e5", "max_replicas": 1}
  },
  "tasks": {
    "tickets": {"intent": {"model": "intent", "schema": "tickets.json", "slots": "slots.chispas", "encoder": "enc", "head": "head.jenc"}}
  }
}
```

```sh
kling ai test tickets "the invoices people need a new case"
# open_ticket {"priority":"normal","queue":"billing"}  (layer encoder, p=0.996, confident, 3.32 ms)
#   fast layers said other p=0.957; encoder 3.3 ms
kling ai eval tickets -data test.jsonl    # filas {"text","lang","intent","slots"}: la orden entera
```

`POST /v1/decide` con `{"task", "text", "lang"?}` devuelve la decisión entera
(`decision` = `intent`, `slots`, `layer`, `confident`, `escalate`, `reason`,
`fast_intent`, `encoder_us`, `encoder_error`) y `encoder`, el estado de la capa
3. La capa 3 se enciende **solo con una evaluación que la respalde**, como la
cascada: `kling ai eval <tarea>` pasa las filas por la cascada sin y con el
codificador y el registro (`ai-evals/<tarea>.json`, `kind: "intent"`) la
enciende si contesta bien **más órdenes completas** donde discrepan (McNemar,
p < 0,05) sin más errores confiados que uno por cada cien ganadas, con los
mismos dominio, `.chispa`, `.chispas`, `.jenc` (por su sha256) y dorado. Si no, las capas
rápidas contestan y escalan a `"encoder"`; con ella, lo que tampoco resuelve
sale con `escalate: "von"`. `encoder_force` la enciende sin respaldo. Si la
réplica no contesta en 5 s (despertarla incluido), la decisión escala a VON
con `encoder_error`. Medido en el Mac: 3,1–3,3 ms por `/v1/decide` con la
réplica caliente, 981 ms si estaba congelada por inactividad.

**La capa 2 también puede ser serverless.** Si el modelo de la tarea (`model`) es
`"backend": "microvm"` (un dorado de `kling ai chispa deploy`, ver
[chispa-serverless.md](chispa-serverless.md#domótica-la-capa-2-serverless)),
`/v1/decide` le pregunta a su réplica por el mismo camino que `/v1/classify`
de una tarea microvm: etiquetas y huecos validados contra el registro de
despliegue del dorado, confianza decidida del lado del gateway. Si el dorado
se desplegó con `-slots`, los huecos también los marca la réplica (una sola
ida y vuelta) y `"slots"` sobra en la tarea; si la tarea lo pone, gana el
`.chispas` local, en proceso. La respuesta trae `chispa_replica`
(`{"model", "state": "frozen" | "paused" | "warm" | "new", "wake_ms",
"request_ms"}`) y, si la réplica no contesta o su respuesta no pasa la
validación, la orden escala como una duda con `reason: "chispa_error"` y
`chispa_error` (las plantillas siguen contestando sin tocar la microVM).

## API

| Ruta | Qué hace |
|---|---|
| `POST /v1/classify` | `{task, text, fields?, mode?, explain?}` → `{label, prob, escalate, source, latency_ms, evidence, chispa, von, degraded}` |
| `POST /v1/decide` | lo mismo, con `decision` (= `label`): para rutas de agentes o `allow`/`deny`. En una tarea de intención, la intención con sus huecos (arriba) |
| `POST /v1/generate` | `{task, input, vars?, max_tokens?, temperature?, seed?}` → `{output, model, finish_reason, usage, latency_ms}` |
| `POST /v1/chat/completions`, `POST /v1/completions` | API de OpenAI hacia el modelo de contrato `openai` que nombra `model` (nombre del registro o del dorado; una réplica o un `upstream`), con streaming |
| `POST /v1/embeddings` | API de OpenAI hacia un modelo `openai-embeddings` (`embed` o `kind http`) |
| `POST`/`GET /v1/m/{modelo}/{ruta}` | las rutas declaradas de un modelo `chispa-decide` o `raw`, sin tocar el cuerpo |
| `GET /v1/models` | los modelos de contrato `openai` y `openai-embeddings` del registro (VON, codificadores, `kind http`), sin despertar nada; un inquilino ve solo los que puede usar. Los de `/v1/m/` no salen: no son modelos de OpenAI |
| `GET /v1/tasks` | tareas, su clase, el estado de su cascada, umbrales efectivos (si el modelo está cargado) y muestras |
| `POST /v1/admin/eval` | la evaluación de `kling ai eval` (cuerpo de hasta 64 MiB); solo el token principal |
| `POST /v1/admin/calibrate` | recalibración (`kling ai calibrate`); solo el token principal |
| `POST /v1/admin/reload` | relee el registro; solo el token principal |
| `POST /v1/feedback` | etiqueta humana (token principal) o voto de un maestro externo para una respuesta (`id`) o un texto ([mejora-continua.md](mejora-continua.md)) |
| `POST /v1/admin/review`, `/retrain`, `/promote`, `/rollback` | la mejora continua: cola de revisión, reentreno con puerta, promoción de una versión microvm y vuelta atrás |
| `GET /metrics` | Prometheus; solo el token principal (un token de inquilino recibe 403) |
| `GET /healthz` | sin token |

Cuando un modelo no está disponible (no hay réplica, el circuito está abierto)
el cliente recibe 503 con `Retry-After`; cuando no contestó, 502. Ninguno de los
dos lleva el error interno (direcciones, mensajes del daemon): eso va al log
del gateway.

Una clasificación en la que Chispa duda, sin cascada:

```json
{"task":"commit-type","label":"fix","escalate":true,"prob":0.262,"source":"chispa","latency_ms":0.07,
 "evidence":[{"feature":"w:crash","weight":0.31}, …],
 "chispa":{"label":"fix","prob":0.262,"threshold":0.517,"decision":"escalate",
        "candidates":[{"label":"fix","prob":0.262},{"label":"feat","prob":0.19},{"label":"test","prob":0.12}]}}
```

- `escalate: true` es «Chispa no llegó a su umbral». Con la cascada activa la
  etiqueta es la de VON (`source: von`, y `von` con su respuesta); si no, es la de
  Chispa, con los candidatos y la evidencia para decidir.
- `prob` es la probabilidad calibrada de **Chispa** para la etiqueta devuelta (0 si
  es `unknown`).
- **Mapeo estricto** de la respuesta de VON: su primera línea, sin espacios ni la
  puntuación de alrededor y sin un `label:` delante, tiene que ser una etiqueta
  **entera** (sin distinguir mayúsculas); si no, `unknown`. Con la gramática ya
  es exacta; el mapeo es la defensa porque el invitado no es de fiar.
- `mode: "chispa"` contesta con Chispa aunque dude y aunque la cascada esté activa. VON
  solo, como clasificador, se mide con `kling ai eval -von-alone`, no se sirve.
- `evidence` va siempre que Chispa duda; en las respuestas confiadas, con
  `explain: true` (cuesta reservas de memoria, y lo confiado es el camino de µs).

Métricas (`GET /metrics`): `kling_ai_requests_total{endpoint,task,source}` con
`source` = `chispa` (confiado), `escalated` (Chispa dudó y contestó él, con `escalate:
true`) o `von` (la cascada, o una generación); `kling_ai_chispa_coverage{task}` y
`kling_ai_escalation_rate{task}` (clasificaciones, desde el arranque),
`kling_ai_latency_seconds{task,source}` (histograma de 10 µs a 60 s),
`kling_ai_von_wake_seconds{model,how}` con `how` = `thaw` (estaba congelada),
`restore` (desde el dorado: el arranque en frío) o `adopt`,
`kling_ai_von_replicas{model,state}` (running/frozen, preguntando al daemon como
mucho cada 5 s), `kling_ai_von_errors_total{model,reason}`,
`kling_ai_von_unknown_total`, `kling_ai_degraded_total`, `kling_ai_audits_total`,
`kling_ai_samples`, `kling_ai_proxy_requests_total{model,code}` (con `code` por
clase: `2xx`, `3xx`, `4xx`, `5xx`; el código lo elige el backend y no puede abrir
series nuevas) y las de la caché
de Chispa (`kling_ai_chispa_models_loaded`, `_bytes`, `_loads_total`, `_evictions_total`).
Las etiquetas salen del registro: un cliente no crea series nuevas con nombres
inventados.

## Cómo está hecho

**Escala a cero con `pkg/scheduler`.** El gateway es otro consumidor del mismo
planificador que usa el gateway MCP de kindling-mcp, con el servicio = el dorado
VON. La primera petición a un modelo despierta una réplica: descongela una suya
si la hay (`thaw`) o restaura una nueva del dorado (`restore`); las siguientes la
reutilizan; si todas tienen `-max-inflight` peticiones en vuelo, pide otra hasta
el tope del modelo (y si no cabe, la cola la hace `llama-server` en la menos
cargada); el segador duerme (según su `idle`: borra, pausa o congela) las que
llevan `-idle` sin peticiones **y sin nada en vuelo**. Un 507 del daemon (no cabe) hace que el planificador congele lo
ocioso y reintente; si aun así no cabe, una generación contesta 503 con
`Retry-After`, y una escalada, Chispa marcado `degraded` (o 503). Los modelos Chispa
cuestan 1–5 MB y no se congelan: se cargan la primera vez que se usan y salen por
LRU al pasar de `-chispa-mem`.

Cambios que hicieron falta en `pkg/scheduler` (sirven también al gateway MCP):

- `Port`: el puerto del invitado (8080, el del puente MCP, por defecto). El
  gateway de IA no lo usa: cada dorado da el suyo con `PortFor` (el `port` de su
  bloque), y con `MaxInflightFor`, `IdleFor` y `ReadyFor` su tope por réplica,
  su política de ociosidad y su sonda HTTP (`/health` en `von` y `embed`).
  `OnForget` avisa de cada réplica que sale del planificador (borrada,
  congelada, caída, TTL vencido, sesión cerrada) para que el gateway suelte sus
  conexiones hacia ella.
- `MachineLabels` + `NamePrefix`: las máquinas del gateway llevan
  `ai.gateway=<id>` y se llaman `gw-<dorado>-<azar>`, y **solo adopta las que
  llevan su etiqueta**: una réplica que alguien lanzó con `kling run -from`, o las
  del gateway MCP en el mismo daemon, no se tocan.
- Un scale-out descongela una réplica congelada del servicio antes de restaurar
  otra del dorado. Antes cada ráfaga dejaba máquinas congeladas nuevas en disco
  para siempre (en macOS, cada una con su memoria entera).
- Dos `acquire` concurrentes ya no pueden elegir la misma máquina (se marca bajo
  el candado hasta que se registra).
- `MachineTTL`: el TTL de red de seguridad es configurable (0 = 2 × idle,
  negativo = ninguno). Contra un daemon con la capacidad `renew` es un
  **arrendamiento**: el planificador lo renueva al adoptar, antes de cada thaw
  (también en el scale-out) y en cada vuelta del segador, así que una réplica
  despierta no se congela bajo tráfico y, si el gateway muere, las suyas se
  congelan solas al vencer. Contra un daemon sin `renew` el gateway va sin TTL
  (el daemon lo contaría desde la creación y re-congelaría una réplica vieja a
  media petición), congela al arrancar lo que un gateway anterior con su id dejó
  corriendo, y al salir congela lo suyo.
- `MaxReplicasFor` (tope por servicio), `OnAcquire` (cómo y en cuánto se
  consiguió cada réplica: la métrica de arranques en frío) y `SetPopularityFile`.

**Seguridad.** La lógica es la del gateway MCP: despertar un modelo es ejecutar
código.

- Socket Unix 0600 por defecto (nace 0600 con `umask`, sin ventana), en un
  directorio 0700; no pisa un socket vivo. TCP solo con `-listen` y token:
  `$KLING_AI_TOKEN` o el fichero `ai.token`, generado 0600 si falta y rechazado si
  otros pueden leerlo. Nunca por la línea de comandos. `-no-auth` solo en loopback,
  y entonces el gateway rechaza (403) las peticiones cuyo `Host` u `Origin` no son
  de loopback. Todo `POST` tiene que llevar `Content-Type: application/json` (415).
- Tokens con nombre y cuota (`tenants` en el registro, que entonces tiene que ser
  0600): `max_inflight` da 429 y `max_instances` limita las réplicas que despierta.
  Son reparto justo, no aislamiento. Las rutas `/v1/admin/*` solo con el token
  principal.
- Cuerpos acotados en todas las rutas (`MaxBytesReader`, 1 MiB; texto de una
  tarea, 64 KiB), JSON estricto (campos desconocidos: 400).
- El invitado no es de fiar: conectar tiene 3 s, las cabeceras 64 KiB, la respuesta
  de una escalada 1 MiB y la del proxy 32 MiB y 5 min; del invitado solo pasan
  `Content-Type` y el código. La cabecera `Authorization` del cliente no llega nunca
  a la réplica (el proxy arma su petición desde cero). El gateway no reenvía rutas
  arbitrarias, y aun así rechaza las de control del agente (`guest.IsControlPath`).
- Cada petición a VON lleva una semilla nueva del host (`crypto/rand`) si el
  cliente no pone la suya: las réplicas nacen del mismo volcado de memoria.

## Recalibración en línea (VON como maestro)

[CHISPA-EVAL.md](CHISPA-EVAL.md) midió que los umbrales prometen en validación una
precisión (0,95) que el tráfico real no cumple cuando la distribución cambia
(0,58–0,85). La cascada tiene algo con qué corregirlo: lo que VON contesta.

Solo con la cascada activa (respaldada o forzada): sin ella VON no se toca.

1. Cada escalada con respuesta válida guarda `(etiqueta de Chispa, su probabilidad,
   etiqueta de VON)` en un anillo por tarea (2000). No se guarda el texto.
2. Con `audit` > 0 y la cascada activa, esa fracción de las respuestas
   **confiadas** también se pregunta a VON en segundo plano (una a la vez; si hay otra en vuelo se descarta
   y se cuenta). Sin esto la muestra solo tendría lo que Chispa escaló y no diría nada
   de si lo confiado está bien. Cada muestra pesa 1/probabilidad de haber entrado:
   1 lo escalado, 1/audit lo auditado.
3. `kling ai calibrate <tarea>` parte la muestra en dos mitades alternas, elige
   con la primera el umbral por clase igual que al entrenar (el menor corte cuya
   concordancia ponderada, estimada como aciertos/(n+1), llega al objetivo, con al
   menos `-min-support` muestras) y mide en la segunda la cobertura y la
   concordancia con los umbrales viejos y los nuevos. **Solo escribe** el `.chispa`
   (dejando el anterior en `<ruta>.prev`, y sirviendo el nuevo sin reiniciar) si
   la concordancia sube cuando estaba bajo el objetivo, o si se mantiene sobre él
   y la cobertura sube. `-dry-run` solo informa. Nunca es automático.

Límites, dichos claros:

- **VON también se equivoca.** Lo que se mide es *concordancia con VON*, no
  acierto. Si VON acierta el 60 %, «95 % de concordancia» no es 95 % de aciertos.
  Lo que garantiza es que Chispa solo conteste donde habría dicho lo mismo que el
  modelo al que escalaría: la cascada no empeora a VON, pero tampoco lo mejora.
  Por eso la puerta de la cascada se decide contra etiquetas de verdad
  (`kling ai eval`), no contra VON.
- Solo se tocan los umbrales, no los pesos ni la temperatura: la muestra está
  sesgada hacia lo dudoso y reentrenar con ella necesitaría textos, que no se
  guardan. Reentrenar sigue siendo `kling ai chispa train` con datos etiquetados.
- La muestra vive en memoria: reiniciar el gateway la vacía.
- Si una auditoría se descarta por haber otra en vuelo, la probabilidad real de
  entrar es menor que `audit` y el peso la sobreestima un poco.
- Un umbral forzado en la tarea (`thresholds`) sigue mandando al servir; el
  informe lo marca.

## Cifras en EC2 (c8i.4xlarge, virtualización anidada, Firecracker)

Medido el 2026-11-24 con `scripts/bench/ec2.sh ia` (spot, us-east-1, Debian 12, disco gp3 de 40-60 GB).
Los JSON y los resúmenes completos los deja el banco en `res/`.

**Réplicas de un modelo VON** (Qwen2.5-1.5B Q4_K_M, dorado de 1240 MiB, 4 vCPU):

| Política | Memoria por réplica de más (PSS) | Ociosa → primera respuesta por el gateway, p50 |
|---|---|---|
| `idle: freeze` (descongelar) | 1009 MiB, el 81 % del modelo | 1138 ms |
| restaurar del dorado (`kling run -from`) | 22 MiB | — |
| `idle: remove` (por defecto en `von` y `embed`) | 7 MiB | 995 ms |

Una réplica descongelada no comparte las páginas del modelo con el dorado; una restaurada sí. Por eso
`remove` es el valor por defecto. Restaurar cuesta 75 ms (p50); el resto hasta la primera respuesta es
la propia petición (~900 ms: prompt a 309 tok/s y generación a 31 tok/s). Con la caché de páginas del host
fría (tras `drop_caches`), la primera respuesta sube a 7,4 s, porque el modelo se lee del gp3: un host con
NVMe local o precargar el dorado lo evita. El Q8_0 (1,9 GiB) no terminó de cargar en 5 minutos bajo
virtualización anidada.

**chispa-server** (modo producto en CPU: kNN con e5 en ONNX Runtime, sin L3; dorado de ~2,6 GiB, 4 vCPU),
servido por `/v1/m/<modelo>/v1/decide`:

| | Con claves, sin calentar | Sin claves, calentado |
|---|---|---|
| restaurar → primera decisión, p50 | 471 ms | 430 ms |
| … con la caché del host fría | 1527 ms | 1472 ms |
| descongelar → primera decisión | 908 ms | 553 ms |
| PSS de 1 réplica / de 4 réplicas | 163 / 236 MiB | 161 / 224 MiB |
| 16 clientes, 1 réplica | 393 dec/s, p50 36 ms | 406 dec/s, p50 30 ms |
| 16 clientes, 4 réplicas | 865 dec/s, p50 16 ms | 870 dec/s, p50 16 ms |

El gateway añade ~0,6-0,7 ms (p50, un cliente) sobre chispa-server en el host, que ocupa 2,4 GB de RSS él
solo. 0 errores en todas las cargas. Sin medir todavía: la misma prueba en hierro (`.metal`) y L3 con vLLM
en GPU como `upstream`.

## Cifras (Mac mini M4, backend `vz`)

Conjunto: el reparto temporal de [CHISPA-EVAL.md](CHISPA-EVAL.md) (861 commits de prueba
que Chispa no vio, 10 clases), modelo `words-fields.chispa`. Cascadas medidas con
`kling ai eval` sobre el gateway (gramática activada; prompt de pocos ejemplos,
el mismo para todos los modelos); `top_k: 3` = VON elige entre los tres
candidatos de Chispa. En todas, Chispa contesta confiado el 23,5 % (acierta ahí el
85,2 %) y escala 659 commits, en los que Chispa acierta el **57,5 %**.

| VON candidato (licencia) | `top_k` | Cascada | VON acierta en lo escalado | Solo Chispa acierta / solo la cascada | VON solo | Latencia VON p50 / p95 | La puerta |
|---|---|---|---|---|---|---|---|
| — (Chispa solo) | — | **0,640** | — | — | — | Chispa: p50 6 µs, p95 10 µs | — |
| SmolLM2-360M Q8_0 (Apache) | — | 0,231 | — | — | 0,056 | 218 / 322 ms | (medida antes de la puerta) |
| SmolLM2-360M Q8_0 | 3 | 0,368 | — | — | — | 376 / 474 ms | |
| Qwen2.5-0.5B Q8_0 (Apache) | 3 | 0,429 | 0,299 | 235 / 53 | 0,237 | 333 / 399 ms | rechazada |
| Qwen2.5-1.5B Q4_K_M (Apache) | todas | 0,353 | 0,200 | 325 / 78 | — | 658 / 893 ms | rechazada |
| Qwen2.5-1.5B Q4_K_M | 3 | 0,520 | 0,419 | 163 / 60 | 0,250 | 1230 / 1567 ms | rechazada |
| Qwen2.5-1.5B Q8_0 (Apache) | 3 | 0,498 | 0,390 | 171 / 49 | — | 725 / 1075 ms | rechazada |
| Qwen2.5-3B Q4_K_M (Qwen Research, no comercial) | 3 | **0,540** | 0,445 | 167 / 81 | 0,335 | 2351 / 2900 ms | rechazada |

Lectura honesta: **ninguna cascada llega a Chispa solo** en esta tarea, y la puerta
las rechaza todas (McNemar p = 1: donde discrepan, acierta más Chispa). Subir de
0,5B a 1,5B y a 3B mejora a VON en lo escalado (0,30 → 0,42 → 0,45) pero sigue por
debajo del 0,575 de Chispa ahí, y cuesta de 4 a 7 veces más por escalada. El
reordenador (`top_k: 3`) es imprescindible: sin él, el 1,5B baja a 0,35. Con
todas las etiquetas, los LLM de este tamaño colapsan a pocas etiquetas (SmolLM2
contestaba `chore` en el 96 % sin gramática ni ejemplos). La latencia del 1,5B
Q4_K_M (1,2 s) es mayor que la del Q8_0 (0,7 s) porque el prompt de pocos
ejemplos son ~200 tokens y Q8_0 evalúa el prompt más rápido en esta CPU (291
frente a 177 tok/s, [von.md](von.md)). Parte de estas evaluaciones corrieron con
la VM de Lima cargando modelos al lado: las latencias son pesimistas, los
aciertos no cambian. Una evaluación del 3B con todas las etiquetas quedó sin
terminar.

La conclusión de diseño es la del principio: Chispa decide, VON genera, y la
cascada queda para tareas en las que se demuestre (p. ej. etiquetas que dependen
de entender el texto y no de sus palabras, o un Chispa con pocos datos).

### Escala a cero

Tiempo de una generación de 1 token por el gateway (`/v1/generate`, SmolLM2-360M
Q8_0, `-idle 20s`), visto por el cliente:

| Estado de la réplica | p50 | mín–máx | n |
|---|---|---|---|
| caliente | **9 ms** | 9–10 ms | 5 |
| congelada (`thaw` tras 32 s ociosa) | 1534 ms | 1217–1596 ms | 5 |
| sin réplica (`restore` desde el dorado) | 1651 ms | 1640–1779 ms | 3 |

En macOS el thaw de una réplica congelada copia su memoria entera: cuesta casi
lo mismo que restaurar del dorado, y crece con el modelo (el de una réplica del
1,5B Q4_K_M, 6,3 s en la calibración de abajo, con la VM de Lima ocupando el
Mac; `run -from` del dorado del 1,5B, 1,7 s de thaw). En Linux el dorado se
mapea perezosamente y el thaw es de ~200 ms ([von.md](von.md)).

### Carga mixta con huecos

`bench2.py mixed`: 4 hilos de clasificaciones (`commit-type`, sin cascada, ~50
peticiones/s en total, Poisson) y 2 de generaciones (`summarize` con SmolLM2,
~1 petición/s), en fases de 60 s de carga y 45 s de silencio, con la memoria de
las réplicas (`kling top`, `phys_footprint`) cada 2 s. 5683 peticiones, 0
errores.

| | n | Cliente p50 / p95 / p99 / máx | Servidor p50 / p95 |
|---|---|---|---|
| clasificación (Chispa) | 5585 | 0,48 / 0,78 / 1,03 / 21 ms | **33 / 72 µs** |
| generación (VON) | 98 | 349 / 602 / 2270 / 2732 ms | 349 / 602 ms |

La cola de las generaciones (p99 2,3 s) es el thaw de la primera petición de
cada fase. Memoria de las réplicas en el tiempo:

| t (s) | Fase | Réplicas despiertas / congeladas | Memoria |
|---|---|---|---|
| 0–8 | carga | 1 / 1 | 1,25 GiB |
| 10–72 | carga (la segunda réplica nace con la 2.ª generación simultánea) | 2 / 1 | 2,51–2,58 GiB |
| 75–85 | silencio desde t≈61 | 1 / 2 (el segador congeló una) | 1,29 GiB |
| 87–107 | silencio | 0 / 3 | **0** |
| 109–186 | carga (thaw de las dos) | 2 / 1 | 2,45–2,48 GiB |
| 188–192 | silencio | 1 / 2 | 1,25 GiB |
| 194– | silencio | 0 / 3 | **0** |

(La tercera congelada es una réplica de Qwen2.5-0.5B de una prueba anterior.)
Las clasificaciones no notan nada de esto: Chispa no tiene réplicas.

### Recalibración: una demostración real

Tarea con la cascada **forzada** (`escalate_force`) a Qwen2.5-1.5B Q4_K_M
(`top_k: 3`) y `audit: 0.1`; tráfico: los 861 commits de validación (acierto de
la cascada ahí, 0,664). Muestras: 563 (532 escaladas, 31 auditadas); Chispa coincide
con VON en el 48,7 %.

- `kling ai calibrate -dry-run` (objetivo 0,95): **se niega** — con ese objetivo
  Chispa no contestaría nunca. Con el maestro de 0,5B pasaba lo mismo (concordancia
  12,8 %, se niega también a 0,80).
- `-target 0.8`: concordancia en la mitad de evaluación 0,714 → 0,788, cobertura
  0,344 → 0,128. **Escribe** el `.chispa` (y `.prev`).
- Contra las etiquetas de verdad del conjunto de prueba (`kling ai chispa eval`), el
  recalibrado **es peor**: cobertura 23,5 % → 12,3 %, precisión en lo confiado
  0,851 → 0,783 (la exactitud total no cambia: 0,640).

Es el límite que la sección de recalibración avisa: la concordancia se mide
contra VON, y aquí VON acierta menos que Chispa en lo que duda. Recalibrar con un
maestro peor que el alumno empeora al alumno. Solo tiene sentido con un maestro
que la puerta haya validado contra etiquetas de verdad.

### Laboratorio Linux (Firecracker anidado)

`probe.sh` con el binario de esta rama en la VM de Lima `kling-arm` (Firecracker
1.16.1 bajo KVM anidado; `-idle 20s`, SmolLM2-360M Q8_0), sin nada más corriendo:

- Clasificación confiada: `docs` en 48 ms la primera (carga del `.chispa`), después
  µs; una en la que Chispa duda, con la cascada rechazada por falta de evaluación:
  `escalate: true` en 0,02 ms y ninguna llamada a VON.
- La réplica nace con **TTL 40 s** (2 × idle): el daemon anuncia `renew` y el
  planificador lo renueva como arrendamiento. El segador la congela a los ~20 s
  ociosa, el thaw cuesta **0,41–0,55 s** (4 thaws) y al parar el gateway congela
  la que corría.
- Lo lento es el cómputo anidado, no el gateway: la primera generación de 24
  tokens tras un thaw tarda 56–73 s (fallos de página de los pesos, ~1 ms cada
  uno) y con la réplica caliente 5,7–10 s; en el Mac, 0,35 s. Un primer intento
  con un dorado de 1,5B cargándose al lado pasó de los 5 min del plazo del
  proxy y dio 502, como debe.

La recalibración de umbrales con VON sigue existiendo, pero lo que la
sustituye es el reentreno con puerta de [mejora-continua.md](mejora-continua.md):
maestros validados contra etiquetas de verdad, texto, el oro siempre dentro y
promoción solo si gana en un conjunto de confianza.

## Límites conocidos

- **La cascada no ganó en la única tarea medida** (commits), ni con 3B. La puerta
  lo impide sola, pero el valor de la cascada está sin demostrar: hace falta una
  tarea en la que VON acierte más que Chispa en lo que Chispa duda.
- La puerta compara contra las etiquetas del conjunto que se le pase: si ese
  conjunto se usó para entrenar o calibrar Chispa, Chispa parecerá mejor de lo que es y
  la cascada peor. El registro guarda el sha256 de los datos para poder
  comprobarlo, no lo impide.
- Recalibrar con VON como maestro solo mejora a Chispa si VON acierta más que Chispa
  en lo que duda; con los modelos medidos lo empeoró (arriba).
- En macOS cada réplica congelada guarda su memoria entera en disco y el thaw la
  copia: a partir de 1,5B el thaw cuesta lo que un arranque en frío (2–9 s), y
  cada réplica pesa en RAM bastante más que su VM (2,6 GiB una de 1,5 GiB). En
  Linux el dorado se comparte y el thaw son ~0,3 s, pero en el laboratorio anidado
  la primera petición paga los fallos de página de los pesos (29 s con el 1,5B).
- Contra un daemon sin la capacidad `renew`, las réplicas van sin TTL: si el
  gateway muere sin cerrar, lo que corría sigue corriendo hasta que vuelva a
  arrancar con el mismo `-id` (que lo congela).
- Dos gateways con el mismo `-id` sobre el mismo daemon se pisarían las réplicas.
- La cobertura y el escalado de `/metrics` son desde el arranque, no una ventana;
  las muestras de recalibración viven en memoria.
- `/v1/generate` no hace streaming (para eso, `/v1/chat/completions`).
- `pickInstance` elige réplica antes de marcar la petición en vuelo: dos
  peticiones simultáneas pueden caer en la misma y esperar en su cola aunque
  quepa otra réplica.
