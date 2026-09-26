# Mejora continua: que Chispa aprenda lo que otros resolvieron por él

Cuando Chispa duda, alguien más lento contesta: VON en la cascada, el
codificador en una tarea de domótica, la capa lenta del propio cliente
(`escalate: true`, «quien llama decide») o una persona. El bucle de mejora
continua guarda esos casos, entrena un Chispa nuevo con ellos y **solo lo pone
a servir si gana en un conjunto de confianza**. La siguiente vez, Chispa
decide rápido lo que antes escalaba.

Todo es opt-in por tarea (bloque `learn` del registro de
[ai-gateway.md](ai-gateway.md)) y nunca es automático: `kling ai retrain` lo
lanza una persona o un cron, y la puerta decide.

```
petición ─> Chispa ─ confiado ─────────────────────────────> respuesta (µs)
              │
              └─ duda ─> respuesta con escalate/id ─┬─> VON / codificador / cliente
                          │                         │     (su respuesta = voto de maestro)
                          └─ captura (en 2.º plano) └─> persona: kling ai review / feedback
                                    │                         (la verdad)
                                    ▼
                  ai-data/<tarea>/  captures/AAAA-MM-DD.jsonl   feedback.jsonl
                                    │
                  kling ai retrain: oro + humano + maestros VALIDADOS (filtro de acuerdo, tope por clase)
                                    │
                                    ▼  modelo sombra
                  puerta en el conjunto de CONFIANZA (McNemar + precisión)
                         │ gana                        │ no gana
                         ▼                             ▼
                  <modelo>@vN.chispa, .prev,     se queda en @shadow.chispa;
                  swap atómico (o dorado -vN)    se sirve lo de antes
```

## Por qué el intento anterior empeoró a Chispa

