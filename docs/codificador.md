# Capa 3: el codificador de frases

La tercera capa de las tareas de intención ([intent.md](intent.md)), medida en
la decisión de domótica del ejemplo ([domotica.md](domotica.md)): lo que
las plantillas y Chispa no contestan con confianza pasa por un **codificador de
frases** pequeño —un BERT de 118 M de parámetros que convierte la orden en un
vector de 384 números— y una **cabeza** entrenada sobre esos vectores que
decide la intención. Tarda ~3 ms en un Mac M4, frente a los ~5 µs de las capas
rápidas y los cientos de milisegundos de un LLM. Lo que tampoco resuelve escala
a la capa 4 (VON) con `escalate: "von"`.

```
orden ─► plantillas (1,5 µs) ─► Chispa + Chispa-slots (5 µs) ─► codificador + cabeza (~3 ms) ─► VON
              confiada: fin          confiada: fin          confiada: fin          (capa 4)
```

**Resumen honesto.** Bate la marca de [DOMOTICA-EVAL.md](DOMOTICA-EVAL.md) en
el mismo test: orden completa bien en MASSIVE **0,745 es / 0,800 en** (la
cascada sin él, 0,718 / 0,782) y **2 errores confiados** en las 54 frases de reto
(los mismos dos de Chispa; el codificador no añade ninguno). Dentro de ámbito
contesta confiado un 2,6 % más de órdenes (88,1 % → 90,7 %) con la misma
precisión (0,976). Pero no hace lo que más se le pedía: **no entiende el
lenguaje indirecto** («hace muchísimo calor en el salón»). Congelado y con los
datos que hay, casi no ve órdenes indirectas al entrenar, y las que acierta no
llegan al umbral de confianza. Con unos pocos ejemplos escritos a mano su
intuición sube del 17 % al 60 % en indirectas nuevas, pero no lo bastante para
actuar sin VON. Lo que falta es un ajuste fino contrastivo (SetFit) con GPU: la
receta, con coste, está al final.

## El modelo: multilingual-e5-small

| | multilingual-e5-small | paraphrase-multilingual-MiniLM-L12-v2 |
|---|---|---|
| origen (revisión fijada) | `intfloat`, `614241f6` | `sentence-transformers`, `e8f8c211` |
| licencia (ficha del modelo) | **MIT** | **Apache-2.0** |
| arquitectura | BERT 12 capas × 384, tokenizador XLM-R de 250 k piezas, resumen *mean* | igual |
| prefijo | `query: ` (se entrenó así) | — |
| GGUF Q8_0 | 126 MiB, sha256 `9a2039af…` | 126 MiB, sha256 `865c7909…` |
| cabeza (MLP 256), acierto de intención en test | **0,961** | 0,949 |
| cascada + codificador, MASSIVE exact es / en | **0,745 / 0,800** | 0,745 / 0,805 |
| cascada + codificador, dentro de ámbito exact | **0,956** | 0,954 |

Se eligió **e5-small** por validación (orden completa dentro de ámbito 0,969
frente a 0,964; intención 0,974 frente a 0,956). Las dos licencias permiten
usar y redistribuir, también con fines comerciales: un dorado es una copia de
los pesos que viaja entre daemons.

### De dónde sale el GGUF

Ni `intfloat` ni `sentence-transformers` publican GGUF, y los de terceros en
Hugging Face no se pueden comprobar. Así que kindling **lo convierte**
(`scripts/encoder-gguf.sh`) con el conversor de llama.cpp de la misma versión
que lo sirve (b11147), con todo fijado: cada fichero de los pesos por su
sha256 en la revisión del catálogo, el código de llama.cpp, `uv` y las
versiones exactas de PyTorch, transformers, sentencepiece y numpy. La salida es
**reproducible bit a bit** (dos conversiones, el mismo sha256) y se compara con
la del catálogo antes de instalarse.

