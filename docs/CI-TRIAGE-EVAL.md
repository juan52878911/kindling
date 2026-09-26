# Triaje de fallos de CI — evaluación (v0.12.0)

Evaluación del segundo ejemplo de kindling, [`examples/ci-triage`](../examples/ci-triage/README.md):
dado el log de un CI que falló, **Chispa** puntúa cada línea (¿explica el fallo?),
elige el trozo y le pone categoría; **VON** (un LLM pequeño en una microVM que
se congela al quedarse ocioso) solo entra cuando Chispa duda y solo lee el
trozo. Todo por el gateway de IA (`kling ai serve`), medido en un Mac mini M4
(backend `vz`).

Resumen:

- **El localizador funciona en su dominio**: en 160 logs de Travis de 16
  repositorios que no vio al entrenar, el trozo de Chispa toca las líneas
  anotadas a mano en el **70 %** de los logs, frente al 54 % de «las últimas 30
  líneas» y el 38 % de una expresión regular de errores, con el mismo tamaño
  de trozo (~400 tokens en vez de las ~2 800 líneas medias de un log). La línea
  que Chispa pone primera es del trozo en el 49 % de los logs; las líneas base,
  en el 0-2 %.
- **Barato**: ~1 µs por línea de Chispa y ~5 µs de preparar la línea (un
  núcleo: ~175 000 líneas/s); por el gateway en proceso, 76 000-80 000
  líneas/s y **25 ms de mediana por log** (p99 0,72 s en logs de 16 000-32 000
  líneas).
- **La categoría es difícil y Chispa no se fía de sí mismo**: 0,52 de
  exactitud de punta a punta sobre categorías revisadas a mano, sin contestar
  nunca con confianza en prueba (sus umbrales se calibraron sobre etiquetas de
  reglas). VON con Qwen2.5-1.5B llega a **0,56** a ~3,7 s por log (no
  significativo: McNemar p = 0,21); con Qwen2.5-0.5B **empeora** a 0,43
  (p = 0,001).
- **Fuera de dominio no gana a la cola del log**: en 34 fallos reales de GitHub
  Actions de un producto privado, Chispa (entrenado solo con Travis) localiza
  el 56 % de los fallos, igual que «las últimas 30 líneas» (56 %) y apenas por
  encima de la expresión regular (53 %); acierta la categoría en el 35 %
  (VON 1,5B: 41 %). Es la cifra honesta de lo que pasa al cambiar de CI: hace
  falta reentrenar con logs propios, y para eso está la confirmación.
- **Chispa en microVM**: 7 600 líneas/s frente a 58 000 en proceso con los
  mismos 4 logs (~7,6×), 82 ms de mediana por log frente a 18 ms, y a este
  volumen agota los puertos efímeros del Mac (una conexión TCP nueva por
  decisión). Para decisiones por línea, en proceso.

## Datos

### LogChunks (público)

[LogChunks](https://doi.org/10.5281/zenodo.3632351) (Brandt, Panichella y
Beller, MSR 2020): 797 logs de Travis CI de 80 repositorios de 29 lenguajes,
cada uno con el trozo que explica el fallo anotado a mano y validado con sus
desarrolladores. Licencia **CC BY 4.0**, comprobada en la ficha de Zenodo
(versión 1.0.0, la única); `build-data.sh` descarga `LogChunks.zip` (24 108 826
bytes, md5 `aafa4507…` como dice Zenodo) y se para si el sha256 no es
`fa2e3d10fc700cfe06b92b286741666a6389b46548356da9f0679c0d89be7fa8`. El
repositorio no redistribuye nada del conjunto salvo las categorías revisadas a
mano del conjunto de prueba (`examples/ci-triage/data/test-categories.tsv`,
derivadas, misma licencia); la atribución está en [NOTICE](../NOTICE).

**Oro por línea.** El trozo del XML no es texto del log tal cual: perdió los
`<`, los colores quedaron como `[31m` sueltos y a veces está recortado. El
lector (`logchunks.Gold`) normaliza ambos lados (sin ANSI ni `<>`, espacios
colapsados) y busca cada aparición del trozo: sus líneas en orden, cada una
contenida en una línea del log, tolerando hasta 3 líneas intercaladas y que
falte un 30 % de las del trozo. Casan **787 de 797** logs (54 con más de una
aparición: el mismo error repetido, y todas cuentan como oro); los 10 restantes
son secuencias de terminal que el XML estropeó y quedan sin oro por línea
(fuera del entrenamiento del localizador; en prueba no hay ninguno). Mediana
del oro: 7 líneas (p90 90); la última línea del oro está a 35 líneas del final
en la mediana, pero a más de 1 500 en el 10 % de los logs.

