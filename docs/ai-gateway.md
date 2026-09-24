# Gateway de IA: muchos modelos listos, ninguno encendido 24/7

`kling ai serve` es un gateway para decisiones pequeñas —clasificar eventos,
enrutar peticiones de agentes, filtrar o moderar entradas— y para servir LLM
pequeños con la API de OpenAI. Junta las dos piezas de v0.11.0:

- **JEV** ([jev.md](jev.md)): un clasificador lineal de ~1 MB que vive **dentro**
  del proceso del gateway. Contesta en microsegundos.
- **VON** ([von.md](von.md)): un LLM de 360M–500M parámetros en una microVM,
  restaurado de un dorado congelado con el modelo ya cargado. Se despierta con la
  primera petición y se **congela al quedarse ocioso** (0 CPU; en Linux su
  memoria vuelve a su fichero).

Una **tarea** los encadena en **cascada**: JEV contesta si está seguro (su
probabilidad calibrada supera el umbral de la clase); si no, **escala** a VON.
Lo que VON contesta en lo escalado se guarda para **recalibrar** los umbrales de
JEV con el tráfico real, a petición (`kling ai calibrate`).

```
cliente ──HTTP──> kling ai serve ──┬── JEV (en proceso, µs)
                                   │      confiado → respuesta
                                   │      duda ↓
                                   └── pkg/scheduler ──socket──> daemon ──> réplica VON (microVM)
                                          thaw al llegar, freeze al quedarse ociosa,
                                          réplicas por concurrencia, tope por modelo
```

## Uso

El registro es un fichero JSON (`~/.config/kling/ai.json` por defecto):

```json
{
  "models": {
    "commits": {"kind": "jev", "path": "commits.jev"},
    "smol":    {"kind": "von", "snapshot": "von-smol", "max_replicas": 2}
  },
  "tasks": {
    "commit-type": {
      "jev": "commits", "von": "smol", "top_k": 3, "audit": 0.05,
      "system": "You classify git commit messages by their Conventional Commits type. Reply with exactly one type.",
      "prompt": "Allowed answers: {labels}\n\nCommit message:\n{text}\n{fields}\nType:"
    }
  }
}
```

```sh
kling models add von-smol -model smollm2-360m-instruct   # el dorado (ver von.md)
kling jev train -data train.jsonl -valid valid.jsonl -o commits.jev

kling ai serve                        # socket Unix 0600 en ~/.config/kling/ai.sock
kling ai test commit-type "fix crash when the cache is cold"
kling ai ls                           # modelos, tareas y muestras guardadas
kling ai calibrate commit-type -dry-run
```

En TCP, solo con `-listen` explícito y con token:

```sh
kling ai serve -listen 0.0.0.0:8090   # genera ~/.config/kling/ai.token (0600) si no existe
curl -H "Authorization: Bearer $(cat ~/.config/kling/ai.token)" \
     -d '{"task":"commit-type","text":"bump deps"}' http://host:8090/v1/classify
```

| Flag de `ai serve` | Por defecto | Qué hace |
|---|---|---|
| `-config` | `~/.config/kling/ai.json` | el registro |
| `-socket` | `~/.config/kling/ai.sock` | socket Unix 0600 (sin token: solo el propio usuario puede abrirlo) |
| `-listen` | — | TCP en vez del socket; exige token (`$KLING_AI_TOKEN` o `-token-file`) |
| `-no-auth` | — | TCP sin token, solo en loopback (desarrollo) |
| `-idle` | 2m | sin peticiones antes de congelar una réplica |
| `-max-replicas` | 2 | réplicas por modelo (`max_replicas` del registro manda) |
| `-max-inflight` | 1 | peticiones por réplica antes de pedir otra (`llama-server` atiende una a la vez con `-parallel 1`) |
| `-keepwarm` | 0 | N modelos más usados siempre despiertos (0 = escala a cero pura) |
| `-jev-mem` | 256 | MiB de modelos JEV cargados (LRU) |
| `-von-timeout` | 60s | plazo de una escalada |
| `-id` | `default` | etiqueta `ai.gateway=<id>` de sus máquinas |

`SIGHUP` o `kling ai reload` releen el registro sin cortar nada.

### Campos de una tarea

