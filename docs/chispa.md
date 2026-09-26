# Chispa — un clasificador lineal diminuto para decisiones pequeñas

Chispa es un modelo lineal (regresión logística) que se entrena fuera, pesa uno o
dos megas y responde en microsegundos. Sirve para las decisiones pequeñas que no
merecen un modelo de lenguaje: de qué tipo es este evento, a qué herramienta va
esta petición de un agente, si esta entrada se filtra o pasa. Vive en el núcleo
de kindling (`pkg/chispa`, `pkg/chispa/train`, `kling ai chispa`), en Go puro, sin cgo ni
dependencias externas.

Su razón de ser es la **cascada**: el gateway le pregunta primero a Chispa; si Chispa
está seguro (su probabilidad calibrada supera el umbral de esa clase), contesta
él; si no, **escala** a un modelo mayor (VON, modelos pequeños en microVMs). Todo
el diseño —probabilidades calibradas, umbral por clase, evidencia— está pensado
para que esa decisión sea fiable y barata.

## Qué es y qué no es

| Es | No es |
|---|---|
| Regresión logística sobre texto hasheado + campos estructurados | Un modelo de lenguaje: no entiende, cuenta palabras |
| Determinista: mismos bits en amd64/arm64, macOS/Linux | Un sustituto de reglas duras cuando existen (si el dato trae la etiqueta, úsala) |
| ~1,5 µs por predicción (texto de 200 caracteres, M4), 0 reservas | Bueno con clases de un puñado de ejemplos: con menos de ~30 por clase, no aprende esa clase |
| Honesto sobre su duda: `escalate` cuando no llega al umbral | Robusto a un cambio de distribución: los umbrales valen para datos como los de validación (ver [CHISPA-EVAL.md](CHISPA-EVAL.md)) |

## Uso rápido

```sh
# datos: JSONL, una línea por ejemplo
{"text": "panic in the parser when the cache is cold", "label": "fix", "fields": {"service": "api", "files": 3}}

kling ai chispa train -data train.jsonl -valid valid.jsonl -o eventos.chispa
kling ai chispa eval -model eventos.chispa -data test.jsonl
kling ai chispa predict -model eventos.chispa -text "segfault resolving symlinks" -fields '{"ext":".zig"}'
kling ai chispa inspect eventos.chispa
```

`predict` sin `-text` lee JSONL de stdin (`{"text":…, "fields":…}`) y escribe una
respuesta JSON por línea: es la forma de usarlo desde otro proceso.

```json
{"label":"fix","index":5,"prob":0.393,"threshold":0.517,"confident":false,"decision":"escalate",
 "probs":[{"label":"fix","prob":0.393},{"label":"refactor","prob":0.152}, …],
 "evidence":[{"feature":"f:ext=.zig","weight":0.174},{"feature":"w:segfault","weight":0.085}, …]}
```

Desde Go:

```go
m, err := chispa.LoadFile("eventos.chispa")
p := m.Predict(chispa.Input{Text: msg, Fields: fields}) // sin reservas, concurrente
if p.Confident {
	return p.Label // Chispa contesta
}
return escalar(msg, m.PredictFull(chispa.Input{Text: msg}, 5)) // pasa probs y evidencia como pista
```

## Cómo lo usa el gateway

En `kling ai serve` ([ai-gateway.md](ai-gateway.md)) Chispa contesta las
clasificaciones; cuando duda, la respuesta sale igual con `escalate: true` y
quien llama decide.

### Modo sin daemon, y el backend serverless

Un registro que solo tiene modelos Chispa en proceso (`kind: "chispa"`, sin
`backend` o con `"inprocess"`) **no necesita daemon ni KVM/vz para nada**:
`kling ai serve` arranca y sirve igual en un portátil donde solo está
instalado el binario `kling`. Si el registro trae además un modelo VON, un
codificador, o una tarea Chispa con `"backend": "microvm"`, y el daemon no
contesta, el gateway avisa una vez con claridad al arrancar y sigue: las
tareas Chispa en proceso no se enteran. `kling ai chispa train|eval|predict|inspect` no
tocan el daemon nunca: son CLI pura sobre el fichero `.chispa`.

Chispa también se puede desplegar como una tarea **serverless**, empaquetada en
su propia microVM y despertada bajo demanda —el mismo modelo operativo que
VON—, con `kling ai chispa deploy` y `"backend": "microvm"` en el registro. Cuándo
compensa cada opción, cómo desplegarla y las cifras (thaw, latencia,
decisiones/s con y sin microVM) están en
[chispa-serverless.md](chispa-serverless.md). Lo de abajo, escalar a VON, es la **cascada**, y solo se
activa por tarea si `kling ai eval` demuestra que gana a Chispa solo: en commits no
ganó con ningún LLM de 0,5B a 3B.