**Reparto por repositorio** (semilla `ci-triage-1`, 60/20/20 de los repos):
nunca un repo en dos conjuntos, porque los logs de un mismo repo se parecen
tanto que el localizador parecería mejor de lo que es.

| Conjunto | Logs | Repos | Líneas del localizador |
|---|---|---|---|
| train | 477 | 48 | 183 211 (7 893 `explains`) |
| valid | 160 | 16 | 42 445 (1 808 `explains`) |
| test | 160 | 16 | se evalúa sobre los logs enteros: 449 497 líneas |

En entrenamiento y validación no entran todas las líneas: todas las del oro
(como mucho 60 por log, para que un diff de 1 600 líneas no pese más que cien
logs), todos los negativos difíciles (a ±10 líneas del oro, en rojo o con
marcas de error) y como mucho 200 negativos al azar por log, elegidos por
hash (estables entre máquinas). Sin submuestreo serían 1,3 M de líneas con un
0,5 % de positivos.

### Categorías

LogChunks no trae el tipo de fallo (su «categoría» es la forma del trozo en el
log). Nueve categorías: `test`, `build`, `lint`, `dependency`, `config`,
`infra`, `timeout`, `resources`, `other`; `flaky` solo la puede poner una
persona (hace falta saber que el mismo commit pasó al repetirlo).

- **Entrenamiento y validación**: etiquetas débiles de reglas
  (`triage/rules.go`, expresiones regulares en orden, escritas mirando solo los
  trozos de entrenamiento).
- **Prueba**: las 160 categorías **revisadas a mano** una a una sobre el trozo
  de oro (lo hizo el agente que escribió el ejemplo; `test-categories.tsv`).
  Las reglas coinciden con la revisión en el **54 %**: las etiquetas de
  entrenamiento son ruidosas, y esa es la cota de lo que Chispa puede aprender
  de ellas. Distribución de prueba: lint 47, test 44, build 21, dependency 18,
  config 17, infra 13 (muchos «lint» son comprobadores de enlaces rotos de
  listas `awesome-*`).

### Logs privados (fuera de dominio)

Los fallos de GitHub Actions de un producto real (35 de las últimas 200
ejecuciones; 34 con log recuperable), con `gh run view --log-failed`: otro CI,
otro formato (trabajo, paso y marca de tiempo por línea, `##[group]`,
`##[error]`), otro idioma en los mensajes propios (español) y solo el paso que
falló. Se anotaron a mano las líneas que explican cada fallo y su categoría
(test 13, config 10, other 8, lint 2, infra 1). **Los logs y sus anotaciones
no salen de la máquina donde se evaluó**: aquí solo van cifras agregadas.

## Métodos

- **Chispa, localizador** (`ci-lines`): binario `explains`/`noise`, 2^18
  cubos, palabras + bigramas + campos, pesos `balanced`, lo de por defecto de
  `kling ai chispa train` (512 KB). Campos por línea (`triage/features.go`):
  marcas (error, fallo, excepción, aserción, tiempo, recursos, «no
  encontrado», salida del proceso, aviso, ok, progreso, resumen) en la línea y
  en sus tres vecinas de cada lado, distancia al error más cercano y a la línea
  en la que el runner da el paso por fallado, distancia al final y decil de
  posición, rojo/amarillo, marco de pila, sangría, sección (`travis_fold`, paso
  o `##[group]`), repeticiones, longitud, palabras y si es sobre todo símbolos.
  El trozo: desde la mejor línea, crece mientras las vecinas puntúen ≥ 35 % de
  ella con huecos de hasta 3 líneas, 30 líneas como mucho; un segundo tramo si
  hay otra ancla ≥ 80 % de la primera; 2 000 bytes de texto en total (líneas
  recortadas a 240). Elegido con un barrido sobre **validación** (`eval
  -sweep`: dos barridos, 162 combinaciones; la elegida da 0,80 localizado en validación).
- **Chispa, categoría** (`ci-category`): multiclase sobre el texto del trozo y
  dos campos, la sección y las palabras del comando que falló (`The command
  "npm test" exited with 1` o el nombre del paso de GitHub). Objetivo de
  precisión 0,9 con `min-support 5` (con el 0,95 por defecto ninguna clase
  llegaba a tener umbral en validación).
