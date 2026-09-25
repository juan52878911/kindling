# Triaje de fallos de CI: Chispa localiza, VON solo lee el trozo

Un CI que falla deja miles de líneas; alguien baja con la rueda hasta dar con
las pocas que explican el fallo y decide de qué tipo es (un test, una
dependencia, la red, el lint…). Este ejemplo lo hace en tres capas sobre el
gateway de IA de kindling (`kling ai serve`), y es un **programa aparte que usa
kindling**, no parte de él: habla con el gateway por HTTP y entrena con
`kling chispa train`.

```
log (miles de líneas) ──> Chispa por línea (µs): ¿explica el fallo?     tarea ci-lines
                            └─ el trozo: 1-2 tramos, ~400 tokens
                          ──> Chispa sobre el trozo (µs): categoría       tarea ci-category
                               seguro → contesta Chispa
                               duda   → VON (LLM pequeño en microVM, json_schema)   tarea ci-summary
                                        {category, summary, next_step}; solo ve el trozo
                          ──> una persona confirma o corrige → ci-triage-feedback.jsonl
                                        (ci-triage export → kling chispa train / kling ai feedback -import)
```

Las cifras (LogChunks, 160 logs de 16 repos que no se vieron al entrenar, y un
producto real con GitHub Actions) están en
[docs/CI-TRIAGE-EVAL.md](../../docs/CI-TRIAGE-EVAL.md).

## Probarlo

```sh
# 1. datos y modelos (descarga LogChunks, 24 MB, CC BY 4.0, fijado por sha256)
examples/ci-triage/build-data.sh /tmp/ci-triage

# 2. el gateway con las tres tareas de ai.json (rutas relativas al registro)
mkdir -p ~/.config/kling/ci-triage
cp /tmp/ci-triage/lines.chispa /tmp/ci-triage/category.chispa ~/.config/kling/ci-triage/
cp examples/ci-triage/ai.json ~/.config/kling/ai.json     # o fusiona sus models y tasks con los tuyos
kling models add von-qwen15-q4 -model qwen2.5-1.5b-instruct -quant q4_k_m   # VON (opcional)
kling ai serve &

# 3. un log
gh run view <id> --log-failed > failed.log      # o cualquier log de Travis / texto plano
go run ./examples/ci-triage analyze failed.log
go run ./examples/ci-triage analyze -confirm ok failed.log        # y guarda la confirmación
go run ./examples/ci-triage serve                                  # http://127.0.0.1:8089/
```

Sin el modelo VON en el registro (o con `-von-when off`) todo funciona igual:
cuando Chispa duda, la categoría sale marcada `chispa-unsure`.

`analyze` imprime el trozo elegido, la categoría, quién decidió (Chispa o VON)
y cuánto tardó cada capa:

```
log: 913 lines (travis), 863 classified by Chispa

lines 829-831 (score 0.86)
lines 846-859 (score 0.95)

  │ WARNING: Could not find 'dotnet', appending /home/travis/.dotnet to PATH.
  │ …
  │ …/TestJsonCommand.cs(39,17): error CS0246: The type or namespace name 'JsonSchema4' could not be found …
  │ Execution of { dotnet $Arguments } by build.psm1: line 368 failed with exit code 1
  │ …

category: build  (decided by von; Chispa unsure, p=0.69; candidates test 0.69, build 0.09, lint 0.08)
summary:   Failed to execute dotnet build due to missing dependency.
next step: Ensure the required dotnet SDK is installed and add it to the PATH.
           (VON ci-von said build: 838 prompt + 51 generated tokens)

latency: read 0.8 ms · features 9.8 ms · Chispa on 863 lines 18.6 ms (4.52 ms inside the gateway) · chunk 0.23 ms · category 0.47 ms · VON 3514 ms · total 3544.1 ms
```

(Un log de PowerShell del conjunto de prueba, en un Mac mini M4 con Qwen2.5-1.5B
despierto. VON acierta la categoría donde Chispa dudaba, pero su resumen se
inventa la causa: el error es un tipo que no se encuentra, no el SDK. Es lo
que cabe esperar de un LLM de 1,5B; por eso la categoría se valida y el
resumen es una pista, no un veredicto.)

## Comandos

| comando | qué hace |
|---|---|
| `analyze [-json] [-confirm ok\|CATEGORÍA] <log\|->` | el triaje de un log; con `-confirm`, añade la confirmación a `-feedback` |
| `serve [-listen 127.0.0.1:8089]` | la página: pegar un log, ver las líneas resaltadas, confirmar o corregir |
| `data -logchunks DIR -out DIR [-labels …]` | los conjuntos de LogChunks, repartidos por repositorio |
| `eval -data DIR [-set test] [-von escalated] [-manifest m.jsonl]` | localizador, categoría y líneas base; `-manifest` para logs propios anotados a mano |
| `lines <log>` | el log tal como lo ve el ejemplo, numerado (para anotar logs propios) |
| `export <feedback.jsonl>` | una etiqueta limpia por log (`{text, fields, label, by}`) |

