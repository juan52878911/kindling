# VON: modelos de lenguaje pequeños bajo demanda

VON son LLM *instruct* pequeños —SmolLM2-360M, Qwen2.5-0.5B y 1.5B— servidos desde
microVMs de kindling con la API compatible con OpenAI de `llama-server`
(llama.cpp), que escalan a cero y vuelven desde un **snapshot dorado congelado con
el modelo ya cargado y caliente**. Es la mitad "generativa" de la cascada que
vendrá: un clasificador lineal diminuto (JEV) contesta lo fácil y lo demás pasa a
VON, detrás de un gateway con API de OpenAI.

## La tesis, y qué dicen los números

En Linux/Firecracker la memoria de un dorado se mapea **perezosamente** desde su
`mem.file` (`MAP_PRIVATE`): una réplica restaurada no carga nada, sus páginas
llegan de la caché de páginas del host según las toca, y **N réplicas del mismo
dorado comparten las páginas de los pesos** que ninguna ha escrito. Lo medido:

- **Compartir memoria: probado.** Cuatro réplicas de SmolLM2-360M Q8_0 suman
  **461 MiB de PSS** donde la suma de sus RSS es 1715 MiB. Cada réplica de más
  cuesta **~11–14 MiB** (su caché KV tocada, su pila y lo que escribe), no los
  ~530 MiB de un `llama-server` más. Tabla abajo.
- **Primer token desde cero: depende de dónde.** El thaw son ~220 ms en el
  laboratorio Linux; el primer token, **4,3 s**, porque la primera petición trae
  los ~500 MiB de pesos a base de fallos de página, y en ese laboratorio (KVM
  anidado en un Mac) cada fallo cuesta cerca de un milisegundo. En macOS (`vz`,
  sin anidar) el dorado da el primer token en **~0,8 s** frente a ~1,4 s en frío.
