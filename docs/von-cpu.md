# VON en CPU: qué lo hizo más rápido y qué no

Fase 2 de [VON](von.md): que los modelos pequeños de kindling (llama.cpp dentro
de una microVM, congelados en un dorado) contesten antes y cuesten menos en una
máquina **sin GPU**. El método es el del proyecto VibeVoice (7× en CPU): cada
palanca tiene un banco de pruebas reproducible y una **puerta explícita** —entra
en los valores por defecto solo si mejora la métrica medida **sin empeorar la
calidad**—, y lo que no funcionó se cuenta con el mismo cuidado que lo que sí.

Todo se midió con [`scripts/97-von-cpu-bench.sh`](../scripts/97-von-cpu-bench.sh)
sobre una tarea realista (abajo). Solo modelos que se pueden usar **y
redistribuir** (Apache-2.0).

## El viaje

Qwen2.5-1.5B-Instruct, 4 vCPU, macOS `vz` en un Mac mini M4. La métrica que
importa en un gateway que escala a cero es **el primer token de la primera
petición de una tarea en una réplica recién restaurada**: es lo que paga cada
despertar.

| Paso | Métrica | Antes → después | Ganancia | Por qué |
|---|---|---|---|---|
| 1. Prefijo de la tarea precalculado en el dorado (`--cache-ram` + `kling ai prime`) | primera petición de la tarea tras restaurar | 4731 → **381 ms** | **12×** | el system prompt (~800 tokens) ya está evaluado en la memoria del dorado; la réplica solo evalúa la pregunta (~20 tokens) |
| 1b. …y la caché de prompts en cada réplica | primer token al alternar dos tareas en la misma réplica | 2773 → **112 ms** | **25×** | `llama-server` guarda la caché KV de cada prefijo y la recupera al volver a esa tarea, en vez de reevaluarlo |
| 1c. Lo mismo en Qwen2.5-0.5B Q8_0 (2 vCPU) | primera petición tras restaurar | 1710 → **168 ms** | 10× | ídem |
| 2. Salida restringida con `json_schema` | respuestas JSON válidas para el esquema | 19/21 → **21/21** | 100 % válido | la gramática que `llama-server` saca del esquema no deja emitir otra cosa; cuesta ~10 % de velocidad de generación |
| 3. Q4_0 en vez de Q4_K_M (1,5B) | prompt de 806 tokens sin caché | 4904 → **2467–2958 ms** | 1,7–2× | llama.cpp reempaqueta Q4_0 para las instrucciones i8mm del M4 (y no Q4_K_M); generación +10 % (53 → 59 tok/s) |
| 3b. Q4_0 en vez de Q8_0 (0,5B, 2 vCPU) | generación, tok/s | 90–102 → **132–135** | 1,35× | menos bytes por token con el mismo reempaquetado; prompt igual y acierto igual o mejor |
| 4. Decodificación especulativa (borrador 0,5B, 135M o n-gramas) | generación, tok/s | 56 → 31–56 | **ninguna: siempre igual o peor** | en CPU verificar k tokens no sale gratis, y el borrador compite por los mismos núcleos |
| 5. Hilos, lotes, `--poll`, caché KV en Q8_0 | prompt y generación | — | ninguna por encima del ruido | los valores por defecto (un hilo por vCPU, flash attention `auto`) ya son los buenos |

