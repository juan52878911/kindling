# Domótica: decidir qué hace la habitación

La demo de producto es una habitación con luces, termostato, persianas, tele,
cerradura, alarma, ventilador, altavoz y enchufe, manejada con órdenes de voz
(aquí, ya como texto) en español e inglés. Decidir qué hacer con cada orden es
el caso de uso de la cascada de kindling: capas de coste creciente, y cada una
solo entra si su evaluación mejora la anterior.

| capa | qué | coste | estado |
|---|---|---|---|
| 1 | emparejador de las órdenes de la demo (`Matcher`) | ~1,5 µs | aquí |
| 2 | Chispa: intención (`pkg/chispa`) + huecos (`pkg/chispa/slots`, Chispa-slots) | ~3–5 µs | aquí |
| 3 | codificador de frases (multilingual-e5-small, embeddings de llama.cpp en una microVM) + cabeza `.jenc` | ~3 ms | [codificador.md](codificador.md) |
| 4 | LLM pequeño (VON) con salida JSON restringida, para lo indirecto | cientos de ms | fase siguiente |

Esta fase deja las capas 1 y 2 como bibliotecas (`pkg/domotica`,
`pkg/chispa/slots`) y una CLI (`kling domotica`) que el gateway de IA podrá
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
kling chispa train -data $D/chispa/train.jsonl -valid $D/chispa/valid.jsonl -o $M/intent.chispa \
    -char 3-5 -class-weight sqrt -lr 0.2 -epochs 100
kling domotica train-slots -data $D/train.jsonl -valid $D/valid.jsonl -o $M/slots.chispas

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
layer: chispa  lang: es  p=0.903  confident  90.7 µs

$ kling domotica decide "enciende la luz y baja la persiana"
intent: cover_close  slots: {"device":"light"}
layer: chispa  lang: es  p=0.938  escalate → encoder (multi_command)  53.5 µs
```

(La latencia de una invocación suelta de la CLI incluye el arranque en frío de
búferes y tablas; en caliente, dentro de un proceso, son 1–15 µs: ver la
evaluación.)

La salida `-json` es la que consumirá el gateway:

```json
{"text":"turn the bedroom lights on","lang":"en","intent":"turn_on",
 "slots":{"device":"light","area":"bedroom"},"layer":"chispa","confident":true,
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
   Chispa (minúsculas, sin acentos ni puntuación), se quitan cortesías («por
   favor», «please», «alexa»), y zonas, números y colores se sustituyen por un
   marcador (`zzarea`, `zznum`, `zzcolor`) guardando su valor. La clave
   resultante se busca en un mapa: coincidencia exacta o nada. Sin reservas de
   memoria en el camino caliente. Si dos plantillas producen la misma frase con
   distinto resultado, `NewMatcher` falla: la demo es inequívoca por
   construcción.
2. **Chispa** predice la intención con su probabilidad calibrada y el umbral de su
   clase (el campo `lang` va como característica). **Chispa-slots** marca los
   huecos y la normalización los convierte en valores: zona y dispositivo
   canónicos, números escritos con palabras («veintidós», «treinta y cinco»,
   «twenty five», «y medio», «máximo»), unidades («grados», «%», «por
   ciento», «fahrenheit»), colores. `Resolve` completa lo implícito («apaga la
   cocina» → device light) y descarta lo que no encaja (un valor en una
   intención sin unidad).
3. **No es confiada** —y escala— si Chispa no llega al umbral, si falta un hueco
   obligatorio (`set_temperature` sin valor), si hay dos órdenes («… y …» con
   dos verbos de orden) o si Chispa dice `out_of_scope`: lo indirecto («aquí hace
   frío») cae justamente ahí, y solo una capa mayor puede decidir que de verdad
   no hay nada que hacer. `Decider.FinalOOS` cambia esa política.

## Chispa-slots (`pkg/chispa/slots`)

El extractor de huecos en la filosofía de Chispa:

- **Modelo**: perceptrón estructurado promediado (Collins 2002) con Viterbi
  sobre etiquetas BIO (`B-area`, `I-area`, …, `O`). Restricción BIO en el
  decodificador: nunca sale un `I-x` que no siga a `B-x`/`I-x`.
- **Características** por token, hasheadas (FNV-1a + finalizador de murmur3,
  como Chispa) a 2^17 cubos: palabra, vecinas a ±2, bigramas a izquierda y
  derecha, prefijo y sufijo de 3 runas, forma (número, `%`, `°`, letras) y la
  clase del token en el **léxico** (zona, dispositivo, color, número, unidad),
  que va dentro del modelo. Los números son `<d>`: el valor exacto no dice
  nada del hueco.
- **Pesos int16** con una escala común a emisiones y transiciones; la suma es
  entera y Viterbi compara enteros exactos: mismo resultado en amd64 y arm64.
- **Tokenizador** propio sobre el plegado de Chispa (`chispa.FoldRune`): «%» y «°»
  son tokens, «21,5» es un número, y cada token recuerda su posición en bytes
  del texto original (los huecos se devuelven sobre lo que escribió el usuario).
- **Entrenamiento** determinista (splitmix64, semilla fija), parada temprana
  por F1 de huecos en validación. 11 739 frases en ~0,3 s.
- **Rendimiento**: ~1,4 µs por frase, 0 reservas (`BenchmarkTag`, M4); el
  fichero pesa ~130 KB.

### Formato `.chispas`

Hermano del `.chispa` (little-endian):

```
[8]  magia "\x89CHS\r\n\x1a\n"
u16  versión (2)            u16  banderas (bit1 = pesos dispersos)
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
| `pkg/chispa/slots` | Chispa-slots: tokenizador, modelo, `.chispas`, entrenamiento |
| `tools/domotica-data` | descarga, YAML, MASSIVE, Home Assistant, repartos |
| `cmd/kling/domotica.go` | `kling domotica` |