### La cascada (Chispa → VON)

1. La petición llega al gateway. Se extrae `text` (y `fields` si los hay: servicio,
   nivel, herramienta pedida…).
2. `Predict` (≈ 1–6 µs). Si `decision == "confident"`, se usa `label` y se termina.
   Coste marginal: nada.
3. Si `decision == "escalate"`, la petición va a VON. `PredictFull` da las
   probabilidades y la evidencia para acotar la pregunta («duda entre fix y
   test; pesan `w:assert`, `f:ext=.ts`»).
4. Lo escalado, con lo que contestó VON (o el codificador, o una persona), se
   puede capturar y reentrenar con `kling ai retrain`, que solo promociona un
   Chispa nuevo si gana en un conjunto de confianza: ver
   [mejora-continua.md](mejora-continua.md). Chispa aprende de lo que antes
   escalaba, sin fiarse de un maestro que no se haya validado.

Las cifras que decide la cascada son las que imprime `kling ai chispa eval`:
**cobertura** (qué fracción contesta Chispa), **precisión en lo confiado** (lo que
promete el umbral), **ECE** (si «0,9» significa acertar 9 de 10) y la
**exactitud en lo escalado** (si es alta, los umbrales son demasiado prudentes).
La precisión objetivo se elige al entrenar (`-precision`, 0,95 por defecto).

Aviso importante, medido: el umbral garantiza la precisión **sobre datos como los
de validación**. Si la distribución cambia (otro repo, otra época), la precisión
real baja —en la evaluación de commits, de 0,95 a 0,58–0,85—. El gateway debe
recalibrar con datos recientes de su propio tráfico, no fiarse del modelo de otro
sitio. Detalle en [CHISPA-EVAL.md](CHISPA-EVAL.md).

## Características

La especificación (`FeatureSpec`) va dentro del fichero y se hashea: un modelo
solo se usa con la extracción con la que se entrenó.

- **Normalización** (sin tablas externas; no hay NFKC en la biblioteca estándar):
  minúsculas Unicode, acentos latinos plegados (U+00C0–U+017F: «canción» =
  «cancion»), marcas combinantes fuera, ancho completo → ASCII, y cada ideograma
  CJK/kana/tailandés como token propio. Tokens = tramos de letras y dígitos.
  Un token con dígitos de 5+ bytes es `<num>` (PRs, hashes, fechas); uno de más de
  40 bytes es `<long>`.
- **Palabras**: unigramas (`w:`) y bigramas (`b:`), en el orden del texto.
- **N-gramas de caracteres** (`c:`, opcionales, `-char 3-5`): en runas, con bordes
  `<` `>` como fastText. Unas 4× más lentas y, en commits, sin ganancia clara.
- **Campos** (`f:`, `-fields`): `{"service":"API"}` → `f:service=api`; listas → una
  característica por elemento (hasta 16); números → orden de magnitud en potencias
  de dos (`f:files#+[2,4)`), calculado con enteros; booleanos → `=true/false`.
  Objetos anidados, ignorados. Como mucho 64 campos (si hay más, los 64 primeros en
  orden alfabético, para que el subconjunto no dependa del orden del mapa).
- **Hashing**: FNV-1a de 64 bits sobre `prefijo + bytes`, finalizador de murmur3;
  cubo = bits bajos (2^18 por defecto, `-buckets` en log2), signo = bit alto
  (hashing con signo: las colisiones se cancelan en media). Sin el hash de mapas
  de Go, que es aleatorio por proceso.
- **Normalización del valor**: el texto se divide por √(nº de apariciones de
  texto), para que un mensaje largo no tenga logits más extremas solo por largo;
  los campos valen ±1.

## Modelo, entrenamiento y cuantización

- **Multinomial** (softmax, una fila de pesos por etiqueta) o **binario**
  (logística, una fila) cuando hay exactamente dos etiquetas. `-one-vs-rest X`
  convierte datos multiclase en «X contra el resto».