| Campo | Qué es |
|---|---|
| `jev`, `von` | los modelos; al menos uno. Sin `von`, JEV contesta siempre (marcado `degraded` si duda). Sin `jev`, todo va a VON y `labels` es obligatorio |
| `labels` | las etiquetas válidas (con JEV salen del modelo) |
| `system`, `prompt` | la pregunta a VON. Variables: `{labels}`, `{text}`, `{fields}`, `{candidates}` (top-3 de JEV con su probabilidad). Se sustituyen en un solo pase: un `{labels}` dentro del texto del usuario no se expande |
| `top_k` | VON solo elige entre las K etiquetas más probables según JEV: un **reordenador**. Ver cifras |
| `thresholds` | umbral τ por clase que sustituye al del modelo (2 = esa clase escala siempre) |
| `grammar` | por defecto `true`: la salida de VON se restringe con una gramática GBNF de `llama-server` a exactamente una etiqueta |
| `audit` | fracción de respuestas **confiadas** de JEV que también se preguntan a VON en segundo plano, solo para recalibrar |
| `samples` | tamaño del anillo de muestras (2000) |
| `precision` | objetivo al recalibrar (por defecto el del modelo, o 0,95) |
| `max_tokens` | de la respuesta de VON (16) |
| `on_von_error` | `jev` (por defecto: contesta JEV marcado `degraded`) o `error` (503) |

## API

| Ruta | Qué hace |
|---|---|
| `POST /v1/classify` | `{task, text, fields?, mode?, explain?}` → `{label, prob, source, latency_ms, evidence, jev, von, degraded}` |
| `POST /v1/decide` | lo mismo, con `decision` (= `label`): para rutas de agentes o `allow`/`deny` |
| `POST /v1/chat/completions`, `POST /v1/completions` | API de OpenAI hacia una réplica del modelo que nombra `model` (nombre del registro o del dorado), con streaming |
| `GET /v1/models` | los modelos VON del registro, sin despertar nada |
| `GET /v1/tasks` | tareas, umbrales efectivos (si el modelo está cargado) y muestras |
| `POST /v1/admin/calibrate` | recalibración (`kling ai calibrate`); solo el token principal |
| `POST /v1/admin/reload` | relee el registro; solo el token principal |
| `GET /metrics` | Prometheus |
| `GET /healthz` | sin token |

Respuesta de una escalada:

```json
{"task":"commit-type","label":"fix","prob":0.39,"source":"von","latency_ms":41.2,
 "evidence":[{"feature":"w:crash","weight":0.31}, …],
 "jev":{"label":"fix","prob":0.39,"threshold":0.517,"decision":"escalate",
        "candidates":[{"label":"fix","prob":0.39},{"label":"test","prob":0.21},{"label":"feat","prob":0.12}]},
 "von":{"model":"smol","answer":"fix","latency_ms":40.8}}
```

- `source` es `jev` o `von`. `prob` es la probabilidad calibrada de **JEV** para la
  etiqueta devuelta (0 si no hay JEV o si es `unknown`).
- **Mapeo estricto**: la respuesta de VON se toma de su primera línea, sin
  espacios ni la puntuación de alrededor (comillas, asteriscos, punto final) y sin
  un `label:` delante, y tiene que ser una etiqueta **entera** (sin distinguir
  mayúsculas). Si no, `unknown`. No se busca la etiqueta dentro de una frase: «not
  a fix, a feat» acertaría o no por casualidad. Con la gramática la respuesta ya
  es exacta; el mapeo es la defensa porque el invitado no es de fiar.
- `mode`: `jev` (solo JEV, aunque dude) y `von` (siempre VON, con todas las
  etiquetas y sin pistas de JEV) existen para medir cada escalón por separado.
- `evidence` va siempre en las escaladas; en las respuestas confiadas, con
  `explain: true` (cuesta reservas de memoria, y lo confiado es el camino de µs).

Métricas (`GET /metrics`): `kling_ai_requests_total{endpoint,task,source}`,
`kling_ai_jev_coverage{task}` y `kling_ai_escalation_rate{task}` (desde el
arranque), `kling_ai_latency_seconds{task,source}` (histograma de 10 µs a 60 s),
`kling_ai_von_wake_seconds{model,how}` con `how` = `thaw` (estaba congelada),
`restore` (arranque desde el dorado: el arranque en frío) o `adopt`,
`kling_ai_von_replicas{model,state}` (running/warm, preguntando al daemon como
mucho cada 5 s), `kling_ai_von_errors_total{model,reason}`,
`kling_ai_von_unknown_total`, `kling_ai_degraded_total`, `kling_ai_audits_total`,
`kling_ai_samples`, `kling_ai_proxy_requests_total{model,code}` y las de la caché
de JEV (`kling_ai_jev_models_loaded`, `_bytes`, `_loads_total`, `_evictions_total`).
Las etiquetas salen del registro: un cliente no crea series nuevas con nombres
inventados.