- **VON** (`ci-summary`): generación con `json_schema`
  `{category, summary, next_step}`, temperatura 0, 200 tokens como mucho; el
  prompt da las nueve categorías con una línea cada una y la duda de Chispa
  (sus tres candidatos con probabilidad) como pista. Qwen2.5-0.5B Q8_0 y
  Qwen2.5-1.5B Q4_K_M (Apache-2.0), `backend vz`, sin prefijo precalculado.
- **Líneas base del localizador**: las últimas 10 o 30 líneas no vacías, y una
  expresión regular (`error|fail|exception|traceback|fatal|panic|assert|timed
  out|killed|denied|not found|cannot|could not|unable to|✗|❌…`, la más tardía
  primero), con el mismo algoritmo de trozo.
- **Línea base de la categoría**: las mismas reglas del etiquetado débil,
  aplicadas al trozo.

Métricas del localizador, por log y promediadas: *hit@k* (alguna de las k
líneas mejor puntuadas es del oro), *P@k* y *R@k* (precisión y cobertura del
oro en las k primeras), **localizado** (el trozo elegido toca el oro: lo que
necesita una persona o VON), *cubre ≥ 50 %* (el trozo contiene al menos la
mitad del oro), precisión/cobertura/F1 del trozo en líneas.

## Resultados

### Localizador (prueba, 160 logs, 16 repos no vistos)

| Método | hit@1 | hit@5 | hit@10 | P@5 | R@10 | R@50 | **Localizado** | Cubre ≥ 50 % | F1 del trozo | Líneas | ~Tokens |
|---|---|---|---|---|---|---|---|---|---|---|---|
| **Chispa** | **0,487** | **0,669** | **0,713** | **0,423** | **0,316** | **0,566** | **0,700** | **0,575** | **0,335** | 38 | 414 |
| Últimas 10 líneas | 0,000 | 0,263 | 0,356 | 0,089 | 0,222 | 0,222 | 0,356 | 0,219 | 0,123 | 13 | 147 |
| Últimas 30 líneas | 0,000 | 0,263 | 0,356 | 0,089 | 0,222 | 0,430 | 0,544 | 0,394 | 0,185 | 29 | 359 |
| Regex de errores | 0,019 | 0,375 | 0,594 | 0,129 | 0,173 | 0,335 | 0,381 | 0,225 | 0,150 | 11 | 147 |

En validación (16 repos distintos) Chispa localiza 0,80 y «últimas 30 líneas»
0,54: la diferencia entre conjuntos es la varianza de tener 16 repositorios
por lado (cada repo aporta ~10 logs casi iguales).

**Dónde falla** (`eval -misses`): cuando lo que falla no parece un error. Un
`gulp && git status | grep 'working directory clean' || echo 'Please commit…'`
(el fallo es que el build cambió ficheros), un despliegue de Firebase que se
corta sin mensaje, o un informe final ruidoso («Your 10 Slowest Tests»,
`cc-test-reporter … previous exit code of 1`) que puntúa más que el
`Failure:` de Minitest justo encima. Y en un log de 16 000 líneas con el
error hacia la línea 1 300, gana una pila de JavaScript que el runner imprime
mucho después.

### Categoría (prueba, 160 logs, categorías revisadas a mano)

| Método | Exactitud | Macro-F1 | Contesta confiado | Precisión confiada |
|---|---|---|---|---|
| **Chispa**, trozo localizado (de punta a punta) | 0,519 | 0,375 | 0 % | — |
| Reglas, trozo localizado | 0,469 | 0,472 | — | — |
| Chispa, trozo de oro | 0,544 | 0,411 | 0 % | — |
| Reglas, trozo de oro | 0,544 | 0,535 | — | — |
| VON Qwen2.5-0.5B Q8_0 sobre el trozo localizado | 0,431 | 0,328 | — | — |
| **VON Qwen2.5-1.5B Q4_K_M** sobre el trozo localizado | **0,556** | **0,433** | — | — |

- Chispa **no contesta nunca con confianza** en prueba: en validación (reglas)
  lo hacía en el 6 % con precisión 1,0. Es el mismo aviso de
  [CHISPA-EVAL.md](CHISPA-EVAL.md): un umbral promete precisión sobre datos
  como los de validación, y aquí cambian los repos **y** el etiquetador
  (reglas frente a una persona). Sin confianza, la cascada entera es
  «VON contesta todo»: el 100 % de los logs escala.