Un detalle que costó: los dos modelos se declaran `BertModel`, pero usan el
tokenizador SentencePiece de XLM-RoBERTa, y el conversor, con `BertModel`,
busca un vocabulario WordPiece y falla. El script se lo presenta como
`XLMRobertaModel` (misma red, otro tokenizador) con `pad_token_id` a `null`,
porque con un `pad_token_id` el conversor recorta las dos primeras posiciones
como pide RoBERTa, y estos modelos son BERT (posiciones desde 0).
**Comprobado contra transformers** (255 órdenes, 13 tokens de media): coseno
**1,00000** en F16 y ≥ 0,9991 (media 0,9999) en Q8_0.

```sh
# en el host Linux del daemon (necesita python3 y red; ~1,5 GB en ~/.cache, bórralo después)
sudo scripts/encoder-gguf.sh multilingual-e5-small -install   # deja el GGUF en la caché del constructor
kling ai model add enc-e5 -model multilingual-e5-small          # imagen + dorado (-mem 512 por defecto)
kling run -from enc-e5 -name enc-e5-1
kling ai model embed enc-e5-1 "turn on the lights"
```

### Servirlo: un VON más

No hay constructor nuevo: el codificador es una entrada del catálogo de VON con
`Kind: "embed"` (`pkg/von/embed.go`). El constructor `llm` es el de siempre;
solo cambian los argumentos de `llama-server` (`--embeddings --pooling mean
--ubatch-size 512 --batch-size 512`: un modelo no causal procesa cada entrada
en un solo lote) y el calentamiento del dorado, que manda frases reales en los
dos idiomas en vez de una respuesta de chat. La máquina lleva la etiqueta
`von.kind=embed`. Un GGUF propio de Hugging Face también puede ser codificador:
`-url … -sha256 …` con `kind` y `pooling` en el spec.

- **Memoria: 512 MiB por VM.** Con 320 (Linux) o 384 (Mac) el OOM killer mata
  `llama-server` al cargar: el vocabulario de 250 k piezas pesa más en el
  arranque que los pesos. Ya cargado, el dorado usa 264 MiB (Mac) / 329 MiB
  (Linux).
- API: `POST /v1/embeddings` (OpenAI) en el puerto 8000 de cada réplica.

## La cabeza (`pkg/codificador`)

Sobre el vector congelado, una red de una capa oculta (384 → 256 ReLU → 28) o una
regresión logística multinomial (`-hidden 0`), en Go puro, sin dependencias:

- **Entrada**: el vector normalizado, centrado y escalado con la media y la
  desviación de train (los codificadores de frases dejan todas las frases en un
  cono estrecho; estandarizar deja un problema bien condicionado).
- **Entrenamiento** determinista (splitmix64, Adam, pesos por clase
  1/√frecuencia como Chispa, parada temprana por macro-F1 de validación); los
  productos se redondean antes de sumar (`float64(a*b)`) para que arm64 y amd64
  den los mismos bits. Dos entrenamientos iguales dan el mismo fichero.
- **Cuantizada a int16** con una escala por capa; acuerdo int16/float 1,000.
- **Calibrada** con la temperatura de Chispa (`chispa.FitTemperature`) sobre la
  cabeza ya cuantizada, y con un **umbral por clase** (`chispa.ChooseThresholds`,
  precisión 0,90, mínimo 3 apoyos) elegido **solo con las filas de validación
  que las capas rápidas escalan**: lo que la capa 3 ve de verdad, más difícil
  que la media. Con 0,95 y 5 apoyos casi ninguna clase llegaba a contestar.
- **Formato `.jenc`**, hermano del `.chispa`: magia, versión, cabecera JSON
  (etiquetas, codificador y su sha256, prefijo, métricas), pesos, CRC-32C. El
  cargador lee como mucho 64 MiB, comprueba el CRC antes de interpretar nada y
  valida cada tamaño antes de reservar (`FuzzUnmarshal`). 212 KB con la capa
  oculta, 25 KB la logística.