## Cómo está hecho

**Escala a cero con `pkg/scheduler`.** El gateway es otro consumidor del mismo
planificador que usa el gateway MCP de kindling-mcp, con el servicio = el dorado
VON. La primera escalada de un modelo despierta una réplica: descongela una suya
si la hay (`thaw`) o restaura una nueva del dorado (`restore`); las siguientes la
reutilizan; si todas tienen `-max-inflight` peticiones en vuelo, pide otra hasta
el tope del modelo (y si no cabe, la cola la hace `llama-server` en la menos
cargada); el segador congela las que llevan `-idle` sin peticiones **y sin nada
en vuelo**. Un 507 del daemon (no cabe) hace que el planificador congele lo
ocioso y reintente; si aun así no cabe, la escalada contesta con JEV marcado
`degraded` (o 503 con `Retry-After`). Los modelos JEV cuestan 1–5 MB y no se
congelan: se cargan la primera vez que se usan y salen por LRU al pasar de
`-jev-mem`.

Cambios que hicieron falta en `pkg/scheduler` (sirven también al gateway MCP):

- `Port`: el puerto del invitado (8000 para VON; 8080, el del puente MCP, por
  defecto).
- `MachineLabels` + `NamePrefix`: las máquinas del gateway llevan
  `ai.gateway=<id>` y se llaman `gw-<dorado>-<azar>`, y **solo adopta las que
  llevan su etiqueta**: una réplica que alguien lanzó con `kling run -from
  von-smol`, o las del gateway MCP en el mismo daemon, no se tocan.
- Un scale-out descongela una réplica congelada del servicio antes de restaurar
  otra del dorado. Antes cada ráfaga dejaba máquinas congeladas nuevas en disco
  para siempre (en macOS, cada una con su memoria entera: 1,3–1,7 GB).
- Dos `acquire` concurrentes ya no pueden elegir la misma máquina (se marca bajo
  el candado hasta que se registra).
- `MachineTTL`: el TTL de red de seguridad es opcional. El daemon lo cuenta desde
  que creó la máquina y un thaw no lo reinicia (a propósito, para los sandboxes),
  así que una réplica de más de 2 × idle que se despierta volvía a congelarse en
  la siguiente vuelta del vigilante del daemon **aunque estuviera atendiendo**. El
  gateway de IA va sin TTL y a cambio congela al arrancar lo que un gateway
  anterior con su id dejara corriendo, y al salir congela lo suyo.
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

[JEV-EVAL.md](JEV-EVAL.md) midió que los umbrales prometen en validación una
precisión (0,95) que el tráfico real no cumple cuando la distribución cambia
(0,58–0,85). La cascada tiene algo con qué corregirlo: lo que VON contesta.

1. Cada escalada con respuesta válida guarda `(etiqueta de JEV, su probabilidad,
   etiqueta de VON)` en un anillo por tarea (2000). No se guarda el texto.
2. Con `audit` > 0, esa fracción de las respuestas **confiadas** también se
   pregunta a VON en segundo plano (una a la vez; si hay otra en vuelo se descarta
   y se cuenta). Sin esto la muestra solo tendría lo que JEV escaló y no diría nada
   de si lo confiado está bien. Cada muestra pesa 1/probabilidad de haber entrado:
   1 lo escalado, 1/audit lo auditado.
3. `kling ai calibrate <tarea>` parte la muestra en dos mitades alternas, elige
   con la primera el umbral por clase igual que al entrenar (el menor corte cuya
   concordancia ponderada, estimada como aciertos/(n+1), llega al objetivo, con al
   menos `-min-support` muestras) y mide en la segunda la cobertura y la
   concordancia con los umbrales viejos y los nuevos. **Solo escribe** el `.jev`
   (dejando el anterior en `<ruta>.prev`, y sirviendo el nuevo sin reiniciar) si
   la concordancia sube cuando estaba bajo el objetivo, o si se mantiene sobre él
   y la cobertura sube. `-dry-run` solo informa. Nunca es automático.

