# Changelog

Todas las novedades relevantes de kindling. Los binarios pre-compilados están
en [Releases](https://github.com/juan52878911/kindling/releases) para
linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64.

## v0.12.0 — sin publicar

### Despertar más rápido: de 152 a 27 ms congelada, 2,2 ms pausada

Medido fase por fase en un i7-8700T (KVM sin anidar, jailer) con una tarea
Chispa de 128 MiB detrás de `kling ai serve`; cada palanca con su antes/después
y lo que no funcionó, en [docs/despertar.md](docs/despertar.md).

- **Desglose por fases**: `thaw` devuelve `wake` (`api.WakePhases`: wait, check,
  net, spawn, socket, load, resync, cgroup, total) y `kling thaw` y `kling
  events` lo enseñan; el planificador lo completa (`scheduler.WakeTrace`: list,
  renew, wake, ready) y el gateway de IA añade la primera petición, en su log y
  en `/metrics` (`kling_ai_wake_phase_seconds{model,how,phase}`).
  `scripts/99-thaw-bench.sh daemon|gateway|paused` lo mide.
- **La red sobrevive al freeze**: namespace, veth, tap y reglas se quedan
  montados y el thaw los reutiliza (47 → 0 ms); el vigilante los suelta pasados
  30 min y un reinicio del daemon, como siempre. Cuando hay que rehacerla, cuesta
  la mitad (el veth nace en su namespace, `ip -n`, reglas en un solo
  `iptables-restore`). La MAC del tap0 es fija.
- **La memoria no se lee a golpe de fallo de página**: freeze deja en la caché
  el volcado de las máquinas pequeñas (≤ 128 MiB) y thaw lee en segundo plano el
  de las de hasta 512 MiB (resync 54 → 6 ms, primera petición 14 → 2,5 ms).
- El VMM nace ya en su cgroup (`CLONE_INTO_CGROUP`, 8 → 0 ms), el socket de su
  API se sondea cada milisegundo (11 → 4 ms) y el chroot del jail se borra al
  congelar, no al descongelar.
- **Nivel pausada**: `kling pause` / `POST /machines/{ref}/pause` (capacidad
  `pause`) deja el VMM vivo con el invitado parado; `thaw` lo reanuda en ~0,3
  ms. `kling ai serve -paused-mib 256` (por defecto; 0 = congelar siempre) pausa
  en vez de congelar las réplicas ociosas que mejor puntúan por popularidad /
  memoria mientras quepan, congela de verdad las que pasan `-paused-for` (10 ×
  idle) sin uso, y las sacrifica primero si falta memoria. Réplica pausada →
  decisión: 2,2 ms, con ~36 MiB de RSS por réplica Chispa.
- `kling ai serve -name-prefix` para el nombre de las réplicas.
- Arreglo: el cliente de la API de Firecracker dejaba una conexión abierta por
  llamada; con varias pausas sobre el mismo VMM su API acababa rechazando la
  siguiente (`write: broken pipe`).

### Mejora continua: Chispa aprende lo que escalaba (`kling ai retrain`)

Diseño, puertas y cifras en [docs/mejora-continua.md](docs/mejora-continua.md).

- **Captura opt-in por tarea** (bloque `learn` del registro): cada respuesta
  lleva un `id`, y lo que Chispa escala se guarda con su texto filtrado de
  secretos (o solo su hash), el top-k de Chispa, la versión que lo dijo y los
  votos de quien contestó después (VON en la cascada, con `von_votes` de
  autoconsistencia; el codificador en domótica). Escritor en segundo plano,
  sin bloquear la petición; almacén acotado por tamaño y días.
- **`POST /v1/feedback` y `kling ai feedback`**: etiquetas humanas (confirmar,
  corregir, descartar; solo el token principal habla como persona) y votos de
  maestros externos (`ext:<nombre>`, nunca verdad por sí solos); `-import`
  para lotes JSONL de otras herramientas.
- **`kling ai review`**: la cola para una persona, con una auditoría al azar
  (20 % de las capturas por hash) que es lo único que valida a los maestros, y
  lo más informativo primero; `-i` interactivo.
- **`kling ai retrain`**: oro entero + humano + lo de maestros validados que
  pasa el filtro de acuerdo (con peso y tope por clase), sombra con los mismos
  hiperparámetros, y promoción solo si gana en el conjunto de confianza
  (McNemar sobre «contesta bien», precisión confiada sin bajar). Guarda de
  fugas, versiones `@vN.chispa` con `.prev` y swap atómico; en microvm, un
  dorado `<snapshot>-vN` verificado por sha256. Vuelve a evaluar la cascada si
  estaba activa. **`kling ai rollback`** vuelve al instante.
- **Métricas**: cobertura por versión en vivo, tasa de escalado en ventana,
  cobertura y precisión por versión en el conjunto de confianza, escaladas y
  tiempo ahorrados (estimados); sección nueva en `kling ai ls`.
- `kling chispa train`: campo **`weight`** por ejemplo en el JSONL.
- Medido con el gateway real y maestros simulados en los datos de domótica:
  cobertura en el conjunto de confianza de 0,588 a 0,689 en cuatro rondas con
  la precisión confiada plana (0,976–0,981) y 750 etiquetas humanas; un LLM
  malo forzado como maestro da un modelo peor (contesta bien 0,676 → 0,625) y
  la puerta lo rechaza (en las tres semillas medidas).

### VON más rápido en CPU

Cada cambio con su banco de pruebas y su puerta (entra solo si mejora lo medido
sin empeorar la calidad); lo que no funcionó, también contado. Todo en
[docs/von-cpu.md](docs/von-cpu.md).

- **Prefijos de tarea precalculados en el dorado.** Las imágenes de `kling
  models add` arrancan `llama-server` con una caché de prompts de 64 MiB
  (`-cache-ram`, que se suma a la memoria de la VM; 0 la quita), y el dorado se
  congela con el system prompt de cada tarea ya evaluado: `kling models add
  -prefix system.txt` (repetible) o **`kling ai prime`**, que los saca del
  registro del gateway (el `system` de cada tarea y el texto fijo de su plantilla)
  y rehace el dorado de cada modelo VON (etiqueta `von.prefixes`; sin cambios, no
  hace nada). Medido con un system prompt de ~800 tokens: la primera petición de
  una réplica recién restaurada pasa de 4,7 s a 0,38 s en Qwen2.5-1.5B y de 1,7 s
  a 0,17 s en Qwen2.5-0.5B; alternar dos tareas en la misma réplica, de 2,8 s a
  0,11 s por petición. Una imagen anterior se reutiliza con `-cache-ram 0` (solo
  queda el último prefijo). No aplica a los codificadores (kind `embed`,
  [docs/codificador.md](docs/codificador.md)): cada petición es una frase
  corta y distinta, así que su spec fija `-cache-ram 0` siempre y `kling models
  add -prefix` / `kling ai prime` los rechazan con un mensaje claro.
- **`json_schema` por tarea** en las generaciones del gateway: la salida de VON
  se restringe a JSON que cumple el esquema (de 19/21 a 21/21 respuestas válidas
  en Qwen2.5-1.5B, de 11/21 a 21/21 en 0.5B), y el gateway contesta 502 si aun así
  no es JSON (p. ej. cortada por `max_tokens`).
- **Q4_0 en el catálogo** para `qwen2.5-0.5b-instruct` y `qwen2.5-1.5b-instruct`:
  en ARM llama.cpp la reempaqueta para i8mm y evalúa el prompt ~1,8× más rápido
  que Q4_K_M (1,5B) o genera ~35 % más rápido que Q8_0 (0,5B), sin acertar menos.
  La cuantización por defecto no cambia (x86 sin medir).
- `scripts/97-von-cpu-bench.sh` y `scripts/von-bench/`: el banco (tarea de
  domótica con respuestas esperadas, primer token, cambio de tarea, tok/s,
  aceptación del borrador, validez del JSON).
- **No entró**: la decodificación especulativa (borrador Qwen2.5-0.5B para 1.5B,
  SmolLM2-135M para 360M y 1.7B, y n-gramas) fue igual o más lenta en todas las
  configuraciones medidas, incluso con un 96 % de aceptación; tampoco hilos
  distintos del número de vCPU, `--poll 0`, lotes mayores ni la caché KV en Q8_0.

### Domótica: capas rápidas de decisión (`kling domotica`)

Diseño en [docs/domotica.md](docs/domotica.md), datos y licencias en
[docs/domotica-datos.md](docs/domotica-datos.md), cifras en
[docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md).

- **`kling domotica decide "<texto>"`** decide qué hace la habitación de demo
  (luces, termostato, persianas, tele, cerradura, alarma, ventilador, altavoz,
  enchufe) en español o inglés: `{intent, slots, layer, confident, latency_us}`.
  Capa 1, órdenes de la demo por coincidencia normalizada (1,5 µs, sin
  reservas); capa 2, intención Chispa + huecos Chispa-slots (~5 µs la cascada). Lo
  indirecto, lo fuera de ámbito, las órdenes múltiples o incompletas escalan
  (`escalate: "encoder"`). También `eval` (frente a plantillas y reglas, con
  frases de reto), `train-slots` y `templates`.
- **Chispa-slots** (`pkg/chispa/slots`): etiquetador de secuencias lineal
  (perceptrón estructurado promediado + Viterbi BIO) sobre características
  hasheadas, pesos int16, determinista; formato `.chispas` con el endurecimiento
  del `.chispa` (topes antes de reservar, CRC-32C, `FuzzLoad`).
- `pkg/domotica`: taxonomía de 28 intenciones, léxico es/en, números con
  palabras y unidades, plantillas estilo hassil, emparejador, cascada.
- `tools/domotica-data`: descarga fijada por sha256 de Amazon MASSIVE 1.0 y
  home-assistant/intents (ambos CC BY 4.0, atribución en `NOTICE`) y los
  convierte a un esquema único con repartos sin fugas.
- `chispa.FoldRune` se exporta para que otros extractores plieguen igual que Chispa.

### VON en hierro x86, sin anidar

- Primeras medidas de VON y Chispa en x86 bare metal (i7-8700T, Firecracker sobre
  KVM nativo, sin la virtualización anidada del laboratorio Lima): thaw y
  primer token bajan a milisegundos y la generación llega a la velocidad real
  de la CPU (48 tok/s en SmolLM2-360M, frente a 8,8 anidado); el binario
  oficial de llama.cpp para amd64 funcionó a la primera. Palancas de
  `llama-server` medidas sin código nuevo de kindling (`-threads`,
  `--cache-type-k`, decodificación especulativa, `--slot-save-path`, Q4_0);
  cifras y método en [docs/von.md](docs/von.md#x86-sin-anidar-i7-8700t).

### Domótica: capa 3, el codificador de frases

Diseño, cifras y la receta del ajuste fino en
[docs/codificador.md](docs/codificador.md); evaluación en
[docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md#capa-3-el-codificador).

- **Codificadores en el catálogo de VON** (kind `embed`):
  `multilingual-e5-small` (MIT) y `paraphrase-multilingual-minilm-l12-v2`
  (Apache-2.0), Q8_0. `kling models add enc-e5 -model multilingual-e5-small`
  construye con el mismo constructor `llm` (`llama-server --embeddings
  --pooling mean`) y congela un dorado calentado con frases reales; `kling
  models embed <réplica> "<texto>"`. Réplica de 512 MiB: ~3 ms por orden en un
  Mac M4; en Linux, +10 MiB de PSS por réplica de más.
- **`scripts/encoder-gguf.sh`**: nadie de confianza publica su GGUF, así que se
  convierten con el conversor de llama.cpp b11147, todo fijado (pesos por
  sha256, código, `uv`, paquetes) y reproducible bit a bit; validado contra
  transformers (coseno 1,00000 en F16, ≥ 0,999 en Q8_0). El constructor toma un
  GGUF convertido de su caché por hash.
- **`pkg/codificador`**: cabeza (regresión logística o una capa oculta) sobre
  los vectores congelados, en Go puro, determinista, int16, calibrada con la
  temperatura y los umbrales por clase de Chispa; formato `.jenc` endurecido
  (`FuzzUnmarshal`), caché de vectores `.jemb`, cliente de `/v1/embeddings`
  acotado, k-NN de comparación.
- **Cascada**: `Decider.Encoder` (capa 3) y `DecideContext`; lo que el
  codificador tampoco resuelve escala a `"von"`. `kling domotica embed`,
  `train-encoder`, y `-encoder`/`-embed-url`/`-embed-cache` en `decide` y
  `eval`. Bate la marca: MASSIVE exact 0,745 es / 0,800 en (0,718 / 0,782), 2
  errores confiados en el reto; lo indirecto sigue siendo de VON.
- **Gateway**: tareas `domotica` en `/v1/decide`, modelos `kind: "embed"`
  despertados y congelados por `pkg/scheduler`, y `kling ai eval` de la tarea,
  cuyo registro enciende la capa 3 solo si contesta bien más órdenes sin más
  errores confiados.
- `pkg/domotica/indirect.jsonl`: 180 órdenes indirectas escritas a mano con
  reparto train/valid/test (`train-encoder -indirect`, y el test en `eval`).
- `scripts/98-encoder-bench.sh` (latencia y memoria de un codificador) y
  `scripts/encoder-setfit/` (ajuste fino contrastivo con GPU: receta sin
  ejecutar).
- **Mejora futura, no aplicada:** el ajuste fino con GPU de arriba resolvería
  el lenguaje indirecto dentro de la capa 3 en vez de escalarlo a VON; decisión
  de no lanzarlo por ahora y detalle (coste, tiempo, alternativa en Mac con
  MPS) en [docs/codificador.md](docs/codificador.md#mejora-futura-no-aplicada-ajuste-fino-con-gpu).
### Chispa serverless: tareas en microVMs congeladas (`kling chispa deploy`)

Hasta ahora Chispa solo vivía dentro del proceso del gateway (`kind: "chispa"`,
microsegundos, sin daemon). Ahora es también una tarea serverless de kindling,
igual que un VON: `kling chispa deploy <tarea> -model m.chispa [-slots s.chispas] [-mem
64] [-vcpus 1]` empaqueta el `.chispa` con un invitado nuevo, estático y sin cgo,
`cmd/kling-chispa` (carga el modelo al arrancar y sirve `/v1/classify` y
`/healthz`), y congela un dorado con el constructor `chispa` nuevo (mismo motor que
`llm`: capa sobre la base `min`, sin nada que descargar). En el registro del
gateway, `"backend": "microvm"` en vez de `"path"` hace que la tarea la sirva
esa réplica —despertada y congelada por `pkg/scheduler`, como a un VON— en vez
de este proceso; `"backend": "inprocess"` (o nada) sigue siendo la opción de
siempre. Detalle, diagrama y cifras en
[docs/chispa-serverless.md](docs/chispa-serverless.md).

- Medido en un i7-8700T (Proxmox CT 105, KVM sin anidar, backend Firecracker):
  imagen de 13 MB en disco (capa sobre `min`), dorado de 45 MB; primer arranque
  desde el dorado (`restore`) 1,41 s, **thaw de una réplica congelada + primera
  decisión, 135-140 ms**; con la réplica ya despierta, mediana 0,57-3 ms según
  concurrencia y hasta 4231 decisiones/s a 16 clientes por el gateway (HTTP +
  microVM). El mismo modelo en proceso, en el mismo host: mediana 91-123 µs y
  hasta 26 924 decisiones/s a 4 clientes.
- **Carga bajo demanda de modelos Chispa en proceso, medida** (Mac M4): RSS ocioso
  con 0/10/100 modelos en el registro (nada cargado todavía) 15,5/18,3/28,3 MiB;
  con un presupuesto de memoria (`-chispa-mem`), el LRU desaloja de verdad (20
  modelos usados, presupuesto de 8 MiB → 5 quedan cargados); hasta 113 000
  decisiones/s agregadas a 32 clientes por el mismo camino HTTP en proceso.
- `pkg/aigw`: `ModelConfig.Backend` (`inprocess` | `microvm`), validado; la
  cascada Chispa → VON funciona igual desde una réplica microvm (candidatos y
  evidencia enteros, no un top-3, para que `top_k` no pierda etiquetas).
- **No entró de esta rama:** el barrido de 1/10/50 tareas microvm simultáneas en
  el backend `vz` del Mac y la comprobación de compartición de páginas entre
  réplicas del mismo dorado en Linux se quedan pendientes (requieren volver a
  entrar en el host de pruebas; ver docs/chispa-serverless.md).

### Domótica: capa 4 (un LLM con salida JSON) y la habitación de demo

Diseño en [docs/domotica.md](docs/domotica.md#capa-4-un-llm-con-salida-json),
cifras en [docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md#capa-4-el-llm-von), la
demo en [docs/demo-domotica.md](docs/demo-domotica.md).

- **Capa 4** (`pkg/domotica`): lo que las capas 1–3 escalan va a un LLM VON por
  una tarea de generación del gateway (`POST /v1/generate`) con un esquema JSON
  (`kind`, `reply` y hasta 4 `actions` con intención, dispositivo, zona, valor
  y color de la taxonomía). La respuesta se valida estrictamente y se ancla a
  la frase (zona, color y número que la frase nombra; nunca abrir la puerta ni
  desarmar la alarma sin decirlo); si algo falla, no se hace nada y se pide
  aclaración. Un veto no deja convertir en orden lo que el modelo rápido da por
  fuera de ámbito, salvo lo indirecto. Varias órdenes en una frase: 8 de 9 bien.
- **`kling domotica eval-llm`** compara la capa 4 con «escalar y no hacer nada»
  (McNemar y la cascada entera ponderada con lo que no es para la habitación),
  en dos alcances, y escribe el registro que la enciende. Con
  Qwen2.5-1.5B Q4_K_M pasa solo donde el modelo rápido duda: 31 órdenes más
  bien en MASSIVE, ninguna acción fuera de ámbito (errores confiados 1,7 → 1,9 %);
  preguntándole por todo, actúa en el 1,5 % de la charla y empeora la cascada.
- `domotica.Cascade` con capas enchufables (`Layer`, `FastFunc`), su traza por
  capa (`Trace`) y los adaptadores al gateway (`GatewayClient`: `/v1/decide`,
  `/v1/generate`, `/v1/tasks`, despertares de `/metrics`).
- **La habitación de demo** es un ejemplo aparte, [`examples/domotica`](examples/domotica/README.md):
  página embebida (sin CDN) con el plano en SVG, órdenes de ejemplo y texto
  libre, traza por capa con latencia y el despertar de cada microVM, panel de
  microVMs por capa con su memoria (del daemon) y contadores; simulador de
  dispositivos en Go con SSE; español e inglés, claro y oscuro, accesible;
  loopback por defecto, cuerpos acotados, CSP estricta. Registro de ejemplo del
  gateway (`ai.json`) y unidades de systemd para el servidor x86.

### Modelos VON: LLM pequeños bajo demanda (`kling models`)

Diseño, uso y cifras en [docs/von.md](docs/von.md).

- **`kling models add <nombre> -model <id> [-quant q8_0]`** construye con una
  orden la imagen de un modelo —`llama-server` de llama.cpp y el GGUF, como capa
  sobre una base Debian trixie— y su **snapshot dorado**, congelado con el modelo
  ya cargado y caliente. `kling run -from <nombre>` da réplicas que contestan en
  cuanto termina el thaw, con la API compatible con OpenAI en el puerto 8000
  (`/v1/chat/completions`, `/v1/models`, `/health`, `/metrics`). También
  `models ls`, `models ask <réplica> <prompt>` (respuesta y tokens/s) y `models rm`.
- Catálogo fijado por revisión y sha256: `smollm2-360m-instruct` (Q8_0, Q4_K_M),
  `qwen2.5-0.5b-instruct` y `qwen2.5-1.5b-instruct` (Q8_0, Q4_K_M; los de 1,5B con
  4 vCPU); o un GGUF propio de Hugging Face con
  `-url …/resolve/<commit>/<fichero>.gguf -sha256 …`. llama.cpp `b11147`, binarios
  oficiales con todas las variantes de CPU, verificados por sha256. Mandos:
  `-ctx` (2048 por defecto), `-parallel`, `-threads`, `-cpus`, `-mem`, `-cpu-pct`.
- **Constructor `llm`** (`kling builder llm`, instalado por `make deploy`): descarga
  y verifica en Go, cachea por hash en `$KLING_ROOT/cache/von` y se hace su base
  glibc (`glibc-trixie`, con `71-build-glibc-base.sh`) la primera vez; necesita
  `debootstrap` en el host. `models add -build-only` construye solo la imagen,
  para copiarla a un daemon de macOS y hacer allí el dorado.
- El dorado se hace con un core por vCPU (`cpu_pct` 100 × vCPU) y devuelve al
  host lo reclamable (`squeeze`) antes de congelar. Las réplicas del mismo dorado
  muestrean con semillas distintas (medido).
- **Solo licencias abiertas en el catálogo por defecto** (Apache-2.0, MIT): cada
  entrada lleva su licencia y dónde leerla (`kling models ls`). `qwen2.5-3b-instruct`
  (Q4_K_M, Qwen Research License, no comercial) está para evaluar y solo se
  construye con `-accept-license qwen-research`.
- `pkg/von`: catálogo, validación, cliente mínimo de la API y `MakeGolden`. El
  calentamiento del dorado se reintenta mientras quede `-wait`: en un host lento
  la primera respuesta de un 1,5B pasa del plazo de 5 min del proxy del daemon.
- `81-base-image.sh` acepta `ROOTFS_DIR` (ficheros que copiar a la imagen) y
  `SERVICE` (un ejecutable que el entrypoint arranca y relanza antes del agente).
- `scripts/96-von-bench.sh`: arranque en frío y thaw hasta el primer token,
  tokens/s, memoria de 1..N réplicas y semillas.
- Arreglos de camino: `kling run -from` hereda el `cpu_pct` del snapshot (antes
  caía al 50 % de un core salvo que lo pasara quien llamaba, como hace el
  planificador), y el daemon deja legible para el VMM la base que un constructor
  cree (antes solo la imagen construida).

### Chispa: un clasificador lineal diminuto

`kling chispa train|eval|predict|inspect` y los paquetes `pkg/chispa` (características,
formato, inferencia, calibración) y `pkg/chispa/train` (entrenador). Go puro, sin
dependencias ni cgo; corre en local, sin daemon. Diseño en
[docs/chispa.md](docs/chispa.md), evaluación en [docs/CHISPA-EVAL.md](docs/CHISPA-EVAL.md).

- Características: palabras y bigramas normalizados (minúsculas Unicode, acentos
  latinos plegados, CJK por carácter), n-gramas de caracteres opcionales y campos
  estructurados del JSON (`campo=valor`, números por orden de magnitud), con
  hashing trick con signo sobre FNV-1a + fmix64 (2^18 cubos por defecto).
- Modelo: regresión logística multinomial o binaria (`-one-vs-rest`), AdaGrad con
  L2, pesos por clase y parada temprana; cuantizado a int16 con escala por clase
  (concordancia con el float64: 100 % en la evaluación). Determinista con la
  semilla, también entre arquitecturas.
- Cada predicción trae etiqueta, probabilidad calibrada (temperatura), umbral τ de
  su clase elegido para una precisión objetivo (0,95), decisión `confident` /
  `escalate` y, si se pide, la evidencia (características por w·x): lo que
  necesita la cascada Chispa → VON.
- Inferencia entera y sin FMA: mismos bits en amd64 y arm64. 1,5 µs por texto de
  200 caracteres (6 µs con n-gramas) en un M4, sin reservas, segura en
  concurrencia.
- Fichero `.chispa`: magia, versión, especificación y su hash, pesos densos o
  dispersos (1,1 MB para 10 clases), CRC-32C; el cargador acota lecturas y
  reservas y rechaza ficheros corruptos sin pánico (`FuzzLoad`).
- Evaluación real con 4 304 commits convencionales de repos locales
  (`tools/chispa-commits`, `scripts/chispa-eval.sh`): exactitud 0,64–0,67 frente a 0,55
  de unas reglas y 0,32 de la clase mayoritaria; pero la precisión prometida por
  los umbrales cae de 0,95 a 0,77–0,85 con el cambio temporal y a 0,58 entre
  repos. Opt-in hasta que cada tarea tenga su evaluación.

### Gateway de IA (`kling ai`)

Diseño, API y cifras en [docs/ai-gateway.md](docs/ai-gateway.md).

- **`kling ai serve`**: Chispa y VON detrás de una API. Chispa clasifica, enruta y
  filtra dentro del proceso (`POST /v1/classify`, `/v1/decide`; cuando duda,
  contesta con `escalate: true` y quien llama decide); VON genera
  (`POST /v1/generate` con plantilla por tarea, y `/v1/chat/completions`,
  `/v1/completions`, `/v1/models` compatibles con OpenAI, con streaming). Registro
  de modelos y tareas en `~/.config/kling/ai.json`; `SIGHUP` o `kling ai reload` lo
  releen sin cortar nada.
- **La cascada Chispa → VON solo con pruebas**: `escalate_to` en una tarea se activa
  únicamente si `kling ai eval <tarea> -data test.jsonl` muestra, con datos
  etiquetados de la tarea, que gana a Chispa solo (McNemar exacta, p < 0,05) con los
  mismos modelos (sha256 del `.chispa`, dorado) y ajustes que se sirven; el registro
  se guarda en `ai-evals/<tarea>.json`. Si no, el gateway la rechaza y dice por
  qué, salvo `escalate_force`. Con la cascada apagada nunca se llama a VON a
  escondidas. `-von-alone` mide también a VON solo.
- Medido en commits (861 de prueba): Chispa solo 0,640; cascadas con Qwen2.5 0.5B
  0,429, 1.5B 0,520 (Q4_K_M) / 0,498 (Q8_0), 3B 0,540: la puerta las rechaza todas.
- Escala a cero con `pkg/scheduler`: réplicas `gw-<dorado>-*` con la etiqueta
  `ai.gateway=<id>`, thaw al llegar, freeze al quedarse ociosas, réplicas por
  concurrencia con tope por modelo, `-keepwarm` por popularidad. En un Mac, una
  generación caliente contesta en 9 ms y una réplica congelada en ~1,5 s.
- `kling ai calibrate`: recalibra los umbrales de Chispa con lo que VON contestó en lo
  escalado y en auditorías (`audit`), con mitad de evaluación; solo escribe si
  mejora, y se niega si Chispa dejaría de contestar. Recalibrar invalida la
  evaluación de la cascada.
- Seguridad: socket Unix 0600 por defecto; TCP solo con `-listen` y token
  (fichero 0600 o `$KLING_AI_TOKEN`); tokens con nombre y cuota; rutas
  `/v1/admin/*` solo con el token principal; cuerpos acotados y JSON estricto; el
  invitado no es de fiar (plazos, topes de respuesta, sin reenviar cabeceras ni
  rutas de control); semilla del host por petición.
- `/metrics` en formato Prometheus: cobertura y escalado de Chispa, latencias por
  fuente, thaws y arranques en frío por modelo, réplicas por estado.
- `pkg/scheduler`: puerto del invitado configurable, etiquetas y prefijo propios
  (solo adopta lo suyo), `MachineTTL` (con la capacidad `renew`, un arrendamiento
  que se renueva también antes del thaw de un scale-out), el scale-out reutiliza
  réplicas congeladas en vez de dejarlas en disco, marca contra adopciones
  dobles, tope de réplicas por servicio y `OnAcquire` para medir los arranques.

### Despliegue: la unidad de systemd ya no lleva valores de un host concreto

`packaging/kling.service` traía grabado `KLING_SOCKET_USER=juan`: en cualquier
otro host, `make deploy` volvía a instalar la unidad y pisaba en silencio lo
que ese host hubiera configurado, dejando el CLI sin acceso al socket (pasó en
el laboratorio). Ahora la unidad no lleva ningún valor propio de una máquina:
los lee de `EnvironmentFile=-/etc/default/kling` (opcional), y `make deploy`
crea ese fichero solo la primera vez, con `KLING_SOCKET_USER` a partir del
usuario de `HOST`; en los redespliegues siguientes no lo toca.

### Arreglos

- **El daemon ya no congela una instancia del gateway a media petición.** Las
  instancias nacen con TTL 2×idle como red de seguridad, pero el daemon lo cuenta
  desde la creación y ni `thaw` ni el tráfico HTTP lo reinician. Una instancia
  creada hace más de 2×idle, congelada por ociosa y despertada por una petición,
  volvía a congelarse en ~10 s con la petición en curso; y una que atendía sin
  parar se congelaba al cumplir 2×idle. Ahora el planificador renueva el TTL
  antes de despertarla, al adoptarla y en cada vuelta del segador, con la ruta
  nueva `POST /machines/{ref}/renew` (capacidad `renew`). Un sandbox sigue sin
  renovarse al despertar y no se puede renovar por esa ruta. Contra un daemon
  anterior el planificador se comporta como antes.

## v0.10.0 — 2026-09-23

### Carpetas compartidas

`kling run -share SRC:DST[:copy|ro|rw]` (repetible; también en `kling sandbox
create`, en `POST /machines` y en `POST /sandboxes`). Diseño, límites y modelo de
amenaza en [docs/compartir.md](docs/compartir.md).

- **copy** (por defecto): el CLI empaqueta la carpeta local en un tar y la sube
  (`POST /shares/uploads`, también por SSH); el daemon valida cada entrada
  (nada de rutas absolutas, `..`, enlaces que salgan o atraviesen otros, enlaces
  duros, dispositivos ni FIFOs; tamaño acotado por `daemon.share_copy_max_mib`,
  1 GiB por defecto), construye un ext4 de solo lectura con `mke2fs -d` y lo
  engancha como un volumen de solo lectura más. Lo monta hasta un agente
  anterior.
- **ro / rw**: la carpeta del host del daemon, en vivo. El agente de invitado
  habla él mismo el protocolo FUSE del kernel (sin libfuse ni cgo) y pide cada
  operación por ruta al daemon por una conexión que abre el daemon
  (`POST /share/attach`, `Upgrade: kling-share/1`); el daemon la sirve con
  `os.Root`, así que ni `..` ni un enlace sacan nada de la carpeta. `ro` lo impone
  el daemon (`EROFS`). Sin enlaces simbólicos, duros ni nodos nuevos; todo es de
  root para el invitado y lo que crea se entrega al dueño de la carpeta.
  Funciona con `egress none`, sobrevive a `freeze`/`thaw` (los ficheros abiertos
  se reabren solos) y a reiniciar el daemon. Solo carpetas bajo
  `daemon.share_roots` (o `KLING_SHARE_ROOTS`), vacío por defecto.
- `commit` de una máquina con carpetas: `409`. `run -from` con carpetas: `400`.
  Una imagen sin agente, o con uno anterior, lo dice claro.
- `kling ps` enseña la columna `SHARES` si alguna máquina tiene; `kling inspect
  <ref>` nuevo, con el estado de cada carpeta viva; `kling info` dice las raíces
  permitidas. Capacidades `shares-copy` y `shares-live`.
- Medido en el laboratorio (Firecracker anidado en un Mac, arm64): lectura y
  escritura secuencial ~12–18 MB/s (el techo es el limitador de 16 MiB/s de la
  red del invitado), ~200–250 creaciones/s y ~1250 `stat`/s de ficheros pequeños;
  `npm install express` en la carpeta, 56 s frente a 45 s en el disco de la
  máquina.

### Arreglos

- **El daemon de systemd lee la configuración de root.** Sin `$HOME` (un
  servicio sin `User=`), la ruta de la configuración salía relativa a `/` y el
  daemon no veía lo que `sudo kling config set` escribía en `/root/.config`.
  Ahora se busca el directorio del usuario en la base de usuarios.

## v0.9.1 — 2026-09-23

### Reloj y entropía propios tras restaurar

- **`POST /resync` en el agente de invitado** (`pkg/guest`, así que lo tienen
  `kling-guest` y el puente de kindling-mcp en cuanto se recompilan): recibe la
  hora del host y entropía fresca, pone el reloj de pared y mezcla la entropía
  acreditándola y forzando la resiembra del CRNG (`RNDADDENTROPY` +
  `RNDRESEEDCRNG`). Cuerpo acotado y validado.
- **El daemon lo llama tras cada restauración** —`thaw` y `run -from`, en los
  dos backends— antes de dar la máquina por arrancada. En macOS, donde
  Virtualization.framework no tiene VMGenID, dos réplicas del mismo snapshot
  sacaban los mismos aleatorios (ids de sesión MCP idénticos) y el reloj se
  quedaba en la hora del volcado; en Linux VMGenID ya resembraba, pero el reloj
  también se quedaba parado. Un agente anterior o una máquina sin agente no
  hacen fallar la restauración: se avisa una vez por imagen. Capacidad
  `guest-resync`; el evento de thaw dice cuánto costó.
- `guest.IsControlPath`: las rutas del agente que solo debe usar el host, para
  que un proxy que reenvía peticiones de terceros (el gateway MCP) las corte.
- Medido: 1–2 ms por restauración en macOS. En Firecracker (laboratorio anidado)
  el resync es la primera petición al invitado restaurado y se lleva los
  ~150–250 ms que antes pagaba el primer cliente; la siguiente va como siempre.
  Para que una máquina sin agente no pague segundos en cada restauración,
  `freeze` sondea el puerto del agente antes de pausar (el `thaw` no lo intenta
  si nadie escuchaba) y un snapshot sin agente se recuerda 10 minutos.

### Arreglos

- **Tras reiniciar el daemon, las máquinas en jail se readoptan con el socket
  de su chroot.** Se readoptaban con el de su directorio, que no existe:
  seguían corriendo, pero `freeze`, `stop` y el resto de llamadas a su VMM
  fallaban con `dial unix .../fc.sock: no such file or directory` hasta
  destruirlas. Lo mismo al descongelar una máquina que ya corría.

### Planificador

- **`Bind` ya no pisa una sesión fijada a otra instancia.** El mapa de rutas
  está indexado por la clave de sesión, y con el id del invitado como clave dos
  clientes que recibían el mismo id acababan en la misma ruta: el segundo
  reapuntaba en silencio la sesión del primero a su microVM. Ahora se niega y lo
  registra.
- **`BindGuest(clave, idInvitado, ...)`**, `Route.GuestSID()`,
  `NewSessionKey()` y `ErrSessionTaken`: quien enruta acuña su propia clave
  (128 bits de `crypto/rand`) y guarda aparte el id del invitado para traducir
  entre los dos. La API anterior sigue compilando.

## v0.9.0 — 2026-09-23

kindling corre nativo en macOS: el daemon, en un Mac con Apple Silicon, arranca
las microVMs con Virtualization.framework en vez de Firecracker. Ver
[docs/mac.md](docs/mac.md) y el contrato con el ayudante en
[docs/backend-vz.md](docs/backend-vz.md).

### macOS nativo (backend `vz`)

- **Un `kling-vz` por microVM** que habla el mismo API que Firecracker: el
  ciclo de vida, los snapshots dorados, el TTL, los sandboxes, el exec y el
  planificador funcionan sin reescribirse. Lo que en Linux hace el host
  alrededor del VMM —namespace, iptables, cgroups, jailer, `/proc`— va por
  etiquetas de compilación: la red y los puertos se le piden al ayudante, el
  overlay se clona con `clonefile`, la admisión mira `kern.memorystatus_level`,
  los huérfanos se buscan con `ps` y la memoria de cada VMM con `GET /kling/stats`.
- **El backend es una clave de configuración**: `kling config set daemon.vmm vz`
  (o `firecracker`); vacía, el de la plataforma. Se valida contra la máquina y
  `KLING_VMM` la sustituye con un nombre o una ruta. `kling info` dice el
  `backend` y la `arch` del daemon.
- **Sin root**: raíz en `~/Library/Application Support/kindling` y socket dentro;
  `kling` sin `-H` lo encuentra. `kling up` diagnostica Apple Silicon, macOS 14+,
  `kling-vz` firmado, e2fsprogs de Homebrew y las imágenes, y arranca el agente
  de launchd si está instalado.
- `kling-vz` vive en `vz/` como módulo aparte (`make vz`): el `go.mod` de la raíz
  sigue sin dependencias ni cgo.
- Límites frente a Linux: ~350 MiB por restauración (no se comparte la memoria
  del dorado), sin construcción de imágenes, sin techo de CPU ni jailer, 4
  arranques simultáneos por defecto y `squeeze` a ciegas (el framework no da
  estadísticas del invitado).

### Imágenes entre daemons

- **`GET/PUT /images/{name}/blob`** (capacidad `image-blobs`): la imagen, la
  capa, la receta y el kernel, en flujo, con sha256 verificado y renombrado
  atómico. Nunca sustituye una imagen en uso por un contenido distinto.
- **`kling images copy <name> -from <host> [-to <host>]`** mueve una imagen de un
  daemon a otro con todo lo que necesita para arrancar (kernel, base de una
  imagen por capas, receta); lo que el destino ya tiene idéntico no se manda, y
  se niega si las arquitecturas no coinciden. Es como se consiguen imágenes en
  un Mac: en macOS `POST /images` contesta 501.

### Direcciones de los invitados

- **`Machine.Forwards` y `Machine.Addr(port)`**: en macOS todos los invitados
  tienen la misma IP y se alcanzan por puertos de loopback que abre su ayudante.
  El proxy del daemon, exec, shell, los volúmenes y `pkg/scheduler` resuelven
  la dirección con `Addr`, que en Linux sigue siendo `IP:puerto`.
- `pkg/scheduler`: `Instance`, `Route` y `Warm` ganan `Addr(port)`; hay
  `AliveAddr`, `WaitReadyAddr` y el gancho `PrepareAddr`. `IP()`, `Alive`,
  `WaitReady` y `Prepare` se conservan: kindling-mcp compila igual y migra aparte.

## v0.8.0 — 2026-09-23

Cierra lo que quedaba abierto de estabilidad, elasticidad y seguridad tras v0.7.

### Estabilidad

- **Un volcado de congelación a medias ya no se da por bueno.** Firecracker
  escribe `snap.file` y `mem.file` sin temporal; si el daemon moría a mitad, al
  volver la máquina figuraba congelada y el thaw cargaba un volcado truncado.
  Ahora cada congelación deja una marca al empezar y un sello al terminar, con el
  sha256 del estado y el tamaño de la memoria, y reconcile y thaw lo exigen.
- **Los restos de un commit interrumpido se pueden borrar**, y el vigilante los
  recoge solo; antes bloqueaban hasta `commit -replace` del mismo nombre.
- **Borrar directorios huérfanos ya no congela el daemon**: bajo el cerrojo solo
  se mueven a una papelera, y el borrado de GiB se hace fuera.
- **Congelar dentro de jailer reanuda la máquina** si falla recuperar el volcado,
  en vez de dejarla en pausa figurando como en marcha.
- **Una máquina recién creada no se pierde si el daemon muere justo después**:
  su registro se escribe a disco antes de lanzar su VMM, y las escrituras de
  estado se serializan con una generación que descarta fotos viejas.

### Elasticidad

- **Memoria elástica**: `kling run -mem-max N` arranca con un techo y el globo
  retiene la diferencia; `kling resize <ref> -mem M` la sube o la baja en caliente.
- **Admisión por presión real**: con PSI por encima del 20 %, 507 aunque
  MemAvailable parezca holgado; con poco disco, 503 (y no 507, para que nadie
  congele para "hacer sitio" escribiendo más en disco).
- **Escalado por carga** en `pkg/scheduler` (`MaxInflight`, `MaxReplicas`): una
  instancia saturada de llamadas en vuelo ya no recibe sesiones nuevas aunque le
  quepan.
- **Puerta de arranque según el host**: 2 arranques a la vez en un host anidado,
  la mitad de los núcleos (hasta 8) en hierro desnudo.

### Seguridad

- **Snapshots firmados** con una clave del host (HMAC-SHA256): detecta
  manipulación y snapshots traídos de otro host. `KLING_REQUIRE_SIGNED=1` rechaza
  los anteriores, que no llevan firma.
- **El proxy al invitado solo llega al puerto del agente** salvo los declarados
  en la etiqueta `kling.ports`.
- **Jailer por defecto cuando está instalado**; `KLING_JAILER=0` lo apaga. Activarlo
  destapó un fallo que llevaba ahí desde que existe el modo jailer: descongelar una
  máquina de imagen por capas enlazaba en la jaula una ruta monolítica que no existe.
  Corregido, y el e2e ahora congela y despierta una imagen por capas.
- `kling info` dice si `$KLING_ROOT` está cifrado en reposo; receta en
  [`docs/cifrado.md`](docs/cifrado.md).

### Otros

- `kling volume populate` usa la ruta de ejecución en streaming, con la antigua
  como respaldo para imágenes anteriores a v0.7.

## v0.7.0 — 2026-09-23

**Sandboxes para agentes de código.** El núcleo gana lo que necesita un agente para
ejecutar lo que escribe sin tocar el host: exec en streaming, ficheros y sandboxes de
usar y tirar. Guía en [`docs/exec-sandbox.md`](docs/exec-sandbox.md).

| kindling | kindling-mcp |
|---|---|
| v0.7.x | v0.1.x |

### Novedades

- **`kling shell <ref>`**: una terminal interactiva dentro de la microVM, con
  pseudoterminal de verdad, redimensionado y Ctrl-C interrumpiendo lo de dentro y
  no la sesión. Es un cambio de protocolo (`Upgrade: kling-shell/1`) con tramas en
  los dos sentidos; el daemon valida cada una en vez de reenviar a ciegas. El
  agente monta `devpts` si falta, así que no hay que reconstruir imágenes.
- **`kling sandbox create|ls|renew|rm`** (`/sandboxes`): una microVM con exec, sin red
  por defecto, que se destruye al vencer su TTL (10 min por defecto). Desde un snapshot
  con exec arranca en ~300 ms con el estado de la plantilla.
- **`kling exec`** (`POST /machines/{ref}/exec`): stdout y stderr por separado y en
  streaming (NDJSON, también por SSH), stdin, entorno, directorio, plazo que mata al
  grupo de procesos (código 137) y topes de salida por flujo. Termina con el código del
  comando remoto. `?wait=1` da el resultado agregado.
- **`kling cp`** (`/machines/{ref}/files`): subir y bajar ficheros, con escritura
  atómica y sin seguir enlaces en el último componente.
- **`-on-ttl freeze` en los sandboxes**: en vez de destruirse al vencer, se
  duermen a coste cero y el siguiente exec los despierta en milisegundos. Con
  `freeze` el TTL cuenta inactividad; con `remove`, vida máxima.
- **`kling run -allow-exec` y `-on-ttl remove`.** `allow_exec` viaja por el API como
  opt-in explícito, se graba en el snapshot con `commit` y las instancias lo heredan;
  pedirlo sobre un snapshot sin él es `409`.
- El agente de invitado (`pkg/guest`, `kling-guest`) sirve `/exec/stream` y `/files`,
  solo con `kling.exec=1`.
- `GET /info` anuncia las capacidades `exec` y `sandboxes`.

### Correcciones de robustez

- **Un fallo posterior al arranque ya no deja un firecracker vivo.** Restaurar
  desde un snapshot y descongelar no mataban el proceso que acababan de lanzar
  —el arranque en frío sí lo hacía—, así que cada intento fallido (el caso real
  es el TSC invalidado tras reiniciar el host) retenía su RAM para siempre,
  invisible para `kling ps`. Descongelar, además, dejaba la red montada y la
  máquina figurando como congelada, y el siguiente intento readoptaba ese proceso
  vacío como sano.
- **Los VMM huérfanos se recogen en marcha**, no solo al arrancar el daemon.
- **El TTL no se reinicia al despertar**: se cuenta desde su propio reloj
  (`ttl_at`), no desde el último arranque. Un sandbox que dormía y despertaba
  podía no vencer nunca.
- **Una máquina con un secreto inyectado ya no reintenta congelarse cada 10 s
  para siempre**: se retira su TTL, una vez y diciéndolo en el log.
- **Antes de rechazar por memoria se pide prestado a los globos** de los
  invitados vivos, que devuelven lo que no usan sin congelar a nadie; y el
  desalojo puede soltar una instancia precalentada, que hasta ahora nunca ocurría.
- **El tope de máquinas del daemon se distingue de la falta de memoria** (409
  propio) y se puede subir con `KLING_MAX_MACHINES`.

### Cambios que se notan

- Las imágenes construidas antes de v0.7 llevan un agente sin exec en streaming: el
  daemon contesta `501` y pide reconstruirlas (`kling images toolchain`, o
  `kling images build -builder base`). `kling volume populate` sigue funcionando con
  ellas.
- `scripts/90-e2e.sh` prueba exec, ficheros y sandboxes, y compara con los mensajes en
  inglés del CLI.

## v0.6.0 — 2026-09-23

**El núcleo deja de llevar MCP.** Todo lo de alojar servidores MCP —el puente, el
gateway, el catálogo, `kling mcp`, `add`, `search`, `connect`, `export`, `memory`,
`migrate`— se muda a su propio repositorio y binario,
[kindling-mcp](https://github.com/juan52878911/kindling-mcp) v0.1.0. `kling` sigue
siendo el único comando: con kindling-mcp instalado, esos comandos aparecen en él
como antes, servidos por la extensión.

| kindling | kindling-mcp |
|---|---|
| v0.6.x | v0.1.x |

### Para actualizar

1. Actualiza kindling (`make deploy` o el instalador) y después instala
   kindling-mcp en tu máquina y en el host del daemon (su `make deploy`), que
   instala el puente, el empaquetador, el constructor `mcp` y las unidades
   `kling-gateway` y `kling-heal`, ahora con `ExecStart=/usr/local/bin/kling-mcp`.
2. Nada que migrar a mano: el daemon pasa catálogos, salud y links de v0.4 a
   anotaciones y store, y `kling` mueve la sección `memory` de la configuración a
   `extensions.mcp` (`kling config set mcp.memory.enabled true`).

### Novedades

- **Constructor `base` en el núcleo** (`scripts/81-base-image.sh`): una capa con
  paquetes de apk/apt y `kling-guest` como PID 1. `kling images toolchain` lo usa.
- **Unidades de extensiones en `kling up`**: el manifiesto declara `units` y
  `kling up` las arranca con el daemon si están instaladas.

### Cambios incompatibles

- Fuera del núcleo los comandos MCP y `kling-bridge`; `install.sh --bridge` avisa
  de que el puente viene con kindling-mcp.
- Retiradas las rutas deprecadas en v0.5 (`/snapshots/{n}/catalog`,
  `/snapshots/{n}/health`, `/links`, `/images/refresh-bridge`,
  `/images/{n}/capabilities`) y los campos `tools`/`health*` del snapshot. Ver
  [`docs/api.md`](docs/api.md#rutas-retiradas-en-v06).
- `POST /images` exige `builder`; el proxy al invitado exige `path` (salvo
  `probe_only`).
- `kling images refresh` pasa a ser `kling mcp refresh-bridge`.
- Fuera de `pkg/api` los tipos de MCP (`ToolSpec`, `Link`, `Capabilities`...);
  viven en kindling-mcp.

## v0.5.0 — 2026-09-23

kindling se separa en dos: **el núcleo de microVMs** y **kindling-mcp**, lo que se
venía usando para alojar servidores MCP. Esta versión hace la separación dentro del
repositorio sin que cambie nada de lo que se teclea: `kling mcp import`, `kling add`,
`kling connect` y el gateway funcionan igual, ahora servidos por una extensión
incorporada. El siguiente paso mueve kindling-mcp a su propio repositorio y binario.
Ver [`docs/extensions.md`](docs/extensions.md) y [`docs/api.md`](docs/api.md).

### Novedades

- **Extensiones de `kling`.** Un ejecutable `kling-<nombre>` en el `PATH` (o en
  `$KLING_PLUGIN_PATH`) añade subcomandos a `kling` declarándolos en un manifiesto:
  aparecen en la ayuda y en el completado, y `kling` les pasa el control con `exec`,
  así que códigos de salida y señales llegan intactos. Pueden añadir líneas a
  `kling status` y claves a `kling config`. `kling plugins` las lista. Los comandos
  MCP pasan por este mismo camino como extensión incorporada.
- **Anotaciones de snapshot y store en el daemon**, para que una extensión guarde
  su estado sin que el núcleo lo entienda. El catálogo y la salud de MCP son ahora
  las anotaciones `mcp.tools` y `mcp.health`; los servidores externos enlazados,
  `store/mcp/links`.
- **Constructores de imágenes con nombre.** `POST /images` con `builder` ejecuta un
  constructor de root instalado por el administrador en
  `/usr/local/lib/kindling/builders/`; `kling images build <nombre> -builder <b>`.
- **Ficheros dentro de imágenes**: `kling images cat` e `images put` leen o ponen al
  día un fichero de una imagen ya construida, sin reconstruirla.
- **`kling-guest`**, el agente de invitado genérico (exec, volúmenes, MMDS, DNS)
  para microVMs sin servidor MCP. `kling-bridge` lo embebe.
- **Paquetes públicos para extensiones**: `pkg/api`, `pkg/config`, `pkg/guest`,
  `pkg/scheduler` (planificación genérica del gateway), `pkg/plugin`,
  `pkg/transport`, `pkg/durable`, `pkg/panico`.
- **`GET /info` devuelve la versión real del daemon** (era siempre `0.1.0`) y la
  lista de capacidades del API.

### Cambios que se notan

- **`kling status -json`**: lo del gateway y los agentes pasa de `gateway` y
  `agents` a `extensions.mcp.gateway` y `extensions.mcp.agents`.
- **El proxy al invitado ya no asume MCP.** Quien pasa `path` recibe solo lo que
  pide. Las peticiones sin `path` (clientes v0.4) conservan los valores de antes
  hasta v0.6.
- Las rutas `/snapshots/{n}/catalog`, `/snapshots/{n}/health`, `/links`,
  `/images/refresh-bridge` y `/images/{n}/capabilities` quedan como alias
  deprecados; se retiran en v0.6. Al arrancar, el daemon migra `links.json` al
  store y deja el original como `links.json.migrated`.

### Correcciones

- **`-bundle` copia el `package.json` junto al bundle.** `server-sequential-thinking`
  lee su versión del `package.json` al arrancar, buscándolo junto al fichero que
  ejecuta; el bundle de esbuild vivía solo en `/opt` y el proceso moría con
  "Could not locate package.json for server version", que el gateway devolvía
  como 502 en el `initialize`. Ahora `scripts/80-mcp-image.sh` deja en
  `/opt/package.json` el del paquete que aporta el entry (el mismo que encontraría
  sin empaquetar). De los servidores oficiales de npm sólo éste lo hace; el SDK
  de TypeScript no.

## v0.4.0 — 2026-08-28

Ochenta y un commits desde v0.3.0. La versión va de **que funcione** a **que se
recupere solo y que no se caiga entera**: casi todo lo de abajo salió de mirar el
sistema vivo, no de leer código.

El fallo que mejor resume la tanda: `semgrep` y `playwright` estuvieron **297 horas
caídos** y nadie se enteró, porque la salud sólo se registraba cuando llegaba una
petición. Un servicio que nadie llama se queda roto en silencio hasta que alguien lo
llama.

### Novedades

- **`kling mcp heal`** con temporizador de systemd (`OnBootSec=2min`,
  `OnUnitActiveSec=6h`). Un reinicio del anfitrión invalida **todos** los dorados a
  la vez —Firecracker los ata a la frecuencia del TSC— y hasta ahora había que
  reimportarlos a mano. `heal` sondea, y sólo reconstruye lo que el TSC invalidó:
  un servicio enfermo por otra causa no se arregla rehaciéndolo, y reimportarlo
  sería ruido que tapa el problema real. Reconstruye con la configuración
  **original** —memoria, vCPUs, egress, volúmenes, etiquetas— no con la de por
  defecto.

- **`kling mcp verify` puede fallar.** Antes salía 0 sin ejercitar nada: pedía
  `tools/list` y se daba por satisfecho. Ahora llama a una herramienta de verdad
  (`browser_navigate` sobre `about:blank` en las imágenes de navegador) y consulta
  `/dns` del puente, que devuelve los nameservers del invitado y si resuelve.

- **`kling images rm`**, que se niega si la imagen es base de otra capa, la usa un
  dorado o tiene una máquina viva.

- **Base glibc con `chrome-headless-shell`** (`scripts/71-build-glibc-base.sh`).
  Medido contra el Chromium de Alpine sobre tres sitios reales: **misma cantidad de
  texto extraído**, 637 MB frente a 986,8 MB y 116 ms frente a 401 ms. Chromium se
  queda como opción; el puente no sabe qué motor arranca, lee
  `/etc/kling/browser.json`.

- **`images refresh` hace crecer la imagen** cuando el puente no cabe dentro, en vez
  de fallar. Y **graba la salud** en lugar de sólo imprimir un aviso: al refrescar
  invalida el dorado, y antes el servicio quedaba roto sin que nadie lo supiera.

### Correcciones — el 502 permanente

Tres fallos encadenados que se disfrazaban de uno solo, y que dejaban un servicio
devolviendo 502 para siempre:

- el recolector de basura medía **el sistema de ficheros entero**, así que se
  desataba por disco que no era suyo;
- el gateway **cacheaba la instancia muerta** y seguía marcándola hacia ella;
- la salud se anotaba **al adquirir** la instancia, no según el resultado, así que
  un servicio roto se reafirmaba sano en cada intento fallido.

Medido después: `memory` y `sequentialthinking` pasan de 502 a 200 en **688 ms**.

### Correcciones — seguridad

- **El gateway ya no reenvía su propio token.** Lo mandaba al invitado y a URLs de
  terceros: un servidor MCP comprometido se llevaba la credencial del agregador.
- **Las imágenes dejan de ser world-readable.** Contenían los ficheros `-env` con
  los secretos de cada servicio; ahora se hace `chown` al usuario del servicio con
  `0640`/`0750`.
- **`cpu` no es `cpuset`.** La detección de controladores de cgroup usaba
  `Contains`, y `cpuset` contiene `cpu` como subcadena: el límite se daba por puesto
  sin estarlo. Ahora se compara palabra a palabra.
- **Tests del cortafuegos de salida**: `isBlockedIP` cubre RFC1918, loopback,
  link-local —incluido el `169.254.169.254` de metadatos—, CGNAT, multicast y sus
  equivalentes IPv6; y `ParseEgress` **falla** ante un valor desconocido en vez de
  caer en el más permisivo.

### Correcciones — robustez

- **Los pánicos de los bucles de fondo quedan contenidos.** Había 21 goroutines y
  **cero** `recover()`: un nil-pointer en el reconciliador o en el persistidor de
  estado mataba el proceso y dejaba huérfanas todas las microVM. Se envuelve **cada
  iteración**, no el bucle: contener el bucle entero dejaría el daemon vivo sin
  reconciliar nada, que es peor porque no se nota.
- **Registro de cerrojos con contador de referencias.** El `sync.Map` de antes
  borraba la entrada mientras otra goroutine seguía esperándola. Medido rompiendo el
  código a propósito: **1.758 entradas dobles** en la sección crítica.
- **Volúmenes: comprobar y reservar bajo el mismo cerrojo.** Dos arranques
  simultáneos podían quedarse el mismo volumen exclusivo. `RemoveVolume` tenía la
  misma carrera entre la comprobación y el `os.Remove`.
- **Escritura durable en un solo sitio**: fichero temporal, `fsync` del fichero,
  `rename`, `fsync` del directorio. Antes sólo `state.json` hacía `fsync`;
  `links.json`, la configuración, `meta.json` y las recetas de imagen no.
- **Matar el grupo de procesos, no sólo el pid del hijo**, para que no queden nietos
  huérfanos.
- **Los topes de tamaño fallan en vez de truncar.** Un JSON cortado por la mitad no
  es un JSON pequeño: es ilegible, y el error decía otra cosa.
- **`kling commit` exige que el invitado SIRVA** antes de congelar un dorado. Un
  snapshot tomado antes de tiempo restaura en 26 ms y luego no contesta, minutos u
  horas después, con un error que no menciona el commit.
- **`evictLRU` reponía la víctima** que no se pudo congelar, y prueba con otra en vez
  de rendirse.
- **`MCPPayload` elegía el primer evento SSE**, que puede ser una notificación; ahora
  busca la respuesta.
- **`callLink` reintentaba ante cualquier error**, incluidos los tiempos de espera;
  ahora sólo ante sesión caducada.
- Dos deref nil en `Freeze`/`Thaw`, `Stop` sin el cerrojo de ciclo de vida, `Remove`
  borrando su entrada demasiado pronto, y un firecracker huérfano al hacer `Thaw`.

### Rendimiento

- **El despertar baja de 4.350 ms a 175–202 ms**, y una carga de 20 peticiones de
  44,08 s a 4,66 s. El puente deja un hijo **caliente sin ligar**, así que el dorado
  no paga el arranque del runtime al restaurar.
- **El veredicto de integridad del snapshot se recuerda** en vez de rehashear 512 MiB
  en cada uso.

### Tests

Los cinco paquetes que no tenían ninguno: `internal/report`, `internal/assets`,
`internal/config`, `internal/events` y `cmd/notas-server`. Más los del registro de
cerrojos, la retirada de imágenes, el transporte, el cliente de Firecracker y el
cortafuegos. Cada arreglo se verificó **rompiendo el código a propósito** y
comprobando que el test se pone rojo.

### Actualizar desde v0.3.0

Un cambio de comportamiento, de la auditoría de entorno de más abajo: el puente
escucha en `127.0.0.1:9100` en vez de `0.0.0.0:9100`. Si el gateway corre en otra
máquina, hace falta `-listen 0.0.0.0:9100` explícito.

Instalar el temporizador de autocuración:

```sh
sudo cp packaging/kling-heal.service packaging/kling-heal.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kling-heal.timer
```

### Correcciones — auditoría de supuestos del entorno

Siete fallos que **no daban ningún error**: cada uno reportaba éxito mientras no
hacía lo que decía. Todos verificados en ejecución, no sólo leídos.

- **`kling memory install-service` instalaba un LaunchAgent que no podía arrancar,
  y salía 0.** `bridgePath()` podía devolver la ruta *relativa* `./kling-bridge-local`,
  y el cwd de launchd es `/`: el job moría con `EX_CONFIG` (78), con stderr vacío y
  sin escribir el log. Ahora la ruta es siempre absoluta, y se consulta a launchd si
  el job vive de verdad — `launchctl load` devuelve 0 aunque nunca llegue a arrancar.
  Los dos códigos que no dicen nada por sí solos se traducen: **78** (ruta relativa o
  inexistente) y **126** (sin permiso, típico bajo `~/Documents`, `~/Desktop` o en un
  volumen externo por TCC).

- **El puente ya no se expone a la red por defecto.** `-listen` pasa de
  `0.0.0.0:9100` a `127.0.0.1:9100` en `memory enable` e `install-service`. Lo que se
  envuelve suele ser la memoria personal, el puente **no autentica** —ni puede
  hacerlo de forma útil, porque `kling mcp link` no manda cabeceras— y `/reset` queda
  accesible para cualquiera que alcance el puerto. Exponerlo sigue siendo posible y
  legítimo con el gateway en otra máquina, pero ahora es una decisión y avisa.
  *Cambia comportamiento:* si el gateway corre en otro host, hace falta
  `-listen 0.0.0.0:9100` explícito.

- **El daemon arrancaba «sano» en un host donde no podía hacer nada.** Sólo se
  comprobaban `ip` e `iptables`. Ahora también `firecracker`, `setpriv` y `mkfs.ext4`,
  y los nombra todos de golpe. `setpriv` era el peor: no lo cubría ninguna
  comprobación y fallaba por microVM en pleno arranque, sin relación visible con la
  causa.

- **`install.sh` podía aceptar un checksum sin verificar nada.** Con `sha256sum`
  ausente, `EXPECTED` y `ACTUAL` quedan vacíos y `"" != ""` es falso: la comprobación
  **pasa**. Añadidas las guardas de vacío en los dos bloques.

- **El puente se aceptaba por existir, no por ser ejecutable en el destino.**
  `[ -f ]` no dice nada del formato: un Mach-O de `make bridge-local` pasaba, se
  instalaba como PID 1 de una imagen x86-64, y la microVM reventaba con un pánico del
  kernel sin mencionar al puente. Ahora se comprueba el mágico `\x7fELF`.

- **`printf %q` es de bash y los entrypoints son `#!/bin/sh`.** En Alpine eso es
  busybox ash, y para un argumento con tabulador `%q` emite `$'x\ty'`, sintaxis que
  dash no entiende. Sustituido por entrecomillado POSIX con comilla simple.

- **Una ruta de socket demasiado larga daba `bind: invalid argument`**, sin mencionar
  ni la longitud ni el socket. El límite de `sun_path` son 104 bytes en macOS y 108 en
  Linux; ahora se dice.

## v0.3.0 — 2026-08-13

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.3.0.md`](docs/RELEASE-v0.3.0.md).

La v0.2.0 hizo kindling instalable, autenticado y con estado. Esta lo hace denso, paralelo
y compartido, y estrena soporte (limitado) para Mac Apple Silicon.

### Novedades

- **Misma herramienta en paralelo.** El gateway crea **réplicas por servicio** bajo demanda
  desde el snapshot dorado (COW); varias sesiones concurrentes ya no las topa el cap de
  sesión del puente.
- **`kling migrate`.** Mueve un MCP a kindling **conservando el nombre de la entrada y de
  las herramientas** (endpoint per-servicio): las skills que lo usaban siguen funcionando
  sin reescribirse.
- **Secretos por sesión vía MMDS**, inyectados en la microVM viva; un snapshot congelado
  nunca lleva secretos dentro.
- **Egress allowlist de dominios** (tercer modo, fail-closed): solo salen los dominios
  declarados, con resolver dinámico DNS→ipset.
- **Cuotas por token/tenant** en el gateway (reparto justo).
- **Devolver la RAM**: `kling squeeze` (balloon) reclama la memoria disponible; `/metrics`
  y `kling top` (PSS) hacen visible el peso real, contando el `mem.file` compartido.
- **Modo proxy HTTP/SSE** en el puente: soporta MCP que no hablan stdio.
- **Auto-detección de capacidades** (navegador/internet/nativo) y Chromium compartido con
  contexto por sesión.
- **zram opt-in** en el host para densificar.
- **Mac Apple Silicon (arm64), compatibilidad limitada.** `make deploy-mac` y binarios
  `kling-darwin-arm64`/`kling-linux-arm64`. Requiere M3+ y virtualización anidada; el
  arranque en frío es ~16 s bajo KVM anidado (vs ~3 s en Linux nativo) y el paralelismo
  práctico ronda ~8 réplicas. Límites y receta en [`docs/mac-arm64.md`](docs/mac-arm64.md).

### Correcciones

- `mcp import` respeta `-cpus` y `defaults.mem_mib`.
- El puente **recicla la sesión más ociosa** al llegar al tope (reconexión limpia en
  servicios de 1 sesión); `-e KEY=VAL` para hornear env que apagan el phone-home (semgrep:
  124 s → ~10 s).
- GET sin sesión a un servicio devuelve **405, no 404** (clientes streamable-HTTP cargan
  el endpoint per-servicio).
- Segador/evict sin perder trabajo en vuelo, reconciliación de rutas pegajosas por vida,
  suelo de `MemFree` y cierre del TOCTOU de memoria. Jailer opt-in en frío/restauración,
  matando VMMs huérfanos. Integridad (sha256) y salud del catálogo.

### Actualizar desde v0.2.0

- Corre `kling images refresh`: el puente trae el modo proxy y la inyección de secretos.
- El paralelismo de la misma herramienta no pide configuración; ajusta las cuotas por
  tenant si repartes un mismo token.
- En Mac: necesitas M3+ y `nested virt` (ver [`docs/mac-arm64.md`](docs/mac-arm64.md)).

## v0.2.0 — 2026-08-11

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.2.0.md`](docs/RELEASE-v0.2.0.md).

La v0.1.0 demostraba que la idea funciona: microVMs que descongelan en milisegundos y un
gateway que las despierta bajo demanda. Esta la hace instalable, autenticada y capaz de
guardar estado.

### Novedades

- **`kling up` y `kling status`.** Instalar deja de ser tres scripts a mano como root: el
  kernel y la imagen base van dentro del binario.
- **El gateway exige token.** Despertar un snapshot es ejecutar código, y el gateway es lo
  único que escucha en red. Se genera solo la primera vez.
- **7 clientes de IA** en `connect -install`, frente a 2.
- **Catálogo oficial**: `kling search` y `kling add` contra `registry.modelcontextprotocol.io`.
- **Volúmenes persistentes**, con journal, hasta cuatro por microVM y compartibles en solo
  lectura: un escritor exclusivo, o cuantos lectores hagan falta.
- **`kling volume populate`**: instala paquetes DENTRO de una microVM desechable en vez de
  como root en el anfitrión.
- **`kling images toolchain`**, **`kling images refresh`** y **`kling images recipe`**.
- **NODE_PATH y PYTHONPATH automáticos** apuntando a los volúmenes que traen paquetes.

### Correcciones

Nueve fallos que solo aparecen metiendo servicios de verdad, entre ellos: `mcp import`
ignoraba `defaults.mem_mib` (la causa real de los timeouts en paralelo que se achacaban al
gateway), los snapshots no guardaban ni su política de red ni su volumen, los volúmenes se
formateaban sin journal, y el segador del gateway congelaba microVMs con trabajo en vuelo.

### Actualizar desde v0.1.0

- El gateway pide token: cópialo con `kling config set gateway.token …`.
- Reimporta los servicios: sus snapshots no guardan la política de red.
- Corre `kling images refresh`: el puente vive dentro de cada imagen.

## v0.1.0 — 2026-08-08

Primera release con binarios distribuidos. Antes de esta versión, `kling` solo
estaba disponible vía `make install` (compilación local).

### Novedades

- **Instalación con una línea** desde releases: `curl -fsSL .../install.sh | sh`.
  Sin Go instalado, sin clonar el repo, sin sudo.
- **Releases multi-plataforma** vía GitHub Actions: binarios pre-compilados para
  Linux (amd64/arm64), macOS (amd64/arm64). El bridge se publica por separado
  porque va dentro de las microVMs (estático, musl-safe).
- **Verificación SHA256** antes de instalar: cada release incluye `SHA256SUMS`
  y el instalador aborta si el checksum no coincide.
- **CLI `--dry-run`** para previsualizar qué se descargará y dónde quedará.

### Arreglos desde la última versión funcional (HEAD)

- **Snapshots stateful ya no rompen los restores.** El daemon ahora expone
  `POST /reset` en el bridge, y `kling mcp import` lo invoca (o espera al
  auto-reset del wrapper HTTP) antes de hacer commit. Sin esto, los snapshots
  dorados se congelaban con el servidor ya inicializado, y al restaurar el
  puerto 8080 nunca abría o el handshake daba 400/406.
- **`everything` (HTTP nativo)** ya funciona end-to-end con el wrapper de
  auto-reset (`/var/run/kling-http-reset-done` persiste en el overlay).
- **`filesystem-mcp` (stdio + bridge)** ya funciona end-to-end: el bridge
  tiene el endpoint `/reset` y `mcpImport` lo invoca tras capturar el catálogo.
- **CLI actualizado detecta las nuevas respuestas del daemon** — antes, un CLI
  viejo podía mostrar mensajes de error engañosos.

### Limitaciones conocidas

- **Windows no soportado.** `internal/machine/manager.go` usa `syscall.Kill`,
  `Setsid`, `Stat_t` que son POSIX. Si necesitas Windows, abre un issue.
- **Solo se publica el CLI para macOS** — el daemon requiere KVM, que en macOS
  no existe fuera de máquinas virtuales con VT-x anidado (que es justamente
  cómo se mide aquí: Proxmox + KVM + Firecracker).
- **El bridge solo se publica para Linux.** Va dentro de microVMs Alpine (musl);
  en macOS no tiene sentido empaquetarlo.