Flags comunes: `-gateway` (socket del gateway o `http://host:puerto`, con
`-token-file` o `$KLING_AI_TOKEN`), `-lines-task`, `-category-task`,
`-summary-task`, `-von-when escalated|always|off`, `-workers` (peticiones en
vuelo al gateway).

## Las capas

1. **Localizador** (`ci-lines`, Chispa binario `explains`/`noise`). Cada línea
   no vacía lleva su texto y campos baratos: marcas de error/fallo/aserción en
   ella y en sus tres vecinas, distancia al error más cercano y a la línea en
   la que el runner da el paso por fallado, posición y distancia al final, si
   iba en rojo, si es un marco de pila, su sección (`travis_fold`, paso o
   `##[group]` de GitHub Actions), cuántas veces se repite. Un modelo lineal no
   ve contexto por sí solo: esos campos se lo dan. El trozo crece desde la
   mejor línea mientras las vecinas puntúen al menos un 35 % de ella (con
   huecos de hasta 3 líneas), hasta 30 líneas; un segundo tramo si hay otra
   ancla casi tan buena.
2. **Categoría** (`ci-category`, Chispa multiclase sobre el trozo y el comando
   o paso que falló): `test`, `build`, `lint`, `dependency`, `config`,
   `infra`, `timeout`, `resources`, `other`. `flaky` no se puede saber de un
   solo log: solo la pone una persona.
3. **VON** (`ci-summary`, generación con `json_schema`): solo cuando Chispa
   duda, y solo con el trozo y la duda de Chispa como pista. El ejemplo valida
   la respuesta (categoría conocida, textos acotados) antes de usarla; si VON
   no contesta, queda la de Chispa marcada como insegura.
4. **Confirmación**: `ci-triage-feedback.jsonl`, una línea por confirmación
   (`schema: ci-triage.feedback/v1`) con el trozo, la categoría confirmada,
   lo que dijo cada capa y el sha256 del log (nunca el log). `kling chispa
   train` la acepta tal cual; `ci-triage export` deja una etiqueta por log
   para la importación de etiquetas humanas de la mejora continua.

## Seguridad y topes

- Logs acotados: se guarda como mucho la cola de 16 MiB y 100 000 líneas (de un
  fichero se salta directamente a la cola; de stdin se lee hasta 1 GiB con
  memoria acotada), líneas de 2 KiB como mucho, 320 bytes por línea a Chispa y
  2 000 bytes de trozo a VON.
- La página solo escucha en loopback (no tiene usuarios), rechaza un `Host`
  que no sea de loopback (DNS rebinding), exige JSON o su cabecera propia en
  los POST, CSP `default-src 'self'`, todo lo del log y de VON entra con
  `textContent`, un análisis a la vez, cuerpos de 8 MiB como mucho. La
  confirmación se arma con lo que el servidor recuerda del análisis, no con lo
  que mande el navegador.
- El fichero de confirmaciones nace 0600 y tiene tope de 64 MiB.

## Piezas

| fichero | qué |
|---|---|
| `triage/logread.go` | lectura acotada; Travis (`\r`, plegados), GitHub Actions (`gh run view --log-failed`, `##[group]`, `##[error]`), ANSI |
| `triage/features.go` | lo que ve Chispa de cada línea |
| `triage/locate.go` | de puntuaciones por línea a tramos; el texto y los campos de la categoría |
| `triage/pipeline.go`, `gateway.go` | las tres capas por el gateway |
| `triage/rules.go` | categorías, reglas de etiquetado débil (también la línea base) |
| `triage/feedback.go` | la exportación de confirmaciones |
| `logchunks/` | lector de LogChunks: el trozo anotado casado con las líneas del log, reparto por repositorio |
| `data/test-categories.tsv` | la categoría de los 160 logs de prueba, revisada a mano |
| `ai.json` | registro de ejemplo del gateway |
| `build-data.sh` | descarga fijada por sha256, conjuntos y modelos |

LogChunks: C. Brandt, A. Panichella, M. Beller, «LogChunks: A Data Set for
Build Log Analysis», MSR 2020, [10.5281/zenodo.3632351](https://doi.org/10.5281/zenodo.3632351),
CC BY 4.0. El repositorio no lo redistribuye: `build-data.sh` lo descarga.