- **Caché de vectores `.jemb`** (texto exacto → vector, con el modelo y su
  sha256): entrenar y evaluar sin réplica encendida, y con resultados
  repetibles.

```sh
D=~/Library/Caches/kindling/domotica/data M=~/Library/Caches/kindling/domotica/models
kindling-domotica embed -url http://<réplica>:8000 -model multilingual-e5-small \
    -data $D/train.jsonl,$D/valid.jsonl,$D/test.jsonl -o $M/e5.jemb      # ~300 frases/s
kindling-domotica train-encoder -data $D/train.jsonl -valid $D/valid.jsonl -cache $M/e5.jemb \
    -o $M/head.jenc -intent $M/intent.chispa -slots $M/slots.chispas            # 40 s
kindling-domotica eval -data $D/test.jsonl -encoder $M/head.jenc -embed-cache $M/e5.jemb
kindling-domotica decide -encoder $M/head.jenc -embed-url http://<réplica>:8000 "subir persiana habitación"
```

Elegido en validación (el test solo se miró una vez antes, con una primera
logística, y otra al final): la red de 256 frente a la logística
(orden completa dentro de ámbito 0,969 frente a 0,963) y a las variantes de
ritmo y regularización (0,965–0,968, ruido); el umbral 0,90/3 frente a 0,95/5
(cobertura 91,1 % frente a 89,3 %, misma precisión). Un **k-NN** (k=10, coseno)
sobre los mismos vectores, como comparación, acierta la intención 0,914 en
test frente a 0,961 de la cabeza: condensar 35 000 ejemplos en 212 KB sale
mejor que votarlos.

### Cómo decide la capa 3 (`Decider.encode`)

- Solo le llega lo que las capas rápidas **no** contestan confiado. Dos órdenes
  en una frase van directas a VON: un clasificador de una etiqueta no las
  arregla y así no gastan los milisegundos.
- Si contesta confiada una orden de la habitación, los **huecos** los sigue
  poniendo Chispa-slots (F1 0,99; el codificador no mejoraría eso) y se exige,
  como en Chispa, que no falte un valor obligatorio.
- «Fuera de ámbito» confiado **escala** (a VON), por la misma razón que en Chispa:
  ahí caen las órdenes indirectas. `FinalOOS` lo cambia.
- Sin confianza, escala con la conjetura de la capa **más segura** de las dos
  (elegido en validación: con la del codificador siempre, lo fuera de ámbito
  que Chispa acierta se convertía en órdenes; exact de toda la validación 0,972
  frente a 0,984).
- Si la réplica no contesta en `EncoderTimeout` (5 s, despertar incluido),
  escala a VON con `reason: encoder_error`.

## Resultados (test, 9 794 frases; las mismas que DOMOTICA-EVAL.md)

Cifras con la caché de vectores; con la réplica del Mac en directo coinciden
(la cascada, idénticas; el codificador solo cambia en unas pocas filas de
9 794: los vectores de la caché se calcularon en el laboratorio Linux, con
otra variante de CPU de llama.cpp y cuatro ranuras).

| | MASSIVE es exact | MASSIVE en exact | dentro de ámbito exact | cubre | prec@c | todo exact | errores de reto |
|---|---:|---:|---:|---:|---:|---:|---:|
| cascada (la marca) | 0,718 | 0,782 | 0,947 | 88,1 % | 0,976 | 0,976 | 2 |
| **cascada + codificador** | **0,745** | **0,800** | **0,956** | **90,7 %** | **0,976** | **0,977** | **2** |
| codificador solo | 0,723 | 0,800 | 0,913 | 16,4 % | 0,924 | 0,957 | 3 |