- **Velocidad: el laboratorio Linux no sirve para medirla.** Bajo virtualización
  anidada el cómputo del invitado va 15–20× más lento que el mismo binario en el
  host (8,8 tok/s frente a 151). En el Mac, un invitado `vz` genera a
  **~140 tok/s** (SmolLM2) y **~110 tok/s** (Qwen2.5 Q8_0) con 2 vCPU. Sección
  [El laboratorio anidado](#el-laboratorio-anidado-por-qué-sus-tiempos-no-cuentan).

## Uso

### Linux (Firecracker)

```sh
kling models add von-smol -model smollm2-360m-instruct          # Q8_0 por defecto
kling models add von-qwen -model qwen2.5-0.5b-instruct -quant q8_0
kling models ls

kling run -from von-smol -name smol-1                             # una réplica
kling models ask smol-1 "What is a microVM? One sentence."
```

`models add` construye la imagen (llama.cpp + el GGUF) con el constructor `llm`
del daemon, arranca una microVM, espera a `/health`, la calienta con una respuesta
corta, la congela como dorado `von-smol` y la borra. La primera vez también se
construye la base glibc (Debian trixie, `debootstrap` en el host: `apt-get install
debootstrap`) y se descargan llama.cpp (13 MiB) y el modelo; lo descargado queda en
`$KLING_ROOT/cache/von` por hash y reconstruir tarda segundos.

Cada réplica sirve la API de OpenAI en el **puerto 8000** del invitado. El host
llega por la IP de la máquina (`kling inspect smol-1 | jq -r .ip`) o, sin tocar la
red, por el proxy del daemon (`POST /machines/{ref}/guest` con `"port": 8000`),
que es lo que usa `models ask`:

```sh
curl -s http://$(kling inspect smol-1 | jq -r .ip):8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Hi"}],"max_tokens":32,"stream":true}'
```

Rutas: `/v1/chat/completions` (con `stream`), `/v1/completions`, `/v1/models`
(devuelve el ID del catálogo, `smollm2-360m-instruct:q8_0`), `/health` y
`/metrics`. Escalar a cero: `kling freeze <réplica>` la deja en disco sin CPU ni
RAM y `kling thaw` la vuelve en milisegundos; `-ttl` congela sola una réplica al
vencer (su reloj lo reinicia `exec`, no el tráfico HTTP). Congelar por
inactividad real y despertar por petición es trabajo del gateway, con
`pkg/scheduler`, como hace kindling-mcp.

### macOS (`vz`)

Un daemon de macOS no construye imágenes. Se construye en un Linux arm64, se copia,
y el dorado se hace en el Mac (un dorado es de su backend y de su host):

```sh
kling models add -H ssh://lab von-smol -model smollm2-360m-instruct -build-only
kling images copy von-smol -from ssh://lab
kling models add von-smol -model smollm2-360m-instruct    # reutiliza la imagen
kling run -from von-smol -name smol-1
curl -s http://$(kling inspect smol-1 | jq -r '.forwards["8000"]')/v1/models
```

### Catálogo y mandos

| Modelo (`-model`) | `-quant` | GGUF | vCPU / memoria por defecto | Origen fijado | Licencia |
|---|---|---|---|---|---|
| `smollm2-360m-instruct` | `q8_0` (defecto) | 369 MiB | 2 / 768 MiB | HuggingFaceTB, rev `593b5a2e` | Apache-2.0 |
| `smollm2-360m-instruct` | `q4_k_m` | 258 MiB | 2 / 640 MiB | bartowski, rev `7be6f65f` | Apache-2.0 |
| `qwen2.5-0.5b-instruct` | `q8_0` | 644 MiB | 2 / 1152 MiB | Qwen, rev `9217f5db` | Apache-2.0 |
| `qwen2.5-0.5b-instruct` | `q4_k_m` | 469 MiB | 2 / 896 MiB | Qwen, rev `9217f5db` | Apache-2.0 |
| `qwen2.5-1.5b-instruct` | `q8_0` | 1807 MiB | 4 / 2304 MiB | Qwen, rev `91cad511` | Apache-2.0 |
| `qwen2.5-1.5b-instruct` | `q4_k_m` | 1066 MiB | 4 / 1536 MiB | Qwen, rev `91cad511` | Apache-2.0 |
| `qwen2.5-3b-instruct` | `q4_k_m` (única) | 2007 MiB | 4 / 2816 MiB | Qwen, rev `7dabda4d` | **Qwen Research** (no comercial): fuera del catálogo por defecto |

Cada entrada lleva revisión de Hugging Face (commit, no rama), sha256, licencia
y dónde leerla (`pkg/von`, `kling models ls`); el constructor verifica el hash
antes de meter nada en la imagen.

**Licencias.** En el catálogo por defecto solo entran modelos que se pueden usar
**y redistribuir** libremente, también con fines comerciales (Apache-2.0, MIT):
un dorado es una copia de los pesos que viaja entre daemons con `images copy`.
Qwen2.5-3B-Instruct está bajo la Qwen Research License (ficha de Hugging Face:
`license: other`, `license_name: qwen-research`): no sale en la lista de modelos y
`models add` la rechaza salvo que se acepte a propósito, con su identificador
exacto: `-accept-license qwen-research` (imprime la licencia y su enlace). Está
en el catálogo para evaluar ([ai-gateway.md](ai-gateway.md)), no para servir.
Candidatos abiertos por medir: Qwen2.5-7B-Instruct (Apache-2.0, ~4,7 GB en
Q4_K_M: no cabe al lado de la VM de Lima en un Mac de 16 GiB) y los Qwen3
pequeños (0.6B, 1.7B, 4B; Apache-2.0). Un GGUF propio:
`-url https://huggingface.co/<org>/<repo>/resolve/<commit>/<fichero>.gguf -sha256
<hash>` (solo Hugging Face y solo por commit, por la misma razón).

| Flag | Por defecto | Qué hace |
|---|---|---|
| `-ctx` | 2048 | contexto total de `llama-server`. La caché KV se reserva entera al arrancar y va en cada réplica y en el dorado: SmolLM2-360M, ~40 KiB/token (80 MiB a 2048); Qwen2.5-0.5B, ~12 KiB/token |
| `-parallel` | 1 | peticiones simultáneas por réplica (el contexto se reparte). Escalar con réplicas sale mejor: cada una cuesta poco más que su KV y no se pisan la CPU |
| `-threads` | nº de vCPU | hilos de cálculo |
| `-cpus`, `-mem` | los del catálogo | tamaño de la microVM; con un GGUF propio, 2 y 1024 |
| `-cpu-pct` | 100 × vCPU | techo de CPU que graba el dorado y heredan las réplicas |
| `-build-only` | — | solo la imagen (para copiarla a un Mac) |
| `-accept-license` | — | construir un modelo del catálogo que no es de licencia abierta (su identificador) |
| `-wait` | 5m | cuánto esperar a que el modelo cargue, y a que conteste el calentamiento (que se reintenta si el proxy del daemon se cansa antes) |
| `-rebuild`, `-replace` | — | rehacer la imagen / pisar el dorado |

## Cómo está hecho

**La imagen** es una capa sobre la base `glibc-trixie`. llama.cpp es la versión
`b11147` en los **binarios oficiales** (`llama-b11147-bin-ubuntu-{arm64,x64}`,
sha256 de GitHub fijado en `pkg/von`), no una compilación propia: traen
`GGML_CPU_ALL_VARIANTS`, una biblioteca por nivel de instrucciones (armv8.0,
8.2+dotprod, 8.6+i8mm, 9.2+SVE2; SSE4.2…AVX-512 en x86) que se elige al arrancar
según la CPU, que es exactamente la base portable que habría que compilar a mano,
y compilarlas en el constructor pasaba del plazo de 15 minutos. Exigen glibc 2.38:
por eso trixie (2.41) y no la bookworm (2.36) de `71-build-glibc-base.sh`, que el
constructor llama con `SUITE=trixie`. De la tarball solo entran `llama-server`, sus
bibliotecas y las variantes de CPU (~30 MiB).

Dentro: `/models/<fichero>.gguf`, `/opt/llama.cpp/`, `/etc/von/run.sh` (los
argumentos de `llama-server`, generados en `pkg/von`) y `/etc/von/model.json` (qué
modelo, hash, contexto, versión de llama.cpp). El entrypoint arranca `run.sh` en
segundo plano, lo relanza si muere, y cede el PID 1 a `kling-guest`, así que `kling
exec`/`cp` siguen funcionando (`-allow-exec` por defecto en los modelos). El log
del servidor: `kling exec <réplica> -- tail /var/log/service.log`.

**Argumentos de `llama-server` que importan**: `--load-mode dio` (abajo),
`--cache-ram 0` (la caché de prompts en RAM viene a 8 GiB por defecto: en una
microVM de 768 MiB es el OOM killer esperando), `--fit off` (que no reajuste nada
según la memoria libre), `--no-webui`, `--metrics`, `--alias <modelo:quant>`.

**El dorado** (`von.MakeGolden`): arranca sin red (`egress none`), espera a
`/health` 200, calienta con una respuesta de 8 tokens a temperatura 0 —lo que la
pasada vacía del arranque no toca: páginas de pesos, hilos de OpenMP, búferes del
tamaño de un lote real—, aprieta el globo (`squeeze`) para que lo libre acabe en
huecos del `mem.file`, congela y borra la plantilla. Etiquetas: `von.model`
(`smollm2-360m-instruct:q8_0`), `kling.ports=8000` y `service`. Un gateway
descubre los modelos listando snapshots con `von.model`.

### Dos cosas que se aprendieron midiendo

- **El techo de CPU por defecto estrangula un modelo.** El daemon da a cada microVM
  el 50 % de un core (pensado para herramientas MCP que esperan casi siempre). Un
  modelo de 2 vCPU queda a una cuarta parte. El dorado se hace ahora con 100 % por
  vCPU, y `kling run -from` **hereda el `cpu_pct` del snapshot** (antes solo lo
  heredaba quien lo pasaba a mano, como el planificador: una réplica lanzada con
  `kling run -from` caía al 50 %).
- **`mmap` + reempaquetado = dos copias.** Por defecto llama.cpp mapea el GGUF y,
  en ARM, *reempaqueta* los pesos Q8_0/Q4 a un formato para la CPU en memoria
  anónima: la caché de páginas guarda el fichero y la memoria anónima la copia
  buena. Medido con SmolLM2 en 768 MiB: 503 MiB anónimos, 197 MiB de caché y
  32 MiB libres. Con `--load-mode dio` (O_DIRECT) la única copia es la
  reempaquetada y el dorado no lleva un duplicado del GGUF: su `mem.file` bajó de
  648 a 527 MiB.

## Semillas en réplicas del mismo dorado

Cien réplicas nacen del mismo volcado de memoria: si el estado del generador
aleatorio viviera en él, todas muestrearían lo mismo. No pasa, por dos razones:

1. `llama-server` crea el muestreador **en cada petición**. Con la semilla por
   defecto (`-1`) la pide nueva: `std::random_device` si es una fuente hardware,
   y si no (libstdc++ en aarch64 lee `getrandom`/`arc4random` y declara
   `entropy() == 0`) el reloj de pared en nanosegundos (`get_rng_seed` en
   `src/llama-sampler.cpp` de `b11147`). No queda estado de un muestreo en el
   snapshot.
2. Tras cada restauración el daemon llama a `/resync` del agente (v0.9.1): pone la
   hora del host y resiembra el CRNG del kernel. Reloj y entropía son de la réplica.

Medido con `96-von-bench.sh` (misma petición, temperatura 1, dos réplicas del
mismo dorado): *"Uranus."* / *"Neptune's Helmet."* en Linux, *"Planet XYZ."* /
*"Planet Lumin."* en macOS; en los cuatro dorados, salidas distintas. Quien
necesite garantías sin depender del invitado (el gateway) debe mandar `seed`
explícito por petición desde `crypto/rand` del host; quien quiera reproducibilidad,
una semilla fija (la API la acepta).

## Dimensionado

| Modelo | Memoria de la VM | Usada en el dorado (Linux / macOS) | Nota |
|---|---|---|---|
| SmolLM2-360M Q8_0 | 768 MiB | 527 / 456 MiB | pesos reempaquetados 369 MiB + KV 80 MiB + cálculo |
| Qwen2.5-0.5B Q4_K_M | 896 MiB | 602 MiB / 528 MiB | KV pequeña (2 cabezas KV); búfer de cálculo grande (vocabulario de 152k) |
| Qwen2.5-0.5B Q8_0 | 1152 MiB | 871 MiB / 800 MiB | el más justo: a 1024 medía 871 MiB (153 libres para la caché KV y los búferes del vocabulario de 152k si se sube `-ctx`); memoria por defecto subida a 1152 |
| Qwen2.5-1.5B Q4_K_M | 1536 MiB | 1286 MiB / 1184 MiB | 4 vCPU; KV ~28 KiB/token (56 MiB a 2048) |
| Qwen2.5-1.5B Q8_0 | 2304 MiB | — / 2048 MiB | en el laboratorio anidado no terminó de cargar en 2 h (abajo) |
| Qwen2.5-3B Q4_K_M | 2816 MiB | — / 2176 MiB | KV ~36 KiB/token; licencia no abierta (arriba) |

En Linux la memoria de la VM que no se toca no cuesta: el `mem.file` es disperso y
lo no escrito son huecos. En macOS sí: `vz` restaura copiando la memoria del
invitado, así que cada réplica paga su tamaño entero (ver abajo). Ahí conviene
ajustar `-mem`.

## Benchmarks

Método: `scripts/96-von-bench.sh <dorado>` contra cada daemon, con el CLI en la
misma máquina (socket local). Los tiempos incluyen lanzar el CLI (~10 ms) y el
proxy del daemon. "Primer token" es una respuesta de `max_tokens: 1` a *"Hi"*, que
es evaluar el prompt con su plantilla de chat (~30 tokens) más un token. Tokens/s:
los `timings` de `llama-server` (sin red ni proxy) con un prompt de ~62 tokens y 128
generados a temperatura 0, en una réplica que ya contestó una vez, mediana de 3.
Memoria: `kling top -json` (PSS de Firecracker en Linux; `phys_footprint` del
ayudante y del proceso de Apple en macOS) tras una petición en cada réplica; en
Linux también la suma de RSS, que cuenta las páginas compartidas en cada réplica.
Medianas con (mín–máx, n).

Equipos:

- **Linux**: la VM Lima `kling-arm` (Ubuntu 24.04, arm64, 6 vCPU, 8 GiB) en un Mac
  mini M4, con **Firecracker 1.16.1 bajo KVM anidado** (macOS → Linux → microVM).
- **macOS**: el mismo Mac M4 (10 núcleos, 16 GiB), backend `vz`, sin anidar: el
  invitado corre sobre el hipervisor de Apple directamente.

### Linux (Firecracker, KVM anidado)

| | SmolLM2-360M Q8_0 | Qwen2.5-0.5B Q4_K_M | Qwen2.5-0.5B Q8_0 |
|---|---|---|---|
| `mem.file` del dorado | 527 MiB | 602 MiB | 871 MiB |
| capa de la imagen en disco | 402 MiB | 502 MiB | 678 MiB |
| crear el dorado (`models add`, imagen en caché) | 5 min 36 s (carga 3 min) | 6 min 6 s (carga 4 min 18 s) | 12 min 49 s (carga 8 min 37 s) |
| arranque en frío → primer token | 4 min 3 s (`COLD=1`: ~192 s arranque+carga, ~51 s la primera petición) | — | — |
| thaw (`thaw_ms` del daemon) | 223 ms (213–358) | 267 ms (249–529) | 254 ms (245–278, n=3) |
| `run -from` → primer token | 4,3 s (4,2–5,5) | 4,0 s (3,9–5,5) | 5,0 s (4,7–5,9, n=3) |
| prompt, tok/s | 12 (10–44) | 29 (22–38) | 22 (21–51, n=3) |
| generación, tok/s | 8,8 (0,8–9) | 13,3 (4,5–13,5) | 13 (3,3–14,1, n=3) |

Referencia en el mismo host Linux, **sin microVM** (`llama-server` de la misma
versión, mismos argumentos, 2 hilos, el GGUF en la caché de páginas): listo en
0,6–1,2 s, primer token en **0,66–1,24 s**, **534 MiB de RSS por proceso**;
`llama-bench` da 731 tok/s de prompt y **151 tok/s** de generación.

**Qué domina el arranque en frío**: de los 4 min 3 s (`COLD=1`, SmolLM2 Q8_0), ~192 s son arrancar la microVM y cargar el GGUF —leerlo entero por `O_DIRECT` (no hay caché de páginas de por medio) y reempaquetar los pesos a instrucciones i8mm, una transformación de CPU, no de E/S— hasta que `/health` responde 200; los ~51 s restantes son la primera petición en sí, muy por encima de los ~4 s de una réplica recién restaurada de un dorado (tabla de arriba). La diferencia es el calentamiento: el dorado se congela **después** de una respuesta de prueba (`MakeGolden`), así que el hilo de OpenMP y el grafo de cómputo de `llama-server` ya están creados en el volcado; un arranque en frío desde la imagen los crea de cero en su primera petición. Las dos cifras están además infladas por el anidamiento (15–20× más lento, sección siguiente); en hierro debería dominar solo la carga del GGUF.

**Memoria de N réplicas del mismo dorado** (SmolLM2-360M Q8_0):

| réplicas | PSS total | PSS de cada una | suma de RSS |
|---|---|---|---|
| 1 | 427 MiB | 427 | 426 MiB |
| 2 | 436 MiB | 218, 218 | 853 MiB |
| 3 | 450 MiB | 151, 148, 151 | 1285 MiB |
| 4 | **461 MiB** | 116, 113, 116, 116 | 1715 MiB |

La diferencia entre las dos columnas es lo compartido: los pesos viven **una vez**
en la caché de páginas del host (el `mem.file` del dorado) y cada réplica añade
~11–14 MiB. Cuatro `llama-server` sueltos serían 4 × 534 = 2136 MiB.

**Qwen2.5-1.5B Q4_K_M** (4 vCPU, 1536 MiB) en el mismo laboratorio: el dorado
tardó 37 min en crearse (21 min de carga del GGUF y tres reintentos del
calentamiento, cuya primera respuesta pasó de los 5 min del proxy del daemon);
`mem.file` de 1286 MiB. Thaw en **360 ms** (241–361, n=3), pero el primer token
llega a los **29 s** (27–33 s): la primera petición trae ~1,2 GiB de pesos a
base de fallos de página de ~1 ms. Una generación de 128 tokens no terminó en
5 min (la velocidad del anidado, abajo). La memoria sí vale, y se comparte igual:

| réplicas | PSS total | PSS de cada una | suma de RSS |
|---|---|---|---|
| 1 | 1011 MiB | 1011 | 1010 MiB |
| 2 | 1030 MiB | 514, 516 | 2023 MiB |
| 3 | **1049 MiB** | 351, 348, 350 | 3036 MiB |

~19 MiB por réplica de más. El Q8_0 no terminó de cargar en 2 h en el
laboratorio y el 3B cargó en 41 min en un intento y no acabó en 80 en otro: con
KVM anidado, un modelo de 2 GiB está en el límite de lo medible. Sus cifras son
las del Mac.

### macOS (`vz`, sin anidar)

| | SmolLM2-360M Q8_0 | Qwen2.5-0.5B Q4_K_M | Qwen2.5-0.5B Q8_0 |
|---|---|---|---|
| `mem.file` del dorado | 456 MiB | 528 MiB | 800 MiB |
| crear el dorado (imagen copiada) | 3 s (carga 1,2 s) | 4 s (carga 1,2 s) | 4 s (carga 1,4 s) |
| arranque en frío → primer token | 1,43 s | 1,52 s | 1,51 s |
| thaw (`thaw_ms`) | 673 ms (597–1515) | 675 ms (657–1434) | 946 ms (926–2348) |
| `run -from` → primer token | **815 ms** (749–1689) | **868 ms** (846–1851) | **1125 ms** (1080–2793) |
| de ello, la petición de 1 token | 105 ms | 136 ms | 126 ms |
| prompt (62 tokens), tok/s | 669 (643–705) | 178 (167–198) | 684 (678–691) |
| generación (128 tokens), tok/s | **139** (133–144) | 87 (82–91) | **112** (108–114) |
| memoria por réplica (`phys_footprint`) | 1298 MiB | 1435 MiB | 1691 MiB |
| 1 → N réplicas | 1298 → 5155 MiB (4) | 1435 → 4309 MiB (3) | 1691 → 5075 MiB (3) |

Los de 1,5B y 3B (4 vCPU; medidos con la VM de Lima —8 GiB— cargando modelos al
lado, así que los tiempos de thaw son pesimistas):

| | Qwen2.5-1.5B Q4_K_M | Qwen2.5-1.5B Q8_0 | Qwen2.5-3B Q4_K_M |
|---|---|---|---|
| `mem.file` del dorado | 1184 MiB | 2048 MiB | 2176 MiB |
| crear el dorado (imagen copiada) | 6 s | 20 s (carga 3,6 s) | 10 s (carga 4,4 s) |
| arranque en frío → primer token | 2,67 s | 4,45 s | 4,00 s |
| thaw (`thaw_ms`) | 1688 ms (1389–4591) | 8491 ms (7515–8962) | 2334 ms (2265–8015) |
| `run -from` → primer token | **1,97 s** (1,67–5,25) | 8,96 s (8,05–9,79) | **2,66 s** (2,50–8,62) |
| de ello, la petición de 1 token | 267 ms | 434 ms | 291 ms |
| prompt (61 tokens), tok/s | 177 (171–180) | 291 (279–316) | 91 (85–96) |
| generación (128 tokens), tok/s | **60** (60–62) | 46 (43–48) | 34 (33–34) |
| memoria por réplica (`phys_footprint`) | 2640 MiB | 4025 MiB | 4833 MiB |
| 1 → N réplicas | 2640 → 5196 MiB (2) | 4025 → 7960 MiB (2) | una (cabe una al lado de Lima) |

- A partir de 1,5B, **en el Mac el dorado casi no gana al arranque en frío**, y el
  Q8_0 pierde: restaurar copia la memoria entera (2 GiB en 8,5 s con el Mac
  apretado de memoria) y cargar el GGUF desde la caché de páginas cuesta 3–4 s.
  El dorado sigue ganando en Linux, donde la memoria se mapea perezosamente.
- Q4_K_M genera más rápido que Q8_0 a este tamaño (60 frente a 46 tok/s: la
  generación la limita el ancho de banda de memoria), pero evalúa el prompt más
  despacio (177 frente a 291): para respuestas cortas con prompt largo (una
  clasificación con ejemplos) gana Q8_0; para generar, Q4_K_M.
- Cada réplica pesa en el Mac bastante más que su VM (el proceso de
  Virtualization.framework y el ayudante): 2,6 GiB una de 1,5 GiB.

Lecturas:

- **En el Mac el dorado gana poco al primer token** (0,8 s frente a 1,4 s en frío):
  `vz` restaura copiando toda la memoria del invitado (el `thaw` crece con el
  tamaño: 673 ms con 768 MiB, 946 ms con 1024), y un arranque en frío de estos
  modelos ya es rápido sin anidar (cargar 369 MiB con O_DIRECT, ~1,2 s).
- **En el Mac no hay compartición**: la memoria de N réplicas es N veces la de una
  (y cada una pesa más que su VM: el ayudante y el proceso de Apple). Densidad
  ahí = memoria de la VM justa.
- **Q8_0 es más rápido que Q4_K_M** en esta CPU (en prompt, 4×): los pesos Q8_0
  se reempaquetan para las instrucciones i8mm y Q4_K_M no. Q4_K_M solo compensa si
  falta memoria. Por eso Q8_0 es el defecto.
- El primer valor alto de cada serie de thaw (1,4–2,8 s) es la primera
  restauración tras crear el dorado: el `mem.file` aún no está en la caché de
  páginas del Mac.

### El laboratorio anidado: por qué sus tiempos no cuentan

Los tiempos de cómputo del laboratorio Linux son de la virtualización anidada, no
de kindling ni de llama.cpp. Pruebas sobre el mismo host Linux (L1) y en una
microVM suya (L2) con la misma imagen:

| prueba | host Linux (L1) | microVM (L2) |
|---|---|---|
| bucle de `awk` (20 M iteraciones, sin memoria) | 0,47 s | 4,3 s (9×) |
| leer 300 MB de tmpfs ya escritos (3 pasadas) | — | 76 s, 76 s, 0,04 s |
| `llama-bench` generación, 2 hilos | 151 tok/s | 8,8 tok/s en una réplica restaurada; <0,1 tok/s en una arrancada en frío |

La lectura de tmpfs apunta a la causa: 300 MB en 76 s son ~1000 páginas de 4 KiB
por segundo, **del orden de 1 ms por página** tocada que el hipervisor intermedio
no tiene mapeada. Lo más probable es la tabla de páginas de segunda etapa, que en
la anidación se mantiene por software; y el modelo recorre sus ~370 MiB de pesos
en cada token. Una réplica restaurada va
mejor que una arrancada en frío (su memoria es el `mem.file`, que el host ya tiene
en caché), pero sigue lejos del hierro. Esto es del entorno (Apple Silicon M4 →
Linux → Firecracker): en un host Linux con KVM nativo el invitado corre a velocidad
de CPU, y el mismo `llama-server` da ~150 tok/s en este mismo core fuera de la
microVM. **Las cifras de memoria del laboratorio sí valen**: la compartición de
páginas no depende de la velocidad.

Pendiente: repetir la tabla de Linux en hierro (x86 con KVM nativo) para tener el
primer token de un dorado sin anidar; ahí la tesis predice ~0,2 s de thaw más la
primera petición a velocidad de CPU.

## GPU (diseño, sin implementar)

Firecracker no tiene *passthrough* de dispositivos PCI, y Virtualization.framework
no da la GPU del Mac a un invitado Linux (solo una GPU paravirtual para gráficos,
sin Metal ni cómputo). Un modelo en GPU no puede vivir en una microVM de kindling
tal como es hoy. Dos caminos:

**1. "Runner de host" (el recomendado para empezar).** `llama-server` con Metal en
el Mac o con CUDA en un servidor NVIDIA, **fuera** de una microVM, supervisado por
el daemon como un proceso más: misma API (OpenAI en un puerto de loopback), mismas
etiquetas (`von.model`, `kling.ports`), `kling ps` lo enseña, y el gateway lo trata
como una réplica. Escala a cero parando el proceso; "despertar" es arrancarlo
(cargar un modelo pequeño en GPU: segundos) porque no hay snapshot de memoria de
GPU que valga.

- A favor: la GPU entera y sin capas; en un M4, Metal genera varias veces más
  rápido que la CPU; sin cambios en el gateway.
- En contra: **sin aislamiento de microVM** (es un proceso del host: el modelo y
  sus dependencias corren con los permisos del daemon, así que solo pesos y
  binarios de confianza, fijados por hash como aquí); sin dorado ni compartición de
  páginas; una GPU se reparte mal entre procesos (memoria de vídeo fija por
  proceso), así que la densidad es "un servidor por GPU con `--parallel`", no N
  réplicas.

**2. cloud-hypervisor + VFIO (la opción aislada, Linux).** cloud-hypervisor sí
pasa dispositivos PCI con VFIO: una GPU NVIDIA entera (o una partición MIG, o una
vGPU con licencia) dentro de una microVM con su driver.

- A favor: el aislamiento de siempre; el invitado es una VM con su kernel.
- En contra: una GPU (o partición) **por VM**, sin compartir; VFIO fija en RAM
  toda la memoria del invitado (adiós memoria perezosa, sobrecompromiso y
  compartición de páginas del dorado); el snapshot de una VM con un dispositivo
  pasado no es restaurable (el estado de la GPU no se vuelca), así que se pierde
  el thaw en milisegundos; y es un segundo VMM que mantener junto a Firecracker y
  `vz`. Para cargas pequeñas como estas sale peor que el runner de host o que la
  CPU; tiene sentido para modelos que no caben en CPU y clientes que exigen
  aislamiento fuerte.

El orden razonable: runner de host detrás del mismo gateway, y VFIO solo si
aparece la necesidad de GPU aislada.

## El gateway

`kling ai serve` ([ai-gateway.md](ai-gateway.md)) sirve estos modelos para
generar (`/v1/generate` con plantilla por tarea, y la API de OpenAI), con
`pkg/scheduler` para despertarlos por petición y congelarlos al quedarse
ociosos. Descubre los dorados por `von.model`, habla con cada réplica en
`api.Machine.Addr(von.Port)` (streaming directo, sin el proxy del daemon) y manda
una semilla propia por petición. Como clasificadores detrás de JEV (la cascada)
no ganaron en la tarea medida, ni con 1,5B ni con 3B: allí están las cifras.

## Límites conocidos

- **Solo CPU** (ver GPU). En hierro, un core moderno da del orden de 100–150 tok/s
  con estos modelos; más vCPU ayudan al prompt más que a la generación.
- **La primera petición tras un thaw paga los fallos de página** de los pesos. En
  el laboratorio anidado son segundos; habrá que medirlo en hierro. Si pesa, el
  remedio es prefaultar el `mem.file` del dorado en la caché del host (o
  `MAP_POPULATE` en la restauración).
- **macOS no comparte memoria entre réplicas** y la restauración copia toda la
  VM: densidad baja, `-mem` ajustado.
- **Construir necesita un host Linux** con `debootstrap` (la base, una vez) y red
  hacia GitHub y Hugging Face.
- `-parallel` > 1 reparte el contexto entre ranuras: con 2048 y 4 ranuras, 512
  tokens por conversación.
- El log de `llama-server` (`/var/log/service.log`) crece sin rotar en el disco de
  la réplica; en réplicas efímeras no importa.