- **Optimizador: AdaGrad por ejemplo**, L2 perezosa, pesos por clase
  (`balanced` = N/(L·n_c) con tope 10, `sqrt`, `none`), parada temprana por
  pérdida de validación con paciencia. Por qué no L-BFGS: su historia son vectores
  densos del tamaño del modelo (2^18 × 10 × 10 pares × 2 ≈ 420 MB), mientras que
  AdaGrad solo toca lo que aparece en cada ejemplo y da un punto de control por
  época. Ojo: el primer paso de AdaGrad mide exactamente `lr` en cada peso
  tocado; con `lr` alto memoriza en la primera época (por eso el defecto es 0,05).
- **Determinista con la semilla**: barajado con splitmix64 propio, características
  ordenadas y la misma aritmética sin FMA que la inferencia. El mismo corpus y la
  misma semilla dan los mismos pesos también en otra arquitectura (test dorado), y
  con `SOURCE_DATE_EPOCH` el `.chispa` sale idéntico byte a byte.
- **Cuantización**: int16 con una escala por salida (`max|w| / 32767`). La
  concordancia int16/float se mide en validación (va a los metadatos) y, con
  `-test`, en prueba: 100 % en todos los conjuntos de la evaluación.
- **Calibración**: temperatura (un escalar que minimiza la NLL en validación,
  ajustado sobre el modelo **ya cuantizado**), y luego un umbral τ por clase: el
  menor corte tal que las predicciones de esa clase con p ≥ τ tengan en validación
  una precisión estimada ≥ objetivo, como aciertos/(n+1) (conservador con pocos
  datos) y con al menos `-min-support` (10) predicciones. Una clase que no llega
  tiene τ = `never`: siempre escala.

## Inferencia y determinismo

```
z_k = ((Σ signo·q[cubo,k] del texto) · 1/√n_texto + Σ signo·q[cubo,k] de campos) · escala_k + sesgo_k
p   = softmax(z / T)        (binario: softmax([0, z]) = sigmoide)
```

La acumulación es entera (int64; el orden no importa). Lo poco en coma flotante
del final está escrito para ser idéntico en todas partes: la especificación de Go
permite fundir `x*y+z` en una FMA y el compilador lo hace en arm64 (no en amd64),
así que cada producto va envuelto en `float64(…)`, que obliga a redondear; y
`math.Exp`/`math.Log` tienen ensamblador por arquitectura, así que se usan
versiones propias (`detExp`, `detLog`) hechas solo de operaciones IEEE. Los tests
dorados de hash, extracción, entrenamiento y probabilidades (bit a bit) pasan
igual en arm64 nativo y con `GOARCH=amd64` (Rosetta).

Medido en un Apple M4 (`go test ./pkg/chispa -bench Predict`), texto de 200
caracteres, modelo de 10 clases a 2^18 cubos:

| Características | ns/predicción | reservas |
|---|---|---|
| palabras + bigramas | 1 540 | 0 |
| + campos | 1 630 | 0 |
| + n-gramas de caracteres 3–5 | 5 900–6 200 | 0 |
| palabras, 10 goroutines en paralelo | 290 (por op., agregado) | 0 |

Sobre los commits reales de la evaluación (asunto + hasta 300 bytes de cuerpo),
`kling ai chispa eval` mide 4,9 µs por ejemplo con palabras+campos y 12,7 µs con
n-gramas de caracteres, incluida la contabilidad de métricas. `Model` es
inmutable; los búferes de cada llamada salen de un `sync.Pool`.