Detalle en [DOMOTICA-EVAL.md](DOMOTICA-EVAL.md#capa-3-el-codificador). Lo que
hay que saber para leerlo:

- **La mejora en MASSIVE está dentro del ruido** de 220 frases por idioma
  (±6 puntos): +6 órdenes en español, +4 en inglés. Donde sí es clara es en
  Home Assistant (0,972 → 0,979, cubre 88,6 % → 92,0 %).
- **Contesta poco**: 94 órdenes confiadas de las 6 648 que le llegan en test,
  93 bien. 23 de las 28 clases **nunca** llegan al umbral (en validación no hay
  bastantes filas escaladas de esas clases para prometer 0,90), así que casi
  todo lo que contesta es `cover_open` y `cover_set_position` que Chispa dudaba.
- **Lo indirecto no lo resuelve**: en las 18 indirectas del reto, 0 bien, 10
  escaladas (bien: lo decide VON) y 1 mal, la de Chispa. Solo, sin capas rápidas,
  se equivoca con confianza en 3.

### La puerta del gateway

`kling ai eval home -data test.jsonl` contra el gateway, con la réplica del Mac:

```
                   answered right   confident   right when confident   confident errors   exact (with guesses)
fast layers        0.311            0.321       0.967                  104                0.976
with the encoder   0.320            0.331       0.968                  105                0.977
to the encoder 6648 (confident 94, request errors 0)
answered right: fast only 0, with the encoder only 93 (McNemar p=1e-28); counting escalated guesses: 30 vs 42 (p=0.097)
encoder latency p50 2.6 ms, p95 4.0 ms (20 s in total)
the encoder layer wins: …
```

La puerta cuenta **órdenes contestadas bien** (confiadas y completas), no la
conjetura de lo que escala: una capa intermedia gana si contesta bien lo que
antes iba a VON sin equivocarse más con confianza (se permite un error de más
por cada cien ganadas). Contando también las conjeturas, la diferencia (+12)
**no es significativa** (p = 0,097); se dice en el veredicto.

## Órdenes indirectas escritas a mano

`examples/domotica/internal/domotica/indirect.jsonl`: 180 órdenes (9 intenciones × 2 idiomas × 9),
repartidas 5/2/2 en train/valid/test. Escritas para este trabajo, sin repetir
ninguna frase de reto y evitando sus palabras clave («tengo las manos
heladas», «i'm roasting in here», «nos vamos de vacaciones»). Son del mismo
proyecto que el reto: lo que se mide con ellas es optimista.

| cabeza | intención (argmax) en indirectas de test (40) | … en indirectas del reto (18) | respuestas confiadas | prec@c total | MASSIVE exact es / en |
|---|---:|---:|---:|---:|---:|
| sin ellas (la elegida) | 17,5 % | 38,9 % | 0 | 0,968 | 0,745 / 0,800 |
| con ellas (`-indirect`) | **60,0 %** | **61,1 %** | 0 | 0,963 | 0,736 / 0,795 |

Con cinco ejemplos por intención e idioma, el codificador congelado **sí**
generaliza a indirectas nuevas (de 1 de cada 6 a 3 de cada 5), pero la cabeza
no llega a estar segura (sus clases no tienen umbral) y la precisión de lo
confiado baja un poco. Queda como opción (`train-encoder -indirect`) y como
el conjunto de pocos ejemplos del ajuste fino.

## Latencia y memoria

**Mac mini M4 (vz, sin anidar)**, 2 vCPU, 512 MiB, una réplica
(`scripts/98-encoder-bench.sh enc-e5`):

| | |
|---|---|
| crear el dorado (imagen copiada) | 3 s |
| `mem.file` del dorado | 264 MiB |
| una orden, réplica caliente (HTTP directo) | **p50 3,0 ms, p90 3,7, p99 4,0** |
| capa 3 dentro de la cascada (cliente Go, 6 646 órdenes) | p50 2,4 ms, p99 4,2 ms |
| `/v1/decide` por el gateway, réplica caliente | 3,1–3,3 ms de punta a punta |
| restaurar del dorado (`thaw_ms`) → primer vector | 376 ms → 441 ms |
| congelar / descongelar / primer vector tras descongelar | 364 ms / 418 ms / 436 ms |
| `/v1/decide` con la réplica congelada por inactividad (el gateway la despierta) | 981 ms (969 de thaw) |
| memoria por réplica (`phys_footprint`) | **683 MiB** |

**Linux (Firecracker bajo KVM anidado, el laboratorio Lima)**: el cómputo va
15–20 veces más lento que en hierro ([von.md](von.md#el-laboratorio-anidado-por-qué-sus-tiempos-no-cuentan)),
así que sus tiempos no cuentan; la memoria sí.

| | |
|---|---|
| `mem.file` del dorado (VM de 768 MiB) | 329 MiB |
| una orden caliente (anidado) | p50 42 ms, p99 267 ms |
| restaurar → primer vector / descongelar → primer vector | 1,25 s / 1,09 s |
| **PSS de 1, 2, 3 réplicas** | **71, 82, 92 MiB** (suma de RSS 71, 144, 218) |

En Linux cada réplica de más cuesta **~10 MiB**: los pesos viven una vez en la
caché de páginas del host. En macOS no hay compartición y cada réplica paga
entera (683 MiB con su proceso de Virtualization.framework): ahí, una.

## Qué sigue necesitando VON

De lo que la cascada con codificador escala en test (6 554 filas) y en el reto:

1. **Lenguaje indirecto** (el reto: 10 de 18 escaladas): «aquí hace frío», «no
   oigo la tele», «me voy de casa». Inferir la intención de un estado.
2. **Todo lo fuera de ámbito** (casi todas las 6 253 frases de test): la política
   escala «no es una orden» para no convertir lo indirecto en «no hago nada».
3. **Varias órdenes en una frase** (9 de 9 en el reto).
4. **Casi fuera de ámbito** («pon una alarma a las 7»: 15 de 15 escaladas).
5. **Relativo frente a absoluto y taxonomía** (MASSIVE): «baja el volumen al
   cincuenta por ciento», «apaga la alarma» (despertador en MASSIVE).
6. Los **dos errores confiados** que quedan son de Chispa (capa 2) y la capa 3
   nunca los ve: «hace muchísimo calor en el salón» → `get_temperature`, «quiero
   la persiana a la mitad» → `cover_close`. Arreglarlos es reentrenar Chispa (con
   indirectas) o subir su umbral en esas clases.

## Mejora futura (no aplicada): ajuste fino con GPU

**Decisión: no se ejecuta por ahora.** El objetivo del producto es que Chispa
corra en máquinas de CPU corrientes, con eficacia y bajo demanda; el
codificador (capa 3) y VON (capa 4) son capas opcionales que se encienden solo
cuando una evaluación muestra que hacen falta. El ajuste fino de abajo mejora
justo la capa 3, no ese objetivo, y tiene un coste y un riesgo (los datos
indirectos son pocos y de una sola persona) que no compensan ahora mismo. Se
deja documentado, con receta y coste, para retomarlo si el lenguaje indirecto
se vuelve un problema real de tráfico.

Lo que ganaría: resolver el lenguaje indirecto («hace muchísimo calor en el
salón», «no oigo la tele») **dentro de la capa 3** (~3 ms), en vez de escalar
esas órdenes a VON (cientos de milisegundos y una réplica despierta). Es la
laguna descrita en "Qué sigue necesitando VON" más arriba: el punto 1, la más
frecuente en el reto (10 de 18 escaladas).

El paso que falta para que la capa 3 entienda lo indirecto es el de SetFit:
ajustar el **cuerpo** del codificador con pares de frases (misma intención =
cerca, distinta = lejos; pérdida de coseno) a partir de pocos ejemplos por
intención, y entrenar después la cabeza de siempre sobre sus vectores. En CPU
no es razonable; **no se ha lanzado ninguna instancia, y la receta de abajo no
se ha probado** (ni el ajuste en GPU ni la conversión de sus pesos).

Ficheros: `scripts/encoder-setfit/finetune.py` (sentence-transformers, pares
estilo SetFit: por frase, 20 positivos y 20 negativos), `requirements.txt`
(versiones fijadas desde PyPI: torch 2.11.0 con CUDA 12.8, transformers
4.57.6, sentence-transformers 5.7.0; sin probar todavía) y `run.sh` (dos
modelos × tres semillas, y la conversión con el mismo `encoder-gguf.sh`,
`SRC_DIR=` para tomar los pesos ajustados).

```sh
# 1. instancia: g5.xlarge (1× NVIDIA A10G 24 GB, 4 vCPU, 16 GiB), Deep Learning Base AMI (Ubuntu 22.04)
scp $D/train.jsonl examples/domotica/internal/domotica/indirect.jsonl scripts/encoder-setfit/* scripts/encoder-gguf.sh ubuntu@<ip>:
ssh ubuntu@<ip> ./run.sh                 # ajuste + conversión de 6 variantes
scp 'ubuntu@<ip>:work/*.gguf' .          # 126 MiB cada una; APAGAR la instancia
# 2. de vuelta en el laboratorio: cada GGUF a la caché del constructor, dorado,
#    embeddings, cabeza y la misma evaluación (valid para elegir, test para contar)
```

| | estimación |
|---|---|
| datos | 28 intenciones × 64 frases + 100 indirectas ≈ 1 900 frases → ~76 000 pares |
| ajuste (una variante, fp16, lotes de 64, ~1 200 pasos) | 2–4 min en una A10G |
| preparar (arranque, instalar PyTorch con CUDA ~2,5 GB) | ~10 min |
| **todo `run.sh` (6 variantes + conversiones)** | **~35–45 min** |
| precio g5.xlarge bajo demanda (us-east-1) | ~1,0 $/h (spot ~0,4 $/h); comprobarlo antes |
| **coste** | **~0,6–0,8 $ bajo demanda**, < 0,5 $ en spot; disco EBS de 100 GB, céntimos |

Lo que habría que ver para quedarse con él: que la cabeza sobre el codificador
ajustado conteste **confiada** en las indirectas de test y del reto sin más
errores confiados, y que la puerta del gateway lo respalde. Riesgo conocido:
con tan pocas indirectas, el ajuste puede aprender el estilo de quien las
escribió; por eso la validación y el test indirectos tendrían que escribirlos
otras personas (o salir de tráfico real, que el gateway ya puede muestrear).

Alternativa sin AWS: el mismo script con PyTorch sobre **MPS** en el Mac (M4),
presumiblemente 5–10 veces más lento que la A10G pero gratis; necesita ~2 GB de
disco para el entorno de Python.

## Piezas

| fichero | qué |
|---|---|
| `pkg/von/embed.go` | kind `embed`: catálogo (e5-small, MiniLM), argumentos de `llama-server`, calentamiento, `von.Embed` |
| `cmd/kling/builder_llm.go` | un GGUF convertido se toma de la caché por hash (no se descarga) |
| `scripts/encoder-gguf.sh` | conversión reproducible y fijada; `-install` a la caché del constructor |
| `pkg/codificador` | cliente de embeddings (acotado), caché `.jemb`, cabeza, `.jenc`, entrenamiento, k-NN, `Layer` |
| `pkg/intent/intent.go` | capa 3 en la cascada (`Encoder`, `DecideContext`) |
| `examples/domotica/internal/domotica/indirect.jsonl` | órdenes indirectas escritas a mano, con reparto |
| `examples/domotica/internal/tools/encoder.go` | `kindling-domotica embed`, `train-encoder`; `-encoder` en `decide` y `eval` |
| `cmd/kling/ai_intent.go` | `kling ai eval` de una tarea de intención |
| `pkg/aigw/intent.go` | tareas de intención en `/v1/decide`, la réplica del codificador por `pkg/scheduler`, su evaluación y su puerta |
| `scripts/98-encoder-bench.sh` | las cifras de latencia y memoria |
| `scripts/encoder-setfit/` | la receta del ajuste fino (sin ejecutar) |
