# Chispa — evaluación con commits reales (v0.12.0)

Evaluación honesta de [Chispa](chispa.md) sobre un problema real y sin descargas:
adivinar el tipo de un commit (`feat`, `fix`, `docs`, `refactor`, `test`, `chore`,
`perf`, `ci`, `build`, `style`) a partir de su mensaje, usando como etiqueta débil
el prefijo de [Conventional Commits](https://www.conventionalcommits.org/) que el
autor escribió, **quitado del texto** (si no, basta leer las cuatro primeras
letras).

Resumen: Chispa **duplica la exactitud de la clase mayoritaria y supera a unas reglas
de palabras clave** en el reparto temporal (0,64–0,67 frente a 0,55; macro-F1
0,41–0,54 frente a 0,33), en 1,5–5 µs por commit. Pero **la precisión prometida por
los umbrales (0,95 en validación) no se sostiene en prueba** cuando la
distribución cambia: 0,77–0,85 en el reparto temporal y 0,58 entre repositorios.
Para la cascada Chispa → VON esto significa que los umbrales se deben calibrar con
tráfico reciente del mismo sitio, o subir el objetivo (con `-precision 0.99` la
precisión real fue 0,94–0,98, a cambio de contestar solo el 6–12 %).

## Datos

`scripts/chispa-eval.sh OUT REPO...` los reconstruye (los datos no se guardan en el
repositorio). Usa `tools/chispa-commits`: `git log --no-merges --numstat` de cada
repo local, se queda con los commits cuyo asunto empieza por un prefijo
convencional, quita el prefijo y su ámbito, añade hasta 300 bytes del cuerpo sin
trailers (`Signed-off-by`, `Co-authored-by`…) ni líneas que empiecen por otro
prefijo (los squash merges listan los commits que juntan: fuga de etiqueta), y
resume los ficheros tocados como campos: `files` (número), `churn` (líneas),
`ext` y `dir` (hasta 8 extensiones y directorios de primer nivel). Los textos
duplicados se quedan en su primera aparición.

Repositorios bajo `~/Documents/GitHub` y `~/Github` con al menos 50 commits
convencionales:

| Repo | Commits | Convencionales | Tras deduplicar | Idioma |
|---|---|---|---|---|
| bun | 17 678 | 2 345 | 2 344 | inglés |
| OpenWA | 1 738 | 1 715 | 1 715 | inglés |
| repo privado (es) | 876 | 247 | 245 | español |

Los demás (kindling, Portafolio, rustworkx, dobla…) tienen menos de 50 y se
descartan. Total: 4 304 ejemplos.

**Reparto sin fugas**, dos variantes:

- **Temporal, por repo**: en orden de fecha, el tramo más antiguo entrena, el
  siguiente valida (parada temprana, temperatura y umbrales) y el 20 % más
  reciente se evalúa. Se probó 70/10/20 y 60/20/20 (ver más abajo por qué).
- **Entre repos**: OpenWA entero solo en prueba; bun y el repo privado (es)
  entrenan (90 % más antiguo) y validan (10 % más reciente).

Distribución por etiqueta (reparto 60/20/20):

| Conjunto | n | build | chore | ci | docs | feat | fix | perf | refactor | style | test |
|---|---|---|---|---|---|---|---|---|---|---|---|
| train | 2 582 | 8 | 296 | 147 | 346 | 395 | 1 164 | 16 | 99 | 7 | 104 |
| valid | 861 | 12 | 35 | 27 | 105 | 109 | 502 | 18 | 12 | 1 | 40 |
| test | 861 | 82 | 21 | 94 | 112 | 33 | 271 | 13 | 26 | 2 | 207 |
| xrepo-train | 2 329 | 53 | 118 | 183 | 299 | 274 | 1 215 | 35 | 40 | 0 | 112 |
| xrepo-test (OpenWA) | 1 715 | 22 | 231 | 35 | 243 | 259 | 703 | 9 | 95 | 10 | 108 |

Lo primero que dice esta tabla es el problema principal: **la distribución cambia
con el tiempo**. En el tramo más reciente de bun hay muchos más `test`, `build` y
`ci` (207 + 82 + 94 de 861) que en el pasado (104 + 8 + 147 de 2 582). Ningún
modelo entrenado con el pasado lo ve venir.

## Líneas base

- **Clase mayoritaria** (la del entrenamiento: `fix`), siempre «segura».
- **Reglas de palabras clave**: la regla que alguien escribiría en diez minutos
  (`typo|readme|docs…` → docs, `test|flaky…` → test, `fix|bug|crash…` → fix, …, en
  orden; `tools/chispa-commits`). Cuando ninguna casa, predice la mayoritaria y cuenta
  como escalado, para comparar su cobertura con la de Chispa.

## Resultados

Configuración por defecto de `kling chispa train` salvo lo indicado: 2^18 cubos,
palabras + bigramas + campos, AdaGrad `lr` 0,05, L2 1e-4, pesos `balanced`,
objetivo de precisión 0,95, `min-support` 10, semilla 1. Los hiperparámetros se
eligieron mirando **solo validación** (barrido de `lr`, L2, pesos por clase,
n-gramas y campos sobre el reparto 70/10; las diferencias entre `lr` 0,02–0,1
eran ruido).

### Reparto temporal (prueba: 861 commits más recientes)

| Modelo | Exactitud | Macro-F1 | ECE | Contesta (confiado) | Precisión en lo confiado |
|---|---|---|---|---|---|
| Mayoritaria (`fix`) | 0,315 | 0,048 | — | 100 % | 0,315 |
| Reglas de palabras clave | 0,547 | 0,327 | — | 73,9 % (regla disparada) | 0,546 |
| **Chispa** palabras + campos, 70/10/20 | **0,670** | **0,535** | 0,073 | 26,9 % | 0,772 |
| Chispa palabras + campos, 60/20/20 | 0,640 | 0,405 | 0,079 | 23,5 % | **0,851** |
| Chispa solo texto, 60/20/20 | 0,458 | 0,290 | 0,112 | 19,0 % | 0,726 |
| Chispa + n-gramas 3–5, 60/20/20 | 0,627 | 0,402 | 0,055 | 20,4 % | 0,864 |
| Chispa objetivo 0,90, 60/20/20 | 0,640 | 0,405 | 0,079 | 34,7 % | 0,793 |
| Chispa objetivo 0,99, 60/20/20 | 0,640 | 0,405 | 0,079 | 5,6 % | 0,979 |
| Chispa objetivo 0,90, 70/10/20 | 0,670 | 0,535 | 0,073 | 37,5 % | 0,650 |
| Chispa objetivo 0,99, 70/10/20 | 0,670 | 0,535 | 0,073 | 11,6 % | 0,940 |

(En los modelos de la misma tabla con distinto objetivo, exactitud y macro-F1 son
iguales: el objetivo solo mueve los umbrales, no el modelo).

Por qué dos repartos: con 70/10/20 la validación tiene 432 ejemplos y solo la clase
`fix` reúne las 10 predicciones confiadas que exige `min-support`; todas las demás
quedan en `never` y el único umbral (0,553) se fija con poca muestra. Con 60/20/20
(861 de validación) aparecen umbrales para `fix` (0,517) y `docs` (0,876), y la
precisión real en lo confiado sube de 0,77 a 0,85, a costa de menos datos de
entrenamiento (exactitud 0,67 → 0,64; `build` pasa de 18 a 8 ejemplos y deja de
aprenderse). El script usa 60/20/20 por defecto.

Por clase (palabras + campos, 60/20/20):

| Etiqueta | Precisión | Recall | F1 | Soporte | τ | Confiadas | Precisión confiada |
|---|---|---|---|---|---|---|---|
| build | 0,000 | 0,000 | 0,000 | 82 | never | 0 | — |
| chore | 0,244 | 0,524 | 0,333 | 21 | never | 0 | — |
| ci | 0,655 | 0,787 | 0,715 | 94 | never | 0 | — |
| docs | 0,849 | 0,804 | 0,826 | 112 | 0,876 | 47 | **1,000** |
| feat | 0,189 | 0,424 | 0,262 | 33 | never | 0 | — |
| fix | 0,706 | 0,683 | 0,694 | 271 | 0,517 | 155 | 0,806 |
| perf | 0,000 | 0,000 | 0,000 | 13 | never | 0 | — |
| refactor | 0,364 | 0,615 | 0,457 | 26 | never | 0 | — |
| style | 0,000 | 0,000 | 0,000 | 2 | never | 0 | — |
| test | 0,742 | 0,778 | 0,759 | 207 | never | 0 | — |

### Entre repos (prueba: OpenWA entero, 1 715 commits)

| Modelo | Exactitud | Macro-F1 | ECE | Contesta | Precisión en lo confiado |
|---|---|---|---|---|---|
| Mayoritaria (`fix`) | 0,410 | 0,058 | — | 100 % | 0,410 |
| Reglas de palabras clave | 0,439 | 0,297 | — | 58,8 % | 0,384 |
| Chispa palabras + campos | 0,464 | 0,310 | 0,143 | 15,8 % | **0,583** |
| Chispa solo texto | 0,471 | 0,241 | 0,050 | 0,2 % | 0,000 (3 ejemplos) |

Entre repos, Chispa apenas mejora a las reglas, y los umbrales aprendidos en bun
y el repo privado (es) **no valen** en OpenWA: `docs` (τ 0,665) contesta 239 veces con precisión
0,586, porque en OpenWA muchos `fix` y `chore` tocan ficheros `.md` y el campo
`f:ext=.md` que en bun delataba a `docs` aquí engaña. Sin campos la calibración es
mejor (ECE 0,050) pero el modelo casi nunca se atreve (3 respuestas).

### Binario: `fix` contra el resto (`-one-vs-rest fix`, 60/20/20)

Exactitud 0,726, macro-F1 0,713, ECE 0,064; contesta el 21,1 % con precisión 0,747
(objetivo 0,95 en validación, donde dio 0,954). Mismo patrón: buena
discriminación, umbral optimista bajo el cambio de distribución.

### Cuantización, tamaño y velocidad

- **int16 frente a float64**: la etiqueta coincide en el **100 %** de todos los
  conjuntos de prueba (861 temporal, 1 715 entre repos, y los de validación); en
  el corpus sintético de los tests, también 100 % (el test exige ≥ 99,5 %).
- **Tamaño**: 1,1 MB el modelo de 10 clases (46 854 de 262 144 cubos con peso,
  guardado disperso; denso serían 5,2 MB), 1,85 MB con n-gramas, 282 KB el
  binario. Entrenar tarda 0,1–0,4 s.
- **Latencia** (Apple M4): 4,9 µs por commit real con palabras + campos y 12,7 µs
  con n-gramas, medidos por `kling chispa eval` incluyendo la contabilidad; en el
  benchmark de 200 caracteres, 1,5 µs / 6 µs, sin reservas de memoria.

## Dónde falla

1. **Cambio de distribución** (lo más importante). Los umbrales se fijan en
   validación y prometen su precisión para datos parecidos. Entre el pasado y el
   presente de bun, y aún más entre repos, la mezcla de etiquetas y las
   convenciones cambian, y la precisión confiada cae de 0,95 a 0,85 (temporal) y
   0,58 (entre repos). La calibración por temperatura tampoco se traslada (ECE
   0,14 entre repos).
2. **Clases con pocos ejemplos**: `build` (8 de entrenamiento), `perf` (16) y
   `style` (7) tienen F1 0. `build` en concreto explota en el tramo reciente de bun
   (82 en prueba) y se confunde con `test`, `ci` y `chore`.
3. **Etiquetas débiles que se solapan**: «make the BufferJSON round-trip test
   exercise the codec» es `test` para su autor y `fix` para el modelo; «correct
   README's MCP posture» es `docs` y se lee como `fix`. De los 30 errores
   confiados (de 202 respuestas confiadas), 30 son predicciones `fix`: `test`→`fix`
   12, `feat`→`fix` 6, `docs`→`fix` 5. `fix` es el cajón de sastre del corpus.
4. **Evidencia pobre con pocos datos**: en commits en español (repo privado
   (es), 245 ejemplos) la evidencia de un error confiado era `w:el`, `w:que`, `w:rol`:
   palabras vacías que correlacionan con `fix` solo porque ese repo es casi todo
   `fix`.
5. **Los n-gramas de caracteres no ayudan aquí** (0,627 frente a 0,640) y cuestan
   4× en latencia y 1,7× en tamaño. Siguen disponibles (`-char 3-5`) porque en
   textos con erratas, identificadores o idiomas sin espacios pueden valer; por
   defecto, apagados.

## Qué implica para la cascada Chispa → VON

- Chispa es útil como primer escalón **si se calibra con el tráfico real** del
  gateway: el umbral es una promesa sobre datos como los de validación.
- Para una promesa de precisión ~0,95 en datos que derivan, conviene un objetivo
  más alto (0,99 dio 0,94–0,98 en prueba) y aceptar menos cobertura (6–12 %), o
  recalibrar a menudo con lo que VON contesta en lo escalado.
- Lo que Chispa escala no es basura: en lo escalado acierta el 57 % (reparto
  temporal); pasar su top-3 y la evidencia a VON acota la pregunta.
- Mantenerlo **opt-in** por tarea hasta que una evaluación como esta, sobre los
  datos de esa tarea, muestre mejora.

## Reproducir

```sh
scripts/chispa-eval.sh /tmp/chispa ~/Documents/GitHub/* ~/Github/*      # 60/20/20
TRAIN_PCT=70 VALID_PCT=10 scripts/chispa-eval.sh /tmp/chispa70 ~/Documents/GitHub/* ~/Github/*
go test ./pkg/chispa -run X -bench Predict -benchtime 2s
```

Con `SOURCE_DATE_EPOCH=0` (lo fija el script) y la misma semilla, los `.chispa` salen
idénticos byte a byte. Los números de arriba son de una sola semilla: con ~900
ejemplos de prueba, diferencias de ±0,02 en exactitud están dentro del ruido.