Lo mismo en x86 (i7-8700T, bare metal, docs/von.md#x86-sin-anidar-i7-8700t):
~2× más lento por predicción a un núcleo (3095-11 919 ns/op según
características) y, con solo 4 núcleos frente a los 10 del M4, el paralelo
agregado llega a 785 000 decisiones/s en vez de ~3,4 M/s. Con los modelos
reales de domótica (`intent.chispa` + `slots.chispas`, `kling domotica eval`,
9794 filas): cascada plantillas→Chispa a 9,74 µs p50 (~103 000 decisiones/s de
un núcleo) y 82 MiB de pico de RSS con ambos modelos cargados. Tabla completa
en docs/von.md.

## Formato del fichero `.chispa`

Todo little-endian:

| Campo | Tamaño |
|---|---|
| magia `\x89CHI\r\n\x1a\n` (como PNG: detecta transferencias en modo texto) | 8 |
| versión del formato (2) | u16 |
| banderas: bit0 binario, bit1 pesos dispersos | u16 |
| longitud de la cabecera JSON (≤ 1 MiB) | u32 |
| cabecera JSON: `spec`, `labels`, `meta` (fecha, SHA-256 del conjunto, recuentos, hiperparámetros, métricas de validación, concordancia int16) | variable |
| hash de la especificación (debe coincidir con el recalculado) | u64 |
| cubos, salidas | u32, u32 |
| temperatura | f64 |
| escala y sesgo por salida | 2 × salidas × f64 |
| τ por etiqueta | etiquetas × f64 |
| pesos densos: cubos × salidas × i16 **o** dispersos: u32 filas + filas × (u32 cubo, salidas × i16) | variable |
| CRC-32C de todo lo anterior | u32 |

`Marshal` elige disperso si ocupa menos (con pocos miles de ejemplos, ~18 % de los
cubos tienen algún peso): el modelo de la evaluación ocupa **1,1 MB** (10 clases,
46 854 cubos no nulos) frente a 5,2 MB en denso; 1,85 MB con n-gramas de
caracteres; 282 KB el binario `fix` contra el resto. En memoria siempre es denso
(5,2 MB a 2^18 × 10), para que la inferencia sea un acceso directo.

El cargador lee como mucho 66 MiB (`io.LimitReader`), comprueba el CRC antes de
interpretar nada, valida cada longitud contra los topes **antes** de reservar
(cubos ≤ 2^22, ≤ 256 etiquetas, tabla de pesos ≤ 64 MiB, filas dispersas
crecientes y dentro de rango, nada de bytes sobrantes), comprueba que escalas,
sesgos, temperatura y umbrales son finitos y razonables, y recalcula el hash de la
especificación. Un fichero corrupto u hostil da error, nunca pánico: lo prueban un
test de truncados y bytes cambiados (también con el CRC recalculado, para que no
baste el CRC) y `FuzzLoad` (11 M de ejecuciones sin fallo en la sesión en que se
escribió; `go test ./pkg/chispa -run X -fuzz FuzzLoad -fuzztime 30s`).

## CLI

```
kling ai chispa train -data train.jsonl -o m.chispa [-valid v.jsonl] [-test t.jsonl]
    [-buckets 18] [-unigrams] [-bigrams] [-char 3-5] [-fields] [-max-text 4096]
    [-epochs 30] [-patience 3] [-lr 0.05] [-l2 1e-4] [-class-weight balanced|sqrt|none]
    [-seed 1] [-valid-frac 0.1] [-precision 0.95] [-min-support 10]
    [-one-vs-rest LABEL] [-v]
kling ai chispa eval -model m.chispa -data t.jsonl [-json]
kling ai chispa predict -model m.chispa [-text T] [-fields JSON] [-top 5] [-json]
kling ai chispa inspect <m.chispa> [-json]
```

Cada línea puede llevar `"weight"` (0 o ausente = 1, como mucho 100): pesa el
ejemplo dentro de su clase (el bucle de mejora continua da 0,5 a lo que
etiqueta un maestro automático).

Sin `-valid`, se aparta un 10 % por hash del contenido (estable aunque cambie el
orden del fichero). Con datos con orden temporal, mejor un `-valid` con lo más
reciente: es lo que se parecerá al tráfico de mañana. Las lecturas de JSONL
están acotadas (líneas de 1 MiB, 4 GiB, 5 M de ejemplos).

## Límites conocidos

- Con pocos ejemplos por clase no hay milagro: en commits, `build`, `perf` y
  `style` (8–24 ejemplos de entrenamiento) no se aprenden.
- Los umbrales no sobreviven a un cambio de distribución (ver arriba).
- La normalización no es NFKC completo: ligaduras, compatibilidad de otros
  alfabetos y el plegado fuera del latín extendido A se quedan como están.
- Las tablas Unicode (`unicode.IsLetter`…) son las de la versión de Go con la que
  se compila; un cambio de Unicode puede mover algún token de escrituras raras
  entre versiones de Go (no entre plataformas).
- La evidencia son pesos de un modelo lineal: dice qué empujó, no por qué. Con
  pocos datos aparecen palabras vacías («w:el», «w:que») como evidencia.

## Mejoras futuras

Chispa en CPU es el objetivo: rápido, barato y bajo demanda. Cuando Chispa duda, la
cascada puede escalar a capas más caras y opcionales (el codificador de
frases, VON) solo si una evaluación muestra que hacen falta. Una de esas
mejoras está documentada pero no aplicada: el [ajuste fino del codificador con
GPU](codificador.md#mejora-futura-no-aplicada-ajuste-fino-con-gpu), que
resolvería el lenguaje indirecto en la capa 3 (~3 ms) en vez de escalarlo a
VON.