- **VON 1,5B frente a Chispa** donde discrepan: 11 logs los acierta solo VON,
  5 solo Chispa (McNemar exacta, dos colas, p = 0,21): mejor, pero sin
  demostrar. Con **0,5B**, 2 frente a 16 (p = 0,001): el LLM pequeño contesta
  `test` a casi todo (32 lint, 16 dependency, 15 build y 13 infra acabaron en
  `test`). El 1,5B se equivoca sobre todo mandando lint y dependencias a
  `build`.
- Las reglas sobre el trozo de oro tienen el mejor macro-F1 (0,535): aciertan
  bien las clases raras (infra, dependency) que Chispa, con pocas decenas de
  ejemplos ruidosos de entrenamiento, no aprende. Con el trozo localizado bajan
  a 0,47 de exactitud: cuando el trozo es otro, la regla también.
- Un caso típico del 1,5B (en el README del ejemplo): acierta `build` donde
  Chispa dudaba entre `test` y `build`, pero su resumen atribuye el fallo a un
  SDK que falta cuando es un tipo no encontrado. La categoría se valida contra
  la lista; el resumen es una pista, no un veredicto.

### Fuera de dominio: GitHub Actions de un producto real (34 fallos privados)

| | Chispa | Últimas 30 líneas | Regex |
|---|---|---|---|
| Localizado | **0,559** | 0,559 | 0,529 |
| hit@1 | 0,382 | 0,000 | 0,000 |
| Cubre ≥ 50 % del oro | 0,529 | 0,529 | 0,500 |

| Categoría | Exactitud |
|---|---|
| Chispa (de punta a punta) | 0,353 (nunca confiado) |
| Reglas | 0,176 |
| VON Qwen2.5-1.5B | 0,412 |

Lectura honesta: `gh run view --log-failed` ya trae solo el paso que falló y
termina en el error, así que la cola del log es una línea base mucho más dura
que en Travis; el localizador de Travis la iguala pero no la supera. La
categoría baja más: 8 de los 34 fallos son migraciones de base de datos o una
compuerta que espera a otro CI (`other`), 10 son configuración del propio
despliegue, y los mensajes propios están en español. Lo que hace falta para
este dominio es lo que el ejemplo ya guarda: confirmaciones de logs propios
para reentrenar (unas decenas por clase, según [CHISPA-EVAL.md](CHISPA-EVAL.md)).

### Latencia y coste (Mac mini M4, gateway en proceso)

| | |
|---|---|
| Chispa por línea (`kling ai chispa eval`, un núcleo) | 1,0 µs |
| Preparar una línea (`BenchmarkFeatures`) | 4,7 µs |
| Un núcleo, las dos cosas | ~175 000 líneas/s |
| Por el gateway (socket Unix, 8 peticiones en vuelo) | 76 000-80 000 líneas/s |
| Por log (leer + preparar + Chispa en cada línea + trozo) | p50 25 ms · p90 165 ms · p99 0,72 s · máx 0,79 s |
| Categoría del trozo | < 1 ms |
| VON Qwen2.5-0.5B Q8_0, réplica despierta | p50 1,7 s · p90 2,5 s · ~917 tokens por llamada |
| VON Qwen2.5-1.5B Q4_K_M, réplica despierta | p50 3,7 s · p90 5,8 s · ~914 tokens por llamada (≈ 860 de prompt) |
| VON 1,5B, primera llamada (restaurar el dorado, 1,2 GiB) | ~3,3 s más |

**Qué ahorra el localizador**: VON lee ~900 tokens (≈ 400 del trozo + el
prompt de la tarea) en vez de un log de 2 800 líneas de media (~35 000
tokens, que no caben en el contexto de 2 048 de la réplica). En esta tarea
VON sigue costando segundos por log porque Chispa escala el 100 %: el ahorro
de verdad llega cuando la categoría se reentrena con confirmaciones y Chispa
contesta lo fácil. `kling ai prime` (el prompt de la tarea ya evaluado en el
dorado) quitaría la mayor parte de esos 860 tokens de prompt; no se usó
porque la imagen de 1,5B de esta máquina se construyó sin caché de prompts.

### Chispa en proceso frente a microVM

Los dos modelos desplegados con `kling ai chispa deploy` (dorados de 16 MiB,
64 MiB de VM) y `"backend": "microvm"` en el registro; los mismos 4 logs de
prueba (2 245 líneas clasificadas), 8 peticiones en vuelo:

| | En proceso | microVM (`vz`) |
|---|---|---|
| Líneas/s | 58 000 | 7 600 |
| Por log, p50 | 18 ms | 82 ms |
| Primera petición (restaurar del dorado) | — | 0,3-0,5 s por réplica |
| Log de 863 líneas en frío / caliente | 20 ms / 20 ms | 925 ms / 121 ms |

