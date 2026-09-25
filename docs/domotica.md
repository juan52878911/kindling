# Domótica: decidir qué hace la habitación

La demo de producto es una habitación con luces, termostato, persianas, tele,
cerradura, alarma, ventilador, altavoz y enchufe, manejada con órdenes de voz
(aquí, ya como texto) en español e inglés. Decidir qué hacer con cada orden es
el caso de uso de la cascada de kindling: capas de coste creciente, y cada una
solo entra si su evaluación mejora la anterior.

| capa | qué | coste | estado |
|---|---|---|---|
| 1 | emparejador de las órdenes de la demo (`Matcher`) | ~1,5 µs | aquí |
| 2 | JEV: intención (`pkg/jev`) + huecos (`pkg/jev/slots`, JEV-slots) | ~3–5 µs | aquí |
| 3 | codificador de frases (multilingual-e5-small, embeddings de llama.cpp en una microVM) + cabeza `.jenc` | ~3 ms | [codificador.md](codificador.md) |
| 4 | LLM pequeño (VON, Qwen2.5-1.5B) con salida JSON restringida y validada: varias órdenes en una, valores relativos, paráfrasis | 1–3 s | [abajo](#capa-4-un-llm-con-salida-json) |

Esta fase deja las capas 1 y 2 como bibliotecas (`pkg/domotica`,
`pkg/jev/slots`) y una CLI (`kling domotica`) que el gateway de IA podrá
llamar después. Nada necesita daemon ni microVM. Datos y licencias en
[domotica-datos.md](domotica-datos.md); resultados en
[DOMOTICA-EVAL.md](DOMOTICA-EVAL.md).

## Uso

```sh
# datos (una vez): descarga fijada por sha256 y conversión al esquema único
go run ./tools/domotica-data fetch
go run ./tools/domotica-data build          # → <cache>/kindling/domotica/data

# modelos
D=~/Library/Caches/kindling/domotica/data   # ~/.cache/... en Linux
M=~/Library/Caches/kindling/domotica/models && mkdir -p $M
kling jev train -data $D/jev/train.jsonl -valid $D/jev/valid.jsonl -o $M/intent.jev \
    -char 3-5 -class-weight sqrt -lr 0.2 -epochs 100
kling domotica train-slots -data $D/train.jsonl -valid $D/valid.jsonl -o $M/slots.jevs

# decidir
kling domotica decide "pon la temperatura del salón a veintidós grados"
kling domotica decide -json "turn the bedroom lights on"
echo '{"text":"baja un poco las persianas"}' | kling domotica decide     # JSONL in/out

# evaluar
kling domotica eval -data $D/test.jsonl -errors 10
kling domotica templates -lang es           # las órdenes que conoce la capa 1
```

Los modelos se buscan en `$KLING_DOMOTICA_MODELS` (o la carpeta de caché de
arriba); `-intent` y `-slots` los fijan. Sin modelos contesta solo la capa 1 y
todo lo demás escala.

```
$ kling domotica decide "pon la temperatura del salón a veintidós grados"
intent: set_temperature  slots: {"device":"thermostat","area":"living_room","value":22,"unit":"°C"}
layer: template  lang: es  p=1.000  confident  14.0 µs

$ kling domotica decide "baja un poco las persianas del dormitorio"
intent: cover_close  slots: {"device":"blinds","area":"bedroom"}
layer: jev  lang: es  p=0.903  confident  90.7 µs

$ kling domotica decide "enciende la luz y baja la persiana"
intent: cover_close  slots: {"device":"light"}
layer: jev  lang: es  p=0.938  escalate → encoder (multi_command)  53.5 µs
```

(La latencia de una invocación suelta de la CLI incluye el arranque en frío de
búferes y tablas; en caliente, dentro de un proceso, son 1–15 µs: ver la
evaluación.)

La salida `-json` es la que consumirá el gateway:

```json
{"text":"turn the bedroom lights on","lang":"en","intent":"turn_on",
 "slots":{"device":"light","area":"bedroom"},"layer":"jev","confident":true,
 "prob":0.9687,"latency_us":55.8,
 "spans":[{"slot":"area","start":9,"end":16,"text":"bedroom"},{"slot":"device","start":17,"end":23,"text":"lights"}]}
```

`confident: false` trae `reason` (`low_probability`, `missing_slot`,
`multi_command`, `out_of_scope`, `no_model`) y `escalate: "encoder"`. Con la
capa 3 (`-encoder head.jenc -embed-url …`, [codificador.md](codificador.md)),
lo que tampoco resuelve el codificador sale con `escalate: "von"` (y
`encoder_error` si la réplica no contestó).

## Cómo decide (`Decider.Decide`)

1. **Plantillas de la demo** (`demo.go`, 127 plantillas es/en con la sintaxis
   de hassil: `(a|b)`, `[opcional]`, `<regla>`, `{lista}`). Se compilan una vez
   a ~62 000 frases normalizadas. Al emparejar, el texto se tokeniza igual que
   JEV (minúsculas, sin acentos ni puntuación), se quitan cortesías («por
   favor», «please», «alexa»), y zonas, números y colores se sustituyen por un
   marcador (`zzarea`, `zznum`, `zzcolor`) guardando su valor. La clave
   resultante se busca en un mapa: coincidencia exacta o nada. Sin reservas de
   memoria en el camino caliente. Si dos plantillas producen la misma frase con
   distinto resultado, `NewMatcher` falla: la demo es inequívoca por
   construcción.
2. **JEV** predice la intención con su probabilidad calibrada y el umbral de su
   clase (el campo `lang` va como característica). **JEV-slots** marca los
   huecos y la normalización los convierte en valores: zona y dispositivo
   canónicos, números escritos con palabras («veintidós», «treinta y cinco»,
   «twenty five», «y medio», «máximo»), unidades («grados», «%», «por
   ciento», «fahrenheit»), colores. `Resolve` completa lo implícito («apaga la
   cocina» → device light) y descarta lo que no encaja (un valor en una
   intención sin unidad).
3. **No es confiada** —y escala— si JEV no llega al umbral, si falta un hueco
   obligatorio (`set_temperature` sin valor), si hay dos órdenes («… y …» con
   dos verbos de orden) o si JEV dice `out_of_scope`: lo indirecto («aquí hace
   frío») cae justamente ahí, y solo una capa mayor puede decidir que de verdad
   no hay nada que hacer. `Decider.FinalOOS` cambia esa política.

## Capa 4: un LLM con salida JSON

Lo que las capas 1–3 escalan puede ir a un LLM instruct pequeño servido por
kindling: la tarea de generación del gateway de IA (`POST /v1/generate`) con el
prompt y el esquema de `pkg/domotica` (`LLMSystemPrompt`, `LLMSchema`; la
tarea lista para `ai.json` está en
[examples/domotica/ai.json](../examples/domotica/ai.json)).

- **Esquema JSON** que llama-server convierte en gramática: `{"kind":
  command|situation|other, "reply": "…", "actions": [{"intent", "device",
  "area", "value", "color"}]}`, con la intención entre las 27 de la
  taxonomía, dispositivo, zona y color entre los canónicos, y como mucho 4
  acciones. El orden importa: clasificar la frase y escribir la frase de vuelta
  antes de las acciones hace que estas sigan a aquella.
- **Validación estricta** en quien ejecuta (`ParseLLM`), porque el invitado no
  es de fiar: JSON sin campos de más, intención conocida, dispositivo que puede
  hacerla, valor en rango (0–100 %, 5–35 °C), huecos obligatorios. Además, la
  respuesta se **ancla a la frase**: una zona que la frase no nombra se quita,
  un color o un número que no dice invalida la respuesta, y abrir la puerta o
  desarmar la alarma exige nombrarlas. Si algo falla no se ejecuta nada y se
  pide aclaración (`reason: invalid_output`).
- **Veto** (`Veto`): si Chispa dio «fuera de ámbito» con confianza, el LLM solo
  puede proponer una situación (`kind: situation`) sin verbo de orden: lo
  indirecto. Una frase en imperativo que Chispa no reconoce («pon una alarma a
  las siete») no es de esta habitación.
- **Puerta y alcance**: `kling domotica eval-llm` compara la capa 4 con
  «escalar y no hacer nada» en lo que escala (McNemar) y en la cascada entera
  ponderada, en dos alcances: `all` (todo lo escalado) y `uncertain` (solo lo
  que Chispa duda; lo que da por fuera de ámbito ni se le pregunta). Escribe
  `layer4-<tarea>.json`, atado al prompt, la validación (`PromptID`) y las
  capas rápidas (`FastID`); quien la usa enciende el alcance más amplio que
  pasó. Con Qwen2.5-1.5B pasa `uncertain` y no `all`
  ([DOMOTICA-EVAL.md](DOMOTICA-EVAL.md#capa-4-el-llm-von)).

```sh
# contra un gateway que ya sirve las tareas room (capas 1–3) y room-llm (capa 4)
kling domotica eval-llm -gateway ~/.config/kling/ai.sock -llm-task room-llm -decide-task room \
    -data $D/test.jsonl -errors 20 -dump escalated.jsonl
# o con un gateway en el propio proceso sobre el daemon (capas 1–2 aquí)
kling domotica eval-llm -von von-qwen15-dom -data $D/test.jsonl
```

En Go, la cascada entera es `domotica.Cascade{Fast, Slow}`: `Fast` son las
capas 1–3 (`InProcess(decider)` o `GatewayClient.Fast(tarea)`, que llama a
`/v1/decide`) y `Slow` las lentas (`&VON{Generate: gw.Generator("room-llm")}`),
cada una una `Layer` (`Decide(ctx, text, lang) (Decision, error)`). La traza
(`Trace`) dice qué hizo cada capa; `Decision.Actions` trae todas las acciones.
La habitación de demo que lo enseña todo es un ejemplo aparte:
[examples/domotica](../examples/domotica/README.md).

## JEV-slots (`pkg/jev/slots`)

El extractor de huecos en la filosofía de JEV:

- **Modelo**: perceptrón estructurado promediado (Collins 2002) con Viterbi
  sobre etiquetas BIO (`B-area`, `I-area`, …, `O`). Restricción BIO en el
  decodificador: nunca sale un `I-x` que no siga a `B-x`/`I-x`.
- **Características** por token, hasheadas (FNV-1a + finalizador de murmur3,
  como JEV) a 2^17 cubos: palabra, vecinas a ±2, bigramas a izquierda y
  derecha, prefijo y sufijo de 3 runas, forma (número, `%`, `°`, letras) y la
  clase del token en el **léxico** (zona, dispositivo, color, número, unidad),
  que va dentro del modelo. Los números son `<d>`: el valor exacto no dice
  nada del hueco.
- **Pesos int16** con una escala común a emisiones y transiciones; la suma es
  entera y Viterbi compara enteros exactos: mismo resultado en amd64 y arm64.
- **Tokenizador** propio sobre el plegado de JEV (`jev.FoldRune`): «%» y «°»
  son tokens, «21,5» es un número, y cada token recuerda su posición en bytes
  del texto original (los huecos se devuelven sobre lo que escribió el usuario).
- **Entrenamiento** determinista (splitmix64, semilla fija), parada temprana
  por F1 de huecos en validación. 11 739 frases en ~0,3 s.
- **Rendimiento**: ~1,4 µs por frase, 0 reservas (`BenchmarkTag`, M4); el
  fichero pesa ~130 KB.

### Formato `.jevs`

Hermano del `.jev` (little-endian):

```
[8]  magia "\x89JVS\r\n\x1a\n"
u16  versión (1)            u16  banderas (bit1 = pesos dispersos)
u32  longitud de la cabecera JSON: {spec, tags, lexicon, meta}
u64  hash de spec + etiquetas + léxico
u32  cubos   u32 etiquetas   f64 escala
i16  × (etiquetas+1) × etiquetas: transiciones (fila 0 = inicio)
pesos densos o dispersos (u32 filas; filas × (u32 cubo, etiquetas × i16))
u32  CRC-32C de todo lo anterior
```

El cargador lee como mucho `MaxFileBytes`, comprueba el CRC antes de
interpretar nada, valida cada longitud contra sus topes **antes** de reservar
(cabecera ≤ 4 MiB, pesos ≤ 64 MiB, ≤ 64 etiquetas, ≤ 65 536 entradas de
léxico) y recalcula el hash de la extracción: un léxico o unas etiquetas
distintas se rechazan en vez de dar huecos basura. `FuzzLoad` lo prueba.

## Piezas

| fichero | qué |
|---|---|
| `pkg/domotica/taxonomy.go` | intenciones, dispositivos implícitos, unidades, `Resolve`, `Complete` |
| `pkg/domotica/lexicon.go` | zonas, dispositivos y colores canónicos con sinónimos es/en; `FindSpans`; léxico del etiquetador; detección de idioma |
| `pkg/domotica/numbers.go` | números con palabras es/en, unidades |
| `pkg/domotica/template.go` | analizador y expansor de plantillas estilo hassil (muestreo determinista y enumeración) |
| `pkg/domotica/demo.go`, `matcher.go` | órdenes de la demo y capa 1 |
| `pkg/domotica/decide.go` | la cascada rápida y su política de escalado |
| `pkg/domotica/keywords.go` | línea base de reglas (solo para evaluar) |
| `pkg/domotica/eval.go`, `challenge.jsonl` | métricas y frases de reto |
| `pkg/jev/slots` | JEV-slots: tokenizador, modelo, `.jevs`, entrenamiento |
| `tools/domotica-data` | descarga, YAML, MASSIVE, Home Assistant, repartos |
| `pkg/domotica/layer.go` | `Layer`, `Cascade`, `Trace`, `Action`: la cascada con capas enchufables |
| `pkg/domotica/llm.go` | capa 4: prompt, esquema, `ParseLLM`, `Veto`, `VON` |
| `pkg/domotica/gateway.go` | las capas por el gateway: `/v1/decide`, `/v1/generate`, `/metrics` |
| `pkg/domotica/layer4eval.go` | evaluación y puerta de la capa 4 |
| `cmd/kling/domotica.go`, `domotica_llm.go` | `kling domotica`, `eval-llm` |