Límites, dichos claros:

- **VON también se equivoca.** Lo que se mide es *concordancia con VON*, no
  acierto. Si VON acierta el 60 %, «95 % de concordancia» no es 95 % de aciertos.
  Lo que garantiza es que JEV solo conteste donde habría dicho lo mismo que el
  modelo al que escalaría: la cascada no empeora a VON, pero tampoco lo mejora.
- Solo se tocan los umbrales, no los pesos ni la temperatura: la muestra está
  sesgada hacia lo dudoso y reentrenar con ella necesitaría textos, que no se
  guardan. Reentrenar sigue siendo `kling jev train` con datos etiquetados.
- La muestra vive en memoria: reiniciar el gateway la vacía.
- Si una auditoría se descarta por haber otra en vuelo, la probabilidad real de
  entrar es menor que `audit` y el peso la sobreestima un poco.
- Un umbral forzado en la tarea (`thresholds`) sigue mandando al servir; el
  informe lo marca.

## Cifras (Mac mini M4, backend `vz`, medidas hasta la pausa)

Conjunto: el reparto temporal de [JEV-EVAL.md](JEV-EVAL.md) (861 commits de prueba,
10 clases), modelo `words-fields.jev`, peticiones secuenciales por TCP con token.
Latencia «servidor» = `latency_ms` del gateway; la del cliente suma ~0,1 ms de HTTP local.

| | Exactitud | Contesta JEV | Latencia |
|---|---|---|---|
| JEV solo (`mode=jev`) | **0,640** | 100 % | p50 **6 µs**, p95 10 µs (cliente 0,13 ms) |
| VON solo, SmolLM2-360M Q8 (`mode=von`, few-shot, gramática) | 0,056 | — | p50 215 ms, p95 303 ms |
| VON solo, Qwen2.5-0.5B Q8 | 0,237 | — | p50 184 ms, p95 257 ms |
| Cascada JEV → SmolLM2 | 0,231 | 23,5 % (0,851 de acierto ahí) | JEV 11 µs; VON p50 218 / p95 322 ms |
| Cascada JEV → SmolLM2, `top_k: 3` | 0,368 | 23,5 % | VON p50 376 / p95 474 ms |
| Cascada JEV → Qwen2.5 | 0,405 | 23,5 % | VON p50 180 / p95 259 ms |
| Cascada JEV → Qwen2.5, `top_k: 3` | **0,429** | 23,5 % | VON p50 334 / p95 405 ms |

Lectura honesta: **en esta tarea la cascada empeora a JEV solo** (0,43 frente a
0,64). En lo que JEV escala, JEV acierta el 57,5 % y los VON el 4–30 %: SmolLM2
y Qwen2.5-0.5B, con prompt de pocos ejemplos, colapsan a una o dos etiquetas
(SmolLM2 contesta `chore` en el 96 % de los casos sin gramática ni ejemplos). El
modo reordenador (`top_k: 3`) ayuda (+0,14 con SmolLM2, +0,02 con Qwen) pero no
basta. Un binario `fix` contra el resto dio lo mismo en una muestra de 150 (JEV
0,79; VON 0,33–0,39). Por eso la cascada es por tarea y se mide antes con
`mode=jev|von`; y por eso `kling ai calibrate` se niega a escribir umbrales que
apaguen a JEV cuando VON discrepa de casi todo.

Escala a cero, medido en la misma sesión: la primera escalada restauró la
réplica del dorado en **2,6 s** (primer `restore` tras arrancar el daemon, con el
`mem.file` fuera de la caché), la siguiente contestó en **16–39 ms**; tras 20 s
ociosa el segador la congeló y el thaw de vuelta costó **1,5–1,8 s** (`thaw`
de `vz`, que copia la memoria). Una réplica congelada de SmolLM2 ocupa 488 MiB en
disco en macOS y 0 de RAM; la de Qwen Q8, 752 MiB. Con audit y peticiones
seguidas, el scale-out creó una segunda réplica de SmolLM2 (tope 2) y al parar
el gateway congeló las que corrían.

## Límites conocidos

- **Un LLM de 360M no es buen clasificador de diez clases** (cifras arriba): la
  cascada solo gana si lo que VON contesta en lo escalado acierta más que JEV ahí.
  Medirlo por tarea con `mode=jev|von` antes de activarla.