Tres hallazgos de medirlo, para el núcleo:

1. **Una conexión TCP por decisión.** El cliente del gateway hacia las
   réplicas va sin keep-alive: 2 245 decisiones dejaron 2 313 sockets en
   `TIME_WAIT`, y el conjunto de prueba entero (425 000 líneas) agotó los
   puertos efímeros del Mac a los pocos segundos (`connect: can't assign
   requested address`, 503). Por línea, microVM no es viable sin keep-alive o
   lotes en `/v1/classify`.
2. **`max_replicas` no acotó las réplicas de Chispa**: con `max_replicas: 2`
   en `ci-lines`, la ráfaga de 8 peticiones en vuelo levantó 8 réplicas.
3. En macOS `kling ai chispa deploy` se negaba a hacer el dorado de una imagen
   traída de Linux (la única forma de tenerla en el Mac); ahora lo hace con
   `-reuse-image`. Y el constructor `chispa` falló en la VM de Lima de
   pruebas al encoger la capa (`resize2fs -M` dejó un «Resize inode not
   valid»); para medir se construyó sin encoger.

Conclusión, la misma de [chispa-serverless.md](chispa-serverless.md): para
decisiones por línea, Chispa en proceso; microVM solo si el aislamiento por
tarea lo justifica, y no a este volumen.

## Mejora continua: lo que se exporta

Cada confirmación de la página o de `analyze -confirm` añade una línea a
`ci-triage-feedback.jsonl` (0600, tope 64 MiB), `schema:
ci-triage.feedback/v1`:

```json
{"schema":"ci-triage.feedback/v1","text":"<el trozo>","label":"lint","fields":{"cmd":["npm","test"],"sec":"…"},
 "source":"human","by":"ci-triage","agreed":false,"predicted":"build","decided_by":"von",
 "chispa_label":"test","chispa_prob":0.53,"chispa_confident":false,"von_label":"build",
 "chunks":[{"from":829,"to":831,"score":0.86}],"log_sha256":"…","log_lines":913,"time":"…"}
```

- Es un ejemplo de entrenamiento válido tal cual (`kling ai chispa train` usa
  `text`, `label`, `fields` e ignora lo demás).
- Lleva lo que necesita un reentreno con criterio: qué dijo cada capa y con
  qué confianza (para medir a VON como maestro frente a la persona), los
  tramos elegidos (para etiquetar líneas más adelante) y el sha256 del log,
  **nunca el log**.
- `ci-triage export feedback.jsonl` deja una etiqueta por log (gana la última
  confirmación), en el formato `{text, fields, label, by}` de la importación
  de etiquetas humanas de la mejora continua (`kling ai feedback <tarea>
  -import`); `flaky` queda fuera salvo con `-flaky`.

El bucle de reentreno (puerta, sombra, promoción) no es de este ejemplo; es de
la mejora continua.

## Límites

- Una sola semilla de reparto; con 16 repositorios por conjunto, la varianza
  entre repartos es grande (0,70 en prueba frente a 0,80 en validación).
- Las categorías de entrenamiento son reglas (54 % de acuerdo con la revisión
  a mano) y las de prueba las revisó una sola persona (el agente), sin segundo
  anotador.
- El modelo de categoría tiene 8 etiquetas, no 9: `resources` no aparece ni
  una vez en entrenamiento, y `timeout` solo 4 veces. En prueba no hay
  ninguna de las dos.
- El oro de LogChunks es un trozo por log; si el fallo se explica en dos
  sitios, el otro cuenta como ruido.
- Los logs privados son 34 de un solo producto.
- VON no se midió con el prefijo precalculado ni en x86.

## Reproducir

```sh
examples/ci-triage/build-data.sh /tmp/ci-triage          # datos y modelos: los .chispa salen idénticos byte a byte
# registro: examples/ci-triage/ai.json, con los .chispa en ~/.config/kling/ci-triage/
kling ai serve &
go run ./examples/ci-triage eval -data /tmp/ci-triage -set valid -sweep     # cómo se eligió el trozo
go run ./examples/ci-triage eval -data /tmp/ci-triage -set test             # tablas del localizador y la categoría
go run ./examples/ci-triage eval -data /tmp/ci-triage -set test -von escalated -out von.jsonl
go run ./examples/ci-triage eval -manifest mis-logs.jsonl -von escalated    # logs propios anotados (ci-triage lines)
go test ./examples/ci-triage/triage -run X -bench Features
```