Lo que entra en los valores por defecto: **1** (con `-cache-ram 64` en las
imágenes nuevas y `kling ai prime` / `-prefix` para llenar la caché del dorado)
y **2** (por tarea, `json_schema` en el registro del gateway). **3** y **3b** entran en el
catálogo (`-quant q4_0`) sin cambiar la cuantización por defecto; ver
[Configuración recomendada](#configuración-recomendada-por-modelo).

## La tarea y el método

`scripts/von-bench/` tiene una tarea de domótica: un system prompt de ~790
tokens (22 dispositivos con lo que aceptan, el formato de respuesta, reglas y
tres ejemplos) y 21 peticiones cortas con su respuesta esperada (las acciones
exactas), más una segunda tarea (triaje de tickets, ~540 tokens) para medir el
cambio de tarea. Es el caso típico de un modelo pequeño detrás del gateway: un
prefijo largo y fijo por tarea, una pregunta corta, una respuesta JSON corta.

```sh
S=scripts/97-von-cpu-bench.sh
$S restart <máquina> --threads 2          # relanza llama-server con args de prueba
$S ttft <máquina>                         # primer token con el prefijo sin caché y con él
$S prefix <dorado>...                     # primera petición en réplicas recién restauradas
$S switch <máquina>                       # dos tareas alternas en la misma réplica
$S gen <máquina>                          # tok/s, aceptación del borrador, acierto
$S schema <máquina>                       # JSON válido y acierto con y sin json_schema
```

- **Calidad**: "acierto" es que el conjunto de acciones sea exactamente el
  esperado (dispositivo, acción y valor), a temperatura 0. Con 21 ejemplos, una
  diferencia de 1–2 aciertos está dentro del ruido; una palanca que baja más
  que eso no entra.
- **Tiempos**: directos a la réplica (reenvío de `kling-vz`), sin el proxy del
  daemon; tok/s de los `timings` de `llama-server`. Medianas de 3–5 repeticiones.
- **Ruido**: el Mac tiene al lado la VM de Lima del laboratorio (8 GiB), que a
  ratos carga CPU (otro trabajo lanzó embeddings con 6 hilos durante la sesión
  y bajó la generación de 56 a 33 tok/s). Las series de las palancas 3–5 se
  repitieron esperando a que la VM de Lima estuviera por debajo del 60 % de un
  core, y las comparaciones se intercalan (A, B, A, B). Aun así hay ±10 % entre
  series: solo cuenta lo que queda por encima.
- **Dónde**: el Mac mini M4 (4 núcleos de rendimiento + 6 de eficiencia, 16 GiB),
  backend `vz`, es lo más parecido a "CPU sin anidar" que hay a mano. **No había
  un x86 en hierro**: nada de esto está medido en x86 (AVX2/AVX-512), y la
  palanca 3 depende de la CPU (abajo). El laboratorio Linux (Firecracker bajo
  KVM anidado) solo sirve para comprobar lo que es de Linux, no velocidades
  ([von.md](von.md#el-laboratorio-anidado-por-qué-sus-tiempos-no-cuentan)).

## 1. El prefijo de la tarea, precalculado

En un modelo de 1,5B en CPU, **evaluar el prompt es casi todo el coste de una
respuesta corta**: con el prefijo de la tarea sin caché, el primer token tarda
4,4–4,9 s; con el prefijo ya en la ranura, 131–164 ms. `llama-server` ya
reutiliza el prefijo común con la petición anterior (`cache_prompt`), así que
en una réplica caliente que solo sirve una tarea esto ya pasaba. Lo que faltaba
es lo que importa al escalar a cero: la **primera** petición de cada réplica
recién restaurada, y las réplicas que sirven **varias** tareas.

Se midieron tres variantes contra el dorado actual (el que se calienta con un
"hello"), con `97-von-cpu-bench.sh prefix` (Qwen2.5-1.5B Q4_K_M, 4 vCPU, n=5):

| Variante | primera petición | `run -from` → primer token | coste |
|---|---|---|---|
| dorado actual | 4731 ms (4585–5465), evalúa 809 tokens | 7907 ms | — |
| (a) un dorado por tarea, congelado tras evaluar su prefijo | 414 ms (358–529), evalúa 20 | 2729 ms | un dorado entero **por tarea** (1220 MiB cada uno en disco; en Linux, sin compartir páginas entre dorados de tareas distintas) |
| (b) un dorado por modelo + `--slot-save-path`, restaurando la ranura de la tarea (`POST /slots/0?action=restore`) antes de la petición | 338 ms (234–468), de ellos 65 ms la restauración | 1645 ms | 22 MiB de fichero por tarea (806 tokens × ~28 KiB) dentro del dorado; **una llamada más por petición** y el gateway tiene que saber qué ranura tiene cada réplica |
| (c) un dorado por modelo + `--cache-ram 64`, congelado tras evaluar los prefijos de **todas** sus tareas | **381 ms** (329–501), evalúa 20 | 2105 ms | +32 MiB en el dorado con dos prefijos; +64 MiB de memoria en la VM |

Repetido al final de la sesión con la guarda de ruido (abajo), dorado actual y
(c) intercalados, n=3 cada serie: 5628 ms (5268–6545) frente a **476 ms**
(461–604), y en otra pareja 205 ms para (c) (la del dorado actual de esa pareja,
18,8 s, coincidió con presión de memoria en el Mac y no cuenta).

Las tres llevan la primera petición del orden de 4,7 s a 0,3–0,4 s; las
diferencias entre ellas están dentro del ruido del Mac (el tiempo de restaurar
varía de 1,3 a 3 s según la presión de memoria). Lo que las separa es el coste:

- **(a)** multiplica dorados por tareas. En Linux la gracia del dorado es que N
  réplicas comparten sus páginas; dorados distintos no comparten nada.
- **(b)** necesita que el gateway lleve la cuenta de qué tarea tiene cada
  réplica en su ranura y haga una llamada de restauración cuando cambia, y los
  ficheros de ranura viven en el disco de cada réplica.
- **(c)** no necesita nada en el gateway: `llama-server` guarda en su caché de
  prompts el estado de cada prompt que ve y, cuando llega uno que empieza igual
  que uno guardado, lo recupera. El dorado se congela con esa caché ya llena, y
  como también sirve en caliente, **cambiar de tarea en la misma réplica** deja
  de costar el prefijo:

| Dos tareas alternas en una réplica (`switch`, n=8) | primer token | tokens evaluados |
|---|---|---|
| `--cache-ram 0` (antes) | 2773 ms (281–4247) | 567 (20–807) |
| `--cache-ram 64` | **112 ms** (64–354) | 19 (9–25) |

**Decisión: (c).** `kling models add` construye las imágenes con
`--cache-ram 64` (`-cache-ram N` lo cambia, 0 lo quita) y suma esos MiB a la
memoria de la microVM: sin sumarlos, un dorado de 1,5B en 1536 MiB se quedó sin
memoria dos veces al congelar (el agente dejó de contestar con la caché llena).
Los prefijos se evalúan al hacer el dorado: `kling models add -prefix
system.txt` (repetible) o, con el gateway, `kling ai prime`, que saca de cada
tarea su `system` y el texto fijo de su plantilla hasta la primera variable, y
rehace el dorado de cada modelo VON (etiqueta `von.prefixes` con el hash; si no
cambió, no hace nada). 64 MiB caben ~2300 tokens de Qwen2.5-1.5B (~28 KiB por
token de caché KV) o ~5000 de Qwen2.5-0.5B: dos o tres tareas de 800 tokens en
el mayor. En Qwen2.5-0.5B Q8_0 (2 vCPU, n=5): la primera petición pasa de
1710 ms (1677–1792) a **168 ms** (161–217), y `run -from` → primer token de
2657 a 1153 ms.

Las imágenes anteriores (con `--cache-ram 0`) siguen sirviendo: `kling models
add` las reutiliza con `-cache-ram 0`, y entonces el dorado conserva solo el
último prefijo (la variante a, que también vale para un modelo con una sola
tarea).

## 2. Salida restringida: `json_schema`

`llama-server` acepta un `json_schema` por petición y lo convierte en una
gramática: el muestreo solo puede emitir JSON que lo cumpla. Con la tarea de
domótica (esquema con la lista de dispositivos y acciones como `enum`),
Qwen2.5-1.5B Q4_K_M, temperatura 0, 21 peticiones:

| | JSON válido para el esquema | acierto | petición (prefijo en caché) | generación |
|---|---|---|---|---|
| sin esquema | 19/21 (luego 20/21) | 16–17/21 | 807–860 ms | 52–53 tok/s |
| con `json_schema` | **21/21** (en las tres series) | 16/21 | 801–961 ms | 47–55 tok/s |

El esquema no hace al modelo más listo (el acierto no cambia: se equivoca de
dispositivo o de valor, no de formato), pero **elimina las respuestas que no se
pueden leer**. Cuesta poco: en las series intercaladas la petición entera no se
movió por encima del ruido; en la primera, 961 frente a 807 ms. **Decisión**: las
tareas de generación del gateway aceptan `json_schema` (se lo pasa a
`llama-server` tal cual), y el gateway comprueba además que la salida sea JSON
—el invitado no es de fiar, y una respuesta cortada por `max_tokens` es JSON a
medias— y si no lo es contesta 502 con el `finish_reason`. No es un valor por
defecto porque cada tarea tiene su esquema.

## 3. Cuantización e hilos

Qwen2.5-1.5B, 4 vCPU, series intercaladas (A, B, A, B), prompt de 806 tokens
sin caché y generación a temperatura 0:

| | prompt (ms) | generación JSON | párrafo (160 tok) | acierto (esquema / libre) |
|---|---|---|---|---|
| Q4_K_M (el del catálogo) | 4904, 4921 | 52–53 tok/s | 63 tok/s | 16/21 / 17/21 |
| **Q4_0** | **2958, 2467** | **59 tok/s** | **70 tok/s** | 16/21 / 15/21 |

Q4_0 evalúa el prompt ~1,8× más rápido y genera ~10 % más: llama.cpp
reempaqueta en memoria los pesos Q4_0 (y Q8_0) al formato de las instrucciones
i8mm de la CPU, y los Q4_K_M no. Es la misma razón por la que, en
[von.md](von.md), Q8_0 evaluaba el prompt más rápido que Q4_K_M. En calidad, con
el esquema, empate (16/21); sin él, 15 frente a 17, dentro del ruido de 21
ejemplos. **Decisión**: Q4_0 entra en el catálogo (`-quant q4_0` de Qwen2.5-0.5B
y 1.5B, mismo repositorio oficial y revisión), recomendado para tareas con
prefijo largo o salida restringida; la cuantización por defecto no cambia. En
x86 el reempaquetado es otro (AVX2/AVX-512) y **no está medido**.

Hilos y demás, en la misma VM (una serie cada uno, series de ±10 %):

| Cambio | prompt (ms) | generación | Veredicto |
|---|---|---|---|
| `--threads 4` (= vCPU, el defecto) | 4363 | 55 tok/s | referencia |
| `--threads 3` | 5105 | 52 | peor |
| `--threads 2` | 7419 | 45 | peor |
| `--threads 2 --threads-batch 4` | 4399 | 46 | el prompt vuelve; la generación no |
| `--poll 0` (sin espera activa) | 4400 | 55 | igual |
| `-ub 1024` (un solo lote para el prompt) | 4486 | 54 | igual |
| `-fa off` (flash attention; defecto `auto`) | 5484 | 52 | peor: `auto` la activa y ayuda |
| `-ctk q8_0 -ctv q8_0 -fa on` (caché KV en Q8_0) | 5448 | 52 | peor; ahorra 28 MiB de 56 con `-ctx 2048` |

Más vCPU que núcleos de rendimiento: la misma imagen con **6 vCPU** (el M4
tiene 4 núcleos de rendimiento y 6 de eficiencia) y `--threads 6` evalúa el
prompt en 3864–4354 ms frente a 5221 con 4 hilos en esa misma VM, y el párrafo
sube de 59 a 66–69 tok/s; las respuestas JSON cortas, igual (51–52). Los
núcleos de eficiencia suman algo al prompt, pero con el prefijo precalculado
(palanca 1) lo que queda de prompt son ~20 tokens: no compensa pagar dos vCPU
más por réplica. Los 4 vCPU del catálogo se quedan.

**Qwen2.5-0.5B** (2 vCPU), Q8_0 frente a Q4_0, intercaladas:

| | prompt (ms) | generación JSON | párrafo | JSON válido sin / con esquema | acierto sin / con esquema |
|---|---|---|---|---|---|
| Q8_0 (el defecto) | 1592, 1528 | 90–102 tok/s | 99–119 tok/s | 11/21 / 21/21 | 7/21 / 10/21 |
| **Q4_0** | 1542, 1485 | **132–135 tok/s** | **166–169 tok/s** | 12/21 / 21/21 | 10/21 / 12/21 |

Aquí Q4_0 evalúa el prompt igual que Q8_0 (los dos van reempaquetados), genera
un ~35 % más rápido (menos bytes que leer por token) y no acierta menos. Y es en
el 0,5B donde el esquema más se nota: sin él, **solo 11 de 21** respuestas son
JSON válido para la tarea.

**Decisión**: nada de esto cambia. Un hilo por vCPU sigue siendo lo mejor, y la
caché KV en Q8_0 ahorra muy poco con contextos de 2048 a cambio de un 10–20 %
de velocidad; `--mlock` no se usa (en una microVM no hay a dónde paginar: la
memoria del invitado es la de la VM) y el contexto sigue en 2048 (la caché KV se
reserva entera: 56 MiB en Qwen2.5-1.5B, el doble con 4096, en cada réplica).

## 4. Lo que no funcionó: decodificación especulativa

La idea: un modelo pequeño (el borrador) propone k tokens y el grande los
verifica de una vez; si acierta, salen varios tokens por pasada del grande.
`llama-server` lo hace con `--model-draft` y `--spec-type draft-simple`
(`--spec-draft-n-max`, `--spec-draft-p-min`), y sin borrador con n-gramas del
propio texto (`--spec-type ngram-simple|ngram-mod|ngram-map-k`). Mismo
tokenizador en cada pareja; `gen` a temperatura 0 y 0,7 con las respuestas JSON
de la tarea y con un párrafo de 160 tokens:

| Objetivo ← borrador | config | JSON: tok/s (aceptación) | párrafo: tok/s (aceptación) |
|---|---|---|---|
| Qwen2.5-1.5B Q4_K_M, sin borrador | — | **56** | **62** |
| ← Qwen2.5-0.5B Q8_0 | n-max 3 | 49 (91 %) | 42 (55 %) |
| ← Qwen2.5-0.5B Q8_0 | n-max 8 | 47 (85 %) | 23 (28 %) |
| ← Qwen2.5-0.5B Q8_0 | n-max 16 | 35 (72 %) | 16 (21 %) |
| ← Qwen2.5-0.5B Q4_0 | n-max 3 | 43 (90 %) | 35 (55 %) |
| ← Qwen2.5-0.5B Q4_0 | n-max 4, p-min 0,75 | 47 (96 %) | 39 (92 %) |
| n-gramas | `ngram-simple` / `ngram-mod` / `ngram-map-k` | 56 / 51 / 45 (4 %, —, 4 %) | 60 / 59 / 59 |
| SmolLM2-1.7B Q4_K_M, sin borrador | — | **44** | **58** |
| ← SmolLM2-135M Q8_0 | n-max 3 | 31 (78 %) | 38 (45 %) |
| ← SmolLM2-135M Q8_0 | n-max 6, p-min 0,75 | 32 (93 %) | 44 (92 %) |
| SmolLM2-360M Q8_0 (2 hilos), sin borrador | — | **86** | **132** |
| ← SmolLM2-135M Q8_0 | n-max 3 | 65 (75 %) | 103 (49 %) |

(Temperatura 0; a 0,7 las mismas conclusiones, con menos aceptación.) **Ninguna
configuración gana**, ni con un 96 % de aceptación. Por qué, en CPU:

- **Verificar no sale gratis.** En GPU verificar k tokens cuesta casi lo mismo
  que generar uno (la generación está limitada por el ancho de banda y sobra
  cálculo). En estos núcleos no tanto: Qwen2.5-1.5B Q4_K_M evalúa ~160 tokens/s
  de prompt frente a ~60 de generación, así que verificar 4 tokens cuesta ~1,5
  veces uno generado, no 1.
- **El borrador no es tan pequeño** en bytes, que es lo que cuenta: el 0,5B Q8_0
  pesa 644 MiB frente a los 1066 del 1,5B Q4_K_M (1,6×). La pareja más
  favorable, 135M (145 MiB) para 1,7B (1007 MiB), 7×, tampoco ganó.
- **Compiten por los mismos núcleos**: borrador y objetivo corren uno detrás de
  otro con los mismos hilos, y cada paso del borrador es un lanzamiento de grafo
  más con su sincronización de hilos.

Con la aceptación baja (párrafos a 0,7: 14–55 %) es peor todavía. La puerta la
cierra: **no entra, ni como opción de `kling models`**: añadir un segundo GGUF a
la imagen y a la memoria de cada réplica (+145–700 MiB) para ir más lento no
tiene caso. Queda documentado para repetirlo si cambia el hardware (más núcleos
que ancho de banda, o x86 con AVX-512) o llega un borrador tipo EAGLE/MTP para
estos modelos.

También se probó y no se adoptó: el Qwen2.5-7B (Apache-2.0, ~4,7 GB en Q4_K_M)
**no se midió**: no cabe al lado de la VM de Lima en un Mac de 16 GiB con su
réplica (en `vz` cada réplica paga su memoria entera) sin pararla, y el
presupuesto de disco de la sesión tampoco daba.

## Configuración recomendada por modelo

| Modelo | Cuantización | vCPU / memoria | Mandos | Para qué |
|---|---|---|---|---|
| SmolLM2-360M-Instruct | Q8_0 | 2 / 768 + 64 MiB | defecto; `-prefix` o `ai prime` | respuestas cortas y baratas; en la tarea de domótica acierta poco (1/8): no para JSON con muchas opciones |
| Qwen2.5-0.5B-Instruct | **Q4_0** (o Q8_0, el defecto) | 2 / 896 + 64 MiB | prefijos precalculados; `json_schema` siempre que la salida sea JSON (sin él, la mitad no lo es) | el más barato que sigue un formato; primera petición de 1,7 s a 0,17 s con el prefijo; acierta 10–12/21 en la tarea de domótica |
| Qwen2.5-1.5B-Instruct | **Q4_0** (latencia) o Q4_K_M | 4 / 1536 + 64 MiB | prefijos precalculados, `json_schema` en tareas JSON | el que acierta en la tarea de domótica (16–17/21); primera petición de 4,7 s a 0,4 s, prompt ~1,8× más rápido en Q4_0 |

En todos: un hilo por vCPU, `-ctx 2048`, sin decodificación especulativa, caché
KV en F16.

## Laboratorio Linux

Lo que es de Linux se comprobó en la VM de Lima `kling-arm` (Firecracker bajo
KVM anidado; los tiempos no valen, las proporciones y la memoria sí), con
SmolLM2-360M Q8_0 y la imagen de siempre relanzada con `--cache-ram 64`:

- **La caché de prompts sobrevive a la restauración de Firecracker**: una réplica
  recién restaurada del dorado con los dos prefijos reutiliza 891 de 912 tokens
  en su primera petición (evalúa 21). La primera petición pasa de 18,3 s
  (16,8–19,1, evalúa 907) a **10,6 s** (10,2–10,7); `run -from` → primer token de
  18,7 a 11,0 s (n=3). Aquí gana menos que en el Mac porque en el anidado lo que
  domina esa primera petición son los fallos de página de los pesos (~1 ms cada
  uno, [von.md](von.md#el-laboratorio-anidado-por-qué-sus-tiempos-no-cuentan)),
  que el prefijo no evita.
- **La compartición de páginas se mantiene** (PSS de `kling top` tras una
  petición de la tarea en cada réplica):

| réplicas | dorado sin prefijos (527 MiB) | dorado con dos prefijos (619 MiB) |
|---|---|---|
| 1 | 501 MiB | 530 MiB |
| 2 | 570 MiB (287 + 283) | 599 MiB (300 + 299) |
| 3 | 640 MiB (3 × ~213) | 667 MiB (3 × ~222) |

  Los prefijos cuestan ~29 MiB una vez (son páginas del dorado, compartidas) y
  cada réplica de más cuesta lo mismo que antes (~69 MiB: la caché KV de la
  petición que escribe). El `mem.file` crece 92 MiB (527 → 619): 64 de la VM
  más grande (832 frente a 768 MiB, con la caché sumada) y la caché KV de los dos
  prefijos, que en SmolLM2 es grande (~40 KiB por token).

La construcción de una imagen nueva con `--cache-ram 64` (el constructor `llm`)
está cubierta por los tests de `pkg/von` (`RunScript`) pero no se hizo en el
laboratorio: su daemon es compartido y no se le cambió el binario. De punta a
punta en el Mac sí se probaron `kling models add -prefix`, `kling ai prime`
(dos tareas; repetirlo no hace nada) y el gateway sirviendo una tarea con
`json_schema` desde el dorado resultante, con una imagen anterior
(`-cache-ram 0`, en la que solo queda el último prefijo: el aviso de `ai prime`
lo dice).

## Reproducir

```sh
# macOS (vz): imagen construida en un Linux y copiada (von.md), dorado aquí
kling models add von-qwen15 -model qwen2.5-1.5b-instruct -quant q4_0 -prefix scripts/von-bench/smarthome-system.txt
S=scripts/97-von-cpu-bench.sh
RUNS=5 $S prefix von-qwen15                     # primera petición en réplicas recién restauradas
kling run -from von-qwen15 -name q1 && $S switch q1 && $S schema q1 && $S gen q1
# una palanca a mano: relanzar llama-server con otros argumentos (-allow-exec)
$S restart q1 --threads 2 && $S ttft q1 && $S gen q1
```