- En macOS cada réplica congelada guarda su memoria entera en disco (1,3–1,7 GB)
  y restaurar la copia entera; en Linux el dorado se comparte y el `mem.file` es
  disperso ([von.md](von.md)).
- Sin TTL en las réplicas: si el gateway muere sin cerrar, lo que corría sigue
  corriendo hasta que se vuelva a arrancar con el mismo `-id` (que lo congela).
- Dos gateways con el mismo `-id` sobre el mismo daemon se pisarían las réplicas.
- La cobertura y el escalado de `/metrics` son desde el arranque, no una ventana.
- `pickInstance` elige réplica antes de marcar la petición en vuelo: dos
  peticiones simultáneas pueden caer en la misma y esperar en su cola aunque
  quepa otra réplica.

## Estado (pausa 2026-09-23)

**Hecho y en commits** (rama `claude/gateway-ia`, sin empujar): `pkg/aigw`
(registro, caché LRU de JEV, cascada, `top_k`, gramática, mapeo estricto, proxy
OpenAI con streaming, métricas, auditoría y recalibración con mitad de evaluación),
`kling ai serve|ls|test|calibrate|reload`, cambios en `pkg/scheduler` (Port,
MachineLabels, NamePrefix, MachineTTL, MaxReplicasFor, OnAcquire, réplicas
congeladas reutilizadas en el scale-out, marca contra adopciones dobles), tests
unitarios con daemon y llama-server falsos (cascada, modos, VON caído, auth y
cuotas, límites, proxy, caché, config, calibración, escala a cero con el
planificador real), este documento y las secciones del README. `go test -race`
de `pkg/aigw` y `pkg/scheduler` verde; cross-compila linux/amd64, linux/arm64 y
darwin/arm64. `make test`: verde salvo el `TestDescubrimiento` de `pkg/plugin`
(tiempo, conocido).

**Medido**: la tabla de arriba (exactitud y latencias de JEV solo, VON solo y
las cuatro cascadas, completas sobre 861) y los tiempos sueltos de restore, thaw
y réplica caliente.

**A medias / sin hacer**:
- Fase de arranques en frío repetida (5 thaw + 3 restore) y fase de carga mixta
  con memoria en el tiempo (`bench.py cold` y `bench.py mixed`, escritos en el
  scratchpad `gw/bench.py`, no ejecutados).
- Demostración de `kling ai calibrate` sobre muestras reales (la tarea
  `commit-q` tenía `audit: 0.1`; no se llegó a llamar).
- Comprobación en el laboratorio Linux (binario y script `gw/lab/probe.sh`
  preparados; no se ejecutó nada allí).
- CHANGELOG v0.11.0 (grupo aparte para el gateway) sin escribir.

**Siguientes pasos exactos**:
1. Mac: `kling daemon -root <scratchpad>/mac-e2e/root -socket /tmp/gw-kl.sock`,
   `kling ai serve -config <scratchpad>/gw/e2e/ai.json -listen 127.0.0.1:18080
   -token-file <scratchpad>/gw/e2e/ai.token -idle 20s` y
   `TOKENFILE=… KLING=… TEST=<scratchpad>/gw/jev/test.jsonl OUT=… python3 gw/bench.py cold`
   y luego `… mixed`; pasar las cifras aquí.
2. `kling ai calibrate commit-q -dry-run` tras una pasada en cascada de `commit-q`.
3. Laboratorio: copiar `kling` linux/arm64 de esta rama, `ai.json`, `commits.jev`
   y `probe.sh` a `/tmp/gw-probe` y ejecutar `probe.sh` (máquinas `gw-von-smol-*`,
   id `gw-lab`); borrar `/tmp/gw-probe` y las `gw-*` al terminar.
4. CHANGELOG, `verificador-kindling`, PR.

**Qué queda en disco**: en el Mac, la raíz de datos `mac-e2e/root` del scratchpad
con las imágenes y dorados `von-smol`, `von-qwen-q4`, `von-qwen-q8` y la base
`glibc-trixie` (conservar); ninguna máquina, daemon, gateway ni proceso `kling-vz`
propio corriendo (el proceso de Virtualization.framework que sigue vivo es la VM
de Lima `kling-arm`). Datos del banco en el scratchpad `gw/` (`jev/` con el
conjunto de commits, `e2e/` con registro, token y resultados `res2/`). En el
laboratorio no queda nada de este trabajo.
