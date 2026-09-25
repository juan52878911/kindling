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
kling models add von-qwen15 -model qwen2.5-1.5b-instruct -quant q4_k_m   # el dorado (von.md)
kling chispa train -data train.jsonl -valid valid.jsonl -o commits.chispa

kling ai serve                          # socket Unix 0600 en ~/.config/kling/ai.sock
kling ai test commit-type "fix crash when the cache is cold"
kling ai generate summarize -var kind="commit message" "fix(parser): handle empty input"
kling ai ls                             # modelos, tareas, cascadas y muestras
```

En TCP, solo con `-listen` explícito y con token:

```sh
kling ai serve -listen 0.0.0.0:8090   # genera ~/.config/kling/ai.token (0600) si no existe
curl -H "Authorization: Bearer $(cat ~/.config/kling/ai.token)" \
     -d '{"task":"commit-type","text":"bump deps"}' http://host:8090/v1/classify
```

| Flag de `ai serve` | Por defecto | Qué hace |
|---|---|---|
| `-config` | `~/.config/kling/ai.json` | el registro; las evaluaciones van en `ai-evals/` a su lado |
| `-socket` | `~/.config/kling/ai.sock` | socket Unix 0600 (sin token: solo el propio usuario puede abrirlo) |
| `-listen` | — | TCP en vez del socket; exige token (`$KLING_AI_TOKEN` o `-token-file`) |
| `-no-auth` | — | TCP sin token, solo en loopback (desarrollo) |
| `-idle` | 2m | sin peticiones antes de congelar una réplica |
| `-max-replicas` | 2 | réplicas por modelo (`max_replicas` del registro manda) |
| `-max-inflight` | 1 | peticiones por réplica antes de pedir otra (`llama-server` atiende una a la vez con `-parallel 1`) |
| `-keepwarm` | 0 | N modelos más usados siempre despiertos (0 = escala a cero pura) |
| `-chispa-mem` | 256 | MiB de modelos Chispa cargados (LRU) |
| `-von-timeout` | 60s | plazo de una escalada |
| `-id` | `default` | etiqueta `ai.gateway=<id>` de sus máquinas |

`SIGHUP` o `kling ai reload` releen el registro sin cortar nada, y dicen qué
cascadas quedan activas, forzadas o rechazadas.

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

### Tareas de domótica: `/v1/decide` con la capa 3

Una tarea con un bloque `domotica` es la decisión de la habitación de demo
([domotica.md](domotica.md)): plantillas → Chispa + huecos en proceso y, para lo
que dudan, el **codificador de frases** ([codificador.md](codificador.md)): un
modelo `kind: "embed"` (el dorado de `kling models add enc-e5 -model
multilingual-e5-small`) que `pkg/scheduler` despierta y congela como a un VON, y
la cabeza `.jenc` que clasifica su vector aquí mismo.

```json
{
  "models": {
    "intent": {"kind": "chispa", "path": "intent.chispa"},
    "enc":    {"kind": "embed", "snapshot": "enc-e5", "max_replicas": 1}
  },
  "tasks": {
    "home": {"domotica": {"intent": "intent", "slots": "slots.chispas", "encoder": "enc", "head": "head.jenc"}}
  }
}
```

```sh
kling ai test home "subir persiana habitación"
# cover_open {"device":"blinds","area":"room"}  (layer encoder, p=0.996, confident, 3.32 ms)
#   fast layers said cover_open p=0.957; encoder 3.3 ms
kling ai eval home -data test.jsonl       # filas del JSONL unificado: texto, idioma, intención y huecos
```

`POST /v1/decide` con `{"task", "text", "lang"?}` devuelve la decisión entera
(`decision` = `intent`, `slots`, `layer`, `confident`, `escalate`, `reason`,
`fast_intent`, `encoder_us`, `encoder_error`) y `encoder`, el estado de la capa
3. La capa 3 se enciende **solo con una evaluación que la respalde**, como la
cascada: `kling ai eval <tarea>` pasa las filas por la cascada sin y con el
codificador y el registro (`ai-evals/<tarea>.json`, `kind: "domotica"`) la
enciende si contesta bien **más órdenes completas** donde discrepan (McNemar,
p < 0,05) sin más errores confiados que uno por cada cien ganadas, con los
mismos `.chispa`, `.chispas`, `.jenc` (por su sha256) y dorado. Si no, las capas
rápidas contestan y escalan a `"encoder"`; con ella, lo que tampoco resuelve
sale con `escalate: "von"`. `encoder_force` la enciende sin respaldo. Si la
réplica no contesta en 5 s (despertarla incluido), la decisión escala a VON
con `encoder_error`. Medido en el Mac: 3,1–3,3 ms por `/v1/decide` con la
réplica caliente, 981 ms si estaba congelada por inactividad.

## API

| Ruta | Qué hace |
|---|---|
| `POST /v1/classify` | `{task, text, fields?, mode?, explain?}` → `{label, prob, escalate, source, latency_ms, evidence, chispa, von, degraded}` |
| `POST /v1/decide` | lo mismo, con `decision` (= `label`): para rutas de agentes o `allow`/`deny`. En una tarea de domótica, la decisión de la habitación (arriba) |
| `POST /v1/generate` | `{task, input, vars?, max_tokens?, temperature?, seed?}` → `{output, model, finish_reason, usage, latency_ms}` |
| `POST /v1/chat/completions`, `POST /v1/completions` | API de OpenAI hacia una réplica del modelo que nombra `model` (nombre del registro o del dorado), con streaming |
| `GET /v1/models` | los modelos VON del registro, sin despertar nada |
| `GET /v1/tasks` | tareas, su clase, el estado de su cascada, umbrales efectivos (si el modelo está cargado) y muestras |
| `POST /v1/admin/eval` | la evaluación de `kling ai eval` (cuerpo de hasta 64 MiB); solo el token principal |
| `POST /v1/admin/calibrate` | recalibración (`kling ai calibrate`); solo el token principal |
| `POST /v1/admin/reload` | relee el registro; solo el token principal |
| `POST /v1/feedback` | etiqueta humana (token principal) o voto de un maestro externo para una respuesta (`id`) o un texto ([mejora-continua.md](mejora-continua.md)) |
| `POST /v1/admin/review`, `/retrain`, `/promote`, `/rollback` | la mejora continua: cola de revisión, reentreno con puerta, promoción de una versión microvm y vuelta atrás |
| `GET /metrics` | Prometheus |
| `GET /healthz` | sin token |

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
`kling_ai_von_replicas{model,state}` (running/warm, preguntando al daemon como
mucho cada 5 s), `kling_ai_von_errors_total{model,reason}`,
`kling_ai_von_unknown_total`, `kling_ai_degraded_total`, `kling_ai_audits_total`,
`kling_ai_samples`, `kling_ai_proxy_requests_total{model,code}` y las de la caché
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
cargada); el segador congela las que llevan `-idle` sin peticiones **y sin nada
en vuelo**. Un 507 del daemon (no cabe) hace que el planificador congele lo
ocioso y reintente; si aun así no cabe, una generación contesta 503 con
`Retry-After`, y una escalada, Chispa marcado `degraded` (o 503). Los modelos Chispa
cuestan 1–5 MB y no se congelan: se cargan la primera vez que se usan y salen por
LRU al pasar de `-chispa-mem`.

Cambios que hicieron falta en `pkg/scheduler` (sirven también al gateway MCP):

- `Port`: el puerto del invitado (8000 para VON; 8080, el del puente MCP, por
  defecto).
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
  otros pueden leerlo. Nunca por la línea de comandos. `-no-auth` solo en loopback.
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
  guardan. Reentrenar sigue siendo `kling chispa train` con datos etiquetados.
- La muestra vive en memoria: reiniciar el gateway la vacía.
- Si una auditoría se descarta por haber otra en vuelo, la probabilidad real de
  entrar es menor que `audit` y el peso la sobreestima un poco.
- Un umbral forzado en la tarea (`thresholds`) sigue mandando al servir; el
  informe lo marca.

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
- Contra las etiquetas de verdad del conjunto de prueba (`kling chispa eval`), el
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