`kling ai calibrate` (VON como maestro, [ai-gateway.md](ai-gateway.md#recalibración-en-línea-von-como-maestro))
bajó la cobertura de 23,5 % a 12,3 % y la precisión confiada de 0,851 a 0,783.
Cuatro causas, y lo que hace este bucle con cada una:

| Causa | Remedio aquí |
|---|---|
| El maestro era **peor que el alumno** donde importaba (VON 42 % frente a Chispa 57,5 % en lo escalado) | La etiqueta de un maestro solo entra si ese maestro está **validado contra etiquetas de verdad** (una auditoría humana al azar, o un registro de `kling ai eval` que lo respalde) y además la muestra pasa un **filtro de acuerdo**. Por defecto solo una persona es verdad |
| Se medía **concordancia** con el maestro, no acierto | Toda puerta se mide contra el **conjunto apartado de confianza** (`learn.heldout`), que ningún entrenamiento ve (guarda de fugas incluida) |
| Muestra **sesgada** hacia lo dudoso | Cada reentreno lleva **entero el oro** (`learn.gold`, nunca se descarta) y un **tope por clase** para lo nuevo; lo de los maestros nunca calibra |
| Solo se movían **umbrales**, sin texto | Se guarda el **texto** (opt-in, acotado, con filtro de secretos) o solo su hash, y se reentrena el modelo entero |

## Configuración

```json
{
  "models": {"intent": {"kind": "chispa", "path": "intent.chispa"}},
  "tasks": {
    "home": {
      "chispa": "intent",
      "learn": {
        "capture": "text",
        "gold": "gold.jsonl",
        "valid": "valid.jsonl",
        "heldout": "heldout.jsonl"
      }
    }
  }
}
```

| Campo | Por defecto | Qué es |
|---|---|---|
| `capture` | off | `text` (texto filtrado), `hash` (solo sha256: nada legible en disco; solo entrena lo que una persona mande con su texto) u `off` |
| `max_mb`, `max_days` | 64, 30 | tope del almacén de capturas por tarea; lo más viejo sale primero |
| `gold` | — | el ancla: el JSONL de entrenamiento. Obligatorio para reentrenar |
| `heldout` | — | etiquetas de verdad que nada entrena. Obligatorio para reentrenar |
| `valid` | 10 % del oro | validación para la temperatura y los umbrales (más un 20 % estable de lo humano) |
| `min_votes` | 2 | votos de maestros validados, todos de acuerdo, para entrar sin revisión |
| `top_k` | 3 | la etiqueta del maestro tiene que estar entre las K de Chispa |
| `teacher_weight` | 0,5 | peso de una muestra de maestro (oro y humano pesan 1; `weight` nuevo del JSONL de `kling ai chispa train`) |
| `max_per_class` | máx(50, oro de la clase) | muestras de maestros por clase y reentreno |
| `min_checks`, `accept_precision` | 30, 0,9 | lo que exige la validación de un maestro (abajo) |
| `trust_teachers` | — | maestros forzados sin validar: queda escrito y cada informe dice qué opinan de ellos las revisiones |
| `von_votes` | 0 | respuestas extra de VON (T = 0,4) por escalada, en segundo plano y una a la vez: la autoconsistencia como segundo voto |
| `escalation_ms` | medido | coste de una escalada para la estimación del tiempo ahorrado |
| `window_minutes` | 60 | ventana de la tasa de escalado de `/metrics` |

Vale para tareas de clasificación (con o sin cascada, en proceso o
[microvm](chispa-serverless.md)) y de [domótica](domotica.md) (el modelo que
aprende es el de intención; se capturan las escaladas por intención —Chispa
dudó o dijo «fuera de ámbito»—, no las de huecos o de órdenes múltiples).

## Captura

Con `capture`, cada respuesta de la tarea lleva un `id`, y cada escalada se
guarda en `ai-data/<tarea>/captures/<día>.jsonl` (junto al registro;
directorio 0700, ficheros 0600):

```json
{"id":"df929680f80a-1b","task":"home","ts":"2026-09-25T00:03:05Z","kind":"escalated",
 "text":"baja un poco la persiana del dormitorio","text_sha256":"…","fields":{"lang":"es"},
 "chispa":{"version":"v2","sha256":"…","label":"cover_close","prob":0.336,
           "top":[{"label":"cover_close","prob":0.336},{"label":"cover_set_position","prob":0.21}, …]},
 "teachers":[{"name":"qwen","label":"cover_set_position"}]}
```

- `kind`: `escalated`, o `audit` (una auditoría de la cascada: Chispa contestó
  confiado y VON dijo otra cosa o lo mismo).
- `teachers`: la respuesta de VON en la cascada (y las de `von_votes`), o la
  del codificador si la capa 3 contestó confiada. Sin cascada, vacío: el voto
  lo trae el cliente después.
- La petición solo **encola** (sin bloquear; cola llena = se pierde y se
  cuenta). Filtrar secretos, serializar y escribir lo hace un único escritor
  en segundo plano. Dedup por texto dentro del día.
- **Filtro de secretos**: claves con nombre (`password: …`, `token=…`,
  `Authorization: Bearer …`), tokens con prefijo conocido (`sk-`, `AKIA`,
  `ghp_`, `xox*-`), JWT, claves privadas PEM, tiras de 32+ caracteres que
  mezclan letras y dígitos, y correos → `[redacted]`. Es básico, no una
  garantía: con datos sensibles, `capture: "hash"`.

## Feedback: personas y maestros externos

`POST /v1/feedback` (y `kling ai feedback`) guarda en `feedback.jsonl`:

```json
{"task":"home","id":"df929680f80a-1b","label":"cover_set_position","by":"juan"}          confirmar/corregir
{"task":"home","id":"df929680f80a-1b","action":"discard"}                                   no entrena nunca
{"task":"home","text":"flaky test in the scheduler","label":"infra","by":"ci-triage"}       sin id: con su texto
{"task":"home","id":"df929680f80a-1b","teacher":"encoder","label":"cover_close","conf":0.97} voto de un maestro
{"task":"home","items":[ … hasta 1000 … ]}
```

- **Solo el token principal habla como persona** (la verdad). Un token de
  tenant solo puede mandar votos de maestro, que se guardan como
  `ext:<tenant>.<nombre>` y, como cualquier maestro, no entrenan sin validarse.
  Un maestro externo nunca puede hacerse pasar por un modelo del registro.
- Sin `id` (o si el caso no se capturó, o se capturó como hash) hace falta el
  texto. Así se enchufa otra herramienta: `kling ai feedback <tarea> -import
  etiquetas.jsonl` acepta una línea por etiqueta con los campos de arriba
  (`{"text", "fields"?, "label", "by"?}` o `{"id", "label"}`), en tandas.
- `feedback.jsonl` no caduca (las etiquetas humanas son lo escaso) y tiene
  tope de 64 MiB: lleno, 507, no se tira nada.

## Revisión: `kling ai review`

Salida real, tras importar 124 etiquetas humanas de 300 peticiones (sin
maestros todavía):

```
$ kling ai review home -n 5
task home: 1 cases need a person, 0 would enter on their own, 124 already reviewed
      1  no teacher answer
no teacher answers yet: only human labels can teach this task

ID              CHISPA            TEACHERS  PROPOSED     WHY                                                                   TEXT
df929680f80a-1  cover_close 0.34  -         cover_close  random audit (measures the teachers): no teacher answer (rare class)  baja un poco la persiana del dormitorio
```

Con maestros, cada uno sale con su validación, p. ej. (semilla 7 de la
simulación, ronda 5):

```
teachers:
  ext:encoder   validated (reviews): beats Chispa on 263 audited cases (77 vs 14, McNemar p=5.2e-12); what the agreement filter accepts is right 0.983 (179 checks)
  ext:noisy-a   validated (forced): forced (trust_teachers) without validation; reviews say: does not beat Chispa on the audited cases (right 12 vs Chispa 26; 3 vs 17 where they differ, McNemar p=1)
```

`-i` los recorre uno a uno (enter = confirmar lo propuesto, una etiqueta =
corregir, `d` = descartar, `s` = saltar) y guarda cada respuesta.

La cola mezcla dos cosas **a propósito**:

1. **Auditoría al azar** (~40 % de la cola): un 20 % fijo de las capturas,
   elegido por el hash de su texto, que solo se revisa por este camino y en
   orden de hash. **Solo esto mide a los maestros.**
2. **Lo más informativo** para entrenar: maestros que discrepan, una etiqueta
   que Chispa ni consideraba, clases sin umbral (siempre escalan), lo de menor
   probabilidad.

Por qué la separación: la primera versión validaba a los maestros con todo lo
revisado, que se elegía por discrepar, y una discrepancia entre dos maestros
tiene siempre uno que se equivoca. En la simulación, un maestro del 20 % de
error entraba y salía de la validación de una ronda a otra según qué casos
tocaran. Con la auditoría aparte, la validación es estable en las tres
semillas medidas.

## Validación de maestros y filtro de acuerdo

Un maestro está **validado** si:

- un registro de `kling ai eval` de la tarea lo respalda contra etiquetas de
  verdad (la cascada con ese VON gana; en domótica, la capa del codificador
  gana), **o**
- en los casos **auditados** en los que votó, acierta más que Chispa (McNemar
  exacta de una cola, p < 0,05, con al menos `min_checks`) **y** lo que el
  filtro de acuerdo habría aceptado con su voto acierta al menos
  `accept_precision` (aciertos/(n+1), con al menos 10 casos).

Un caso sin etiqueta humana entra solo si pasa el **filtro de acuerdo**:
todos los votos (de cualquier maestro, validado o no) dicen lo mismo, al menos
`min_votes` son de maestros validados, la etiqueta es del modelo y está en el
top-k de Chispa, y hay texto. Entra con peso `teacher_weight`, dentro del tope
por clase (lo más reciente primero). Lo demás espera a una persona.

## Reentreno con puerta: `kling ai retrain`

1. Datos: **oro entero** + etiquetas humanas + lo aceptado. **Guarda de
   fugas**: se quita todo lo que esté en `heldout` (texto normalizado).
2. Validación: `valid` (o el 10 % del oro) + un 20 % estable de lo humano. Lo
   de los maestros nunca calibra.
3. Sombra con **los mismos hiperparámetros y la misma especificación** que el
   modelo que se sirve (salen de sus metadatos): solo cambian los datos, así
   que la puerta mide los datos. Determinista; 0,3–0,4 s con 2 000–5 000
   ejemplos a 2^18 cubos. Ojo: entrena dentro del proceso del gateway
   (~180 MB de pico con 28 clases) y uno a la vez.
4. **Puerta** sobre el conjunto de confianza, con los umbrales efectivos de la
   tarea: se promociona si la sombra **contesta bien** (confiada y acertada)
   más ejemplos donde discrepan, McNemar p < 0,05 (o, con `-rule
   no-regression`, si no contesta bien menos ni acierta menos), **y** su
   precisión confiada no baja más de `-precision-tolerance` (0,005). Es la
   forma de la puerta de la capa del codificador.
5. Sin nada nuevo (ni humano ni aceptado) no entrena.
6. Informe en `ai-evals/<tarea>-retrain.json`; historial (50 intentos) y
   versiones en `ai-data/<tarea>/versions.json`.

**Promoción atómica y versionada**:

- *En proceso*: la versión servida se registra como v1 la primera vez; la
  nueva se guarda como `ai-data/<tarea>/versions/<modelo>@vN.chispa`, la ruta
  del registro pasa a `<ruta>.prev` y se sustituye con un rename (quien lea
  ve el fichero viejo o el nuevo, nunca medio), y la caché del gateway se
  actualiza sin reiniciar. Se conservan los ficheros de v1, la servida, la
  anterior y las 10 últimas. La sombra rechazada queda en `@shadow.chispa`
  para inspeccionarla (`kling ai chispa eval`).
- *microvm*: el gateway no construye imágenes. La versión queda **pendiente**
  y el CLI hace `kling ai chispa deploy` como el dorado `<snapshot>-vN`, y lo
  confirma con `/v1/admin/promote`, que comprueba que el sha256 del registro
  de despliegue del dorado es el de la versión. La tarea pasa a apuntar a ese
  dorado (`versions.json` manda sobre el `snapshot` del registro, que no se
  reescribe) y el anterior sigue ahí. La primera vez hay que dar el `.chispa`
  que sirve (`-current`), que se verifica contra el sha256 del despliegue.

**La cascada**: su registro de evaluación va atado al sha256 del `.chispa`,
así que un modelo nuevo **apaga** una cascada respaldada. El reentreno lo hace
explícito: si estaba activa, vuelve a correr `kling ai eval` con
`learn.heldout` (o con `-eval datos.jsonl`; en domótica hacen falta filas) y
dice cómo queda; con `-no-eval` o si falla (VON caído), dice que queda apagada
hasta evaluarla. Volver con `rollback` al modelo evaluado la reactiva sola.

**Rollback**: `kling ai rollback <tarea> [-to vN]` vuelve a la anterior (o a
la dada) al instante: en proceso, el fichero de esa versión (verificado por
sha256) pasa a la ruta del registro; en microvm, la tarea vuelve a su dorado.

## Métricas

En `/metrics` (y un resumen en `kling ai ls`):

| Serie | Qué dice |
|---|---|
| `kling_ai_chispa_version_coverage{task,version}` | fracción que cada versión contesta confiada, en vivo, desde que este proceso la sirve |
| `kling_ai_escalation_rate_window{task}`, `kling_ai_requests_window{task}` | tasa de escalado y volumen en la ventana (`window_minutes`) |
| `kling_ai_heldout_coverage{task,version}`, `kling_ai_heldout_precision{task,version}` | cada versión en el conjunto de confianza (medido al promocionar) |
| `kling_ai_learn_escalations_avoided_estimate{task}` | peticiones de la ventana × (cobertura de la servida − la de v1, en el conjunto de confianza). Una **estimación** |
| `kling_ai_learn_seconds_saved_estimate{task}` | lo anterior × el coste de una escalada (`escalation_ms`, o la latencia media medida de VON en la tarea) |
| `kling_ai_learn_captures_total{task,outcome}` | capturas: `written`, `dedup`, `cap` (almacén lleno), `queue_full`, `votes_dropped`, `error` |

```
LEARN TASK   CAPTURE   SERVING   VERSIONS (held-out coverage / precision)   CAPTURED   TO REVIEW   ESCALATED (WINDOW)
home         text      v2        v1 0.588/0.976, v2 0.596/0.978             125        1           0.415 of 301 (60 min)
```

## Evidencia: el bucle en los datos de domótica

**Qué es simulado y qué no.** El gateway es el de verdad, en proceso y sin
daemon (`tools/mejora-continua`: `aigw.New`, `Classify`, `Feedback`, `Review`,
`Retrain`, los mismos que sirve `kling ai serve`). Simulado es **quién
contesta cuando Chispa duda**:

- dos **maestros** con error fijo, 8 % («encoder») y 20 % («llm»), que al
  fallar eligen el 70 % de las veces una de las etiquetas del top-3 de Chispa
  (confundir lo confundible) y mandan su voto por `/v1/feedback` como lo haría
  un cliente con su capa lenta. Correr VON de verdad sobre ~10 000 escaladas
  en CPU no cabe en una sesión, y el codificador real solo contesta confiado
  una fracción pequeña de lo escalado (94 de 6 648 en
  [DOMOTICA-EVAL.md](DOMOTICA-EVAL.md)): aquí contestan a todo, así que el
  volumen aceptado es optimista;
- una **persona** que cada ronda revisa 150 casos de la cola de `kling ai
  review` y contesta con la etiqueta de verdad.

**Datos** (los de [domotica-datos.md](domotica-datos.md), formato Chispa):
oro = 2 000 ejemplos al azar de `train.jsonl` (5,6 %); validación = 1 000 de
`valid.jsonl`; tráfico = los 33 821 restantes de `train.jsonl` (MASSIVE, Home
Assistant y la demo), 5 000 por ronda; conjunto de confianza = `test.jsonl`
entero (9 794, 28 intenciones, 64 % «fuera de ámbito»). v1:

```sh
go run ./tools/mejora-continua prepare -data <datos> -out mc     # gold / valid / traffic / heldout
kling ai chispa train -data mc/gold.jsonl -valid mc/valid.jsonl -o mc/intent.chispa
#   26 labels, 2000 train / 1000 valid; valid: accuracy 0.817, confident 64.5% at precision 0.988
go run ./tools/mejora-continua run -out mc -seed 7
```

Cuatro rondas con los maestros buenos y una quinta en la que los maestros son
**un LLM malo** (45 % de error) muestreado dos veces (la autoconsistencia de un
modelo que se equivoca: las dos muestras coinciden el 90 % de las veces,
también en el error, así que el filtro de acuerdo no lo para), **forzado** con
`trust_teachers`. Semilla 7:

| Ronda | Sirve | Cobertura en vivo | Maestros | Humanas (total) | Aceptadas | Validados | Cobertura confianza actual→nueva | Precisión confiada | Contesta bien | +/− | p | Promociona |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | v1 | 0,604 | encoder 8 % + llm 20 % | 150 (150) | 917 | los dos | 0,588 → 0,612 | 0,976 → 0,979 | 0,574 → 0,600 | +358/−104 | 5e-34 | **v2** |
| 2 | v2 | 0,640 | ídem | 150 (300) | 1 332 | los dos | 0,612 → 0,653 | 0,979 → 0,981 | 0,600 → 0,641 | +439/−37 | 1e-88 | **v3** |
| 3 | v3 | 0,705 | ídem | 150 (450) | 1 522 | los dos | 0,653 → 0,670 | 0,981 → 0,981 | 0,641 → 0,658 | +368/−202 | 2e-12 | **v4** |
| 4 | v4 | 0,710 | ídem | 150 (600) | 1 656 | los dos | 0,670 → 0,689 | 0,981 → 0,981 | 0,658 → 0,676 | +435/−258 | 9e-12 | **v5** |
| 5 | v5 | 0,715 | LLM malo 45 % ×2, **forzado** | 150 (750) | 1 771 | + los forzados | 0,689 → **0,635** | 0,981 → 0,984 | 0,676 → **0,625** | +85/**−582** | 1 | **no** |
| — | v5 | 0,725 | (tráfico final) | | | | | | | | | |

- **La cobertura sube y la precisión se queda plana**: en el conjunto de
  confianza, de 0,588 a 0,689 de cobertura (+17 %) con la precisión confiada
  entre 0,976 y 0,981; en vivo, de 0,604 a 0,725. «Contesta bien» (confiado y
  acertado) pasa de 0,574 a 0,676. Con 750 etiquetas humanas en total (menos
  de dos horas de una persona) y los maestros validados.
- **La puerta rechaza el modelo peor**: con el LLM malo forzado, la sombra
  contesta bien 582 ejemplos menos de los que gana (0,676 → 0,625): no se
  promociona y v5 sigue sirviendo. Las revisiones ya lo decían: «does not beat
  Chispa on the audited cases (right 12 vs Chispa 26; 3 vs 17 where they
  differ)». Sin forzarlo no habría entrado nada suyo.
- Ninguna versión se promocionó con menos precisión que la anterior menos la
  tolerancia.
- Estimación de `/metrics` al final: 3 246 escaladas evitadas en las 30 000
  peticiones de la ventana frente a v1 (cobertura de confianza 0,696 − 0,588
  × 30 000). A 2,6 ms por escalada (el codificador,
  [DOMOTICA-EVAL.md](DOMOTICA-EVAL.md)) son ~8 s; a 1,23 s (Qwen2.5-1.5B,
  [ai-gateway.md](ai-gateway.md#cifras-mac-mini-m4-backend-vz)), ~67 min de
  CPU de réplica.
- Cada reentreno: 0,3–0,5 s en total (entrenar, medir en 9 794 ejemplos,
  promocionar), Apple M4; toda la simulación, ~3 s.

Otras dos semillas (8 y 9), mismo guion:

| Semilla | Promociones | Cobertura confianza v1 → última | Precisión | Ronda ruidosa |
|---|---|---|---|---|
| 8 | v2, v3, v4 (la ronda 3 no gana: +116/−98, p = 0,1) | 0,588 → 0,693 | 0,976 → 0,982 | rechazada (0,680 → 0,628 contesta bien; +45/−556) |
| 9 | v2, v3, v4, v5 | 0,588 → 0,699 | 0,976 → 0,983 | rechazada (0,687 → 0,658; +66/−349) |

En la semilla 8 y en la 9 la primera ronda solo valida al maestro del 8 %
(el del 20 % aún no tiene evidencia suficiente en la auditoría), así que con
`min_votes: 2` no entra nada de maestros y v2 sale solo de las 150 humanas
(+171/−46 y +266/−37): la exigencia de dos votos validados es prudente, no
gratis.

Lo que **no** demuestra esta simulación: que un VON o un codificador reales
tengan esos errores (hay que medirlos: `kling ai eval` y la auditoría lo hacen
por tarea), ni el efecto de la deriva temporal (el tráfico sale de la misma
distribución que el oro).

## Límites conocidos

- No hay **modo sombra** en tráfico real (predecir en paralelo sin contestar)
  ni **detección de deriva**; la puerta es el conjunto de confianza, que
  describe la distribución del pasado.
- El reentreno corre dentro del proceso del gateway (memoria: la del
  entrenador) y bloquea otros reentrenos y recalibraciones mientras dura.
- La guarda de fugas compara texto normalizado (minúsculas, espacios), no
  casi-duplicados.
- `hash` no guarda texto: esas capturas solo sirven si una persona manda el
  texto con su etiqueta.
- Un modelo compartido por dos tareas no se reentrena (se rechaza con un
  mensaje): cada tarea que aprende necesita su propio modelo.
- La promoción microvm depende del CLI (construir el dorado); si falla a
  medias, la versión queda pendiente y basta con reentrenar otra vez.
- El filtro de secretos es una lista de patrones, no una garantía.

## Código

`pkg/aigw/learn.go` (configuración, almacén, captura, filtro, `/v1/feedback`),
`learn_review.go` (casos, auditoría, validación de maestros, filtro de
acuerdo, cola de revisión), `learn_retrain.go` (sombra, puerta, versiones,
promoción, rollback), `learn_http.go` (rutas, `/v1/tasks`, métricas);
`cmd/kling/ai_learn.go`; `pkg/chispa/train` (peso por ejemplo);
`tools/mejora-continua` (la simulación).
