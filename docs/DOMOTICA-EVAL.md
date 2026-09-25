# Evaluación: decisión de domótica, capas 1 y 2

Qué tal deciden las capas rápidas qué hacer con una orden de la habitación de
demo, medido sobre datos libres y con las cifras que no favorecen. Diseño en
[domotica.md](domotica.md); datos, licencias y repartos en
[domotica-datos.md](domotica-datos.md).

**Resumen.** La cascada plantillas → Chispa acierta la orden **completa**
(intención y todos los huecos) en el **94,7 %** de las órdenes de la
habitación del test (92,6 % es, 96,4 % en), contesta con confianza el 88 % y,
de lo que contesta, acierta el **97,6 %**; lo demás escala. Tarda ~5 µs de
mediana. Las reglas por palabras clave, en las mismas frases, se quedan en un
65,7 % sin saber dudar. Pero sobre las frases **humanas** de MASSIVE —las que
no salen de una gramática— la orden completa baja al **72–78 %**: esa es la
cifra honesta de lo que Chispa sabe de verdad, y la razón de las capas 3 y 4.

## Montaje

- Datos: `tools/domotica-data build` con los valores por defecto (test: 9 794
  frases, 3 541 dentro de ámbito). Los repartos no comparten familias ni
  frases normalizadas (ver domotica-datos.md).
- Intención: `kling chispa train -char 3-5 -class-weight sqrt -lr 0.2 -epochs 100`
  (elegido por el macro-F1 de **validación** entre seis configuraciones; el de
  test no se miró para elegir). 28 etiquetas, 5,3 MB, temperatura 0,687, acuerdo
  int16/float 1,000.
- Huecos: `kling domotica train-slots` por defecto (2^17 cubos, ventana 2,
  afijos 3, léxico). 101 KB, acuerdo int16/float 1,000.
- `kling domotica eval -data test.jsonl`, Apple M4, Go 1.27.1, un hilo.
- Métricas: *intent* = acierto de intención; *mF1* = macro-F1 de intención;
  *slotF1* = F1 de pares hueco=valor normalizados (zona, dispositivo, valor,
  unidad, color; implícitos completados igual en oro y predicción); **exact** =
  intención y todos los huecos bien, la que responde a «¿hizo el dispositivo
  correcto lo correcto?»; *cover* = parte contestada con confianza; *prec@c* =
  exact entre lo confiado.

## Órdenes de la habitación (dentro de ámbito, 3 541 frases)

| sistema | idioma | intent | mF1 | slotF1 | **exact** | cover | prec@c |
|---|---|---:|---:|---:|---:|---:|---:|
| plantillas (capa 1) | es | 0,264 | 0,449 | 0,473 | 0,263 | 26,7 % | 0,986 |
| | en | 0,171 | 0,249 | 0,341 | 0,171 | 17,1 % | **1,000** |
| palabras clave | es | 0,609 | 0,682 | 0,874 | 0,599 | 100 % | 0,599 |
| | en | 0,729 | 0,761 | 0,929 | 0,704 | 100 % | 0,704 |
| Chispa + Chispa-slots | es | 0,866 | 0,735 | 0,982 | 0,850 | 76,9 % | 0,923 |
| | en | 0,963 | 0,952 | 0,992 | 0,953 | 88,6 % | 0,976 |
| **cascada** (plantillas → Chispa) | es | 0,943 | 0,953 | 0,990 | **0,926** | 85,3 % | 0,963 |
| | en | 0,973 | 0,966 | 0,993 | **0,964** | 90,2 % | 0,985 |

Por fuente (cascada; en paréntesis, Chispa sola):

| fuente | n | intent | slotF1 | **exact** | cover | prec@c |
|---|---:|---:|---:|---:|---:|---:|
| MASSIVE es (frases humanas) | 220 | 0,800 (0,805) | 0,911 | **0,718** (0,723) | 73,2 % | 0,839 |
| MASSIVE en (frases humanas) | 220 | 0,859 (0,859) | 0,927 | **0,782** (0,777) | 72,7 % | 0,881 |
| Home Assistant (plantillas no vistas) | 2 675 | 0,975 (0,965) | 0,997 | 0,972 (0,961) | 88,6 % | 0,987 |
| demo (plantillas no vistas por Chispa) | 426 | 1,000 (0,730) | 1,000 | 1,000 (0,730) | 100 % | 1,000 |

Cómo leerlo:

- **Capa 1 (plantillas)**: por construcción reconoce el 100 % de las órdenes de
  la demo con el 100 % de acierto (426/426 en test, y un test de unidades fija
  las frases de la demo). Fuera de la demo casi no contesta, y cuando lo hace
  acierta el 99,2 %; sus fallos en MASSIVE son conflictos de taxonomía (abajo),
  no errores de emparejado. **Se activa.**
- **Capa 2 (Chispa)** mejora con mucho la línea base de reglas en todo (exact
  dentro de ámbito 0,907 frente a 0,657) y la cascada mejora a cada capa sola:
  **se activa**. Las reglas ganan en una sola cosa: siempre contestan; Chispa
  escala un 12–15 % dentro de ámbito.
- **Español peor que inglés** (exact 0,926 frente a 0,964): Home Assistant
  tiene menos plantillas en español (4 685 frases de train frente a 7 078) y
  las traducciones de MASSIVE son más variadas. Donde más se nota es en Chispa
  sola (0,850 frente a 0,953).
- **Home Assistant es optimista**: sus frases de test salen de plantillas que
  no se vieron, pero con las mismas reglas de expansión y las mismas listas
  (zonas, nombres) que train. Mide que Chispa generaliza entre plantillas, no a
  cómo habla la gente. Esa es la fila de MASSIVE, y tiene solo 220 frases por
  idioma: ±6 puntos al 95 %.

## Fuera de ámbito (6 253 frases)

Chispa reconoce lo que no es de la habitación con **99,3 %** de acierto (confiado
en el 99,7 %; las reglas, 95,4 %). Aun así, **la cascada no da por buena esa
respuesta: la escala** (`reason: out_of_scope`). El motivo está en las frases
de reto: con «fuera de ámbito» como respuesta final, 8 de 18 órdenes
indirectas («it's freezing in here», «me voy de casa») se quedarían en «no hago
nada» con toda confianza; escalándolo, solo 1. El precio es que todo lo que no
es de la habitación pasa por la capa siguiente; en una demo donde casi todo lo
que se dice es una orden, compensa. `Decider.FinalOOS` es la otra política
(fila «cascade, OOS final» de `kling domotica eval`: cubre el 95,7 % con
precisión 0,985, pero comete esos 8 errores).

## Huecos: el etiquetador solo (F1 de huecos exactos)

| fuente | area | device | value | color | total |
|---|---:|---:|---:|---:|---:|
| todas (3 541 frases) | 0,986 | 0,995 | 0,994 | 0,917 | **0,990** |
| Home Assistant | 0,997 | 1,000 | 0,998 | 1,000 | 0,999 |
| MASSIVE (440 frases) | 0,808 | 0,963 | 0,789 | 0,723 | **0,899** |

Otra vez: sobre frases generadas por gramática es casi perfecto; sobre frases
humanas, 0,90. Las zonas que no están en el léxico («porche», «cobertizo») se
pierden, los colores raros («tono pastel») también, y en MASSIVE el valor de
`change_amount` incluye cosas como «al nivel mínimo» que el etiquetador marca
de otra forma. Aun así el F1 de valores normalizados (lo que importa al
ejecutar) en MASSIVE es 0,91–0,93.

## Latencia (en caliente, un hilo, M4)

| capa | p50 | p99 | reservas |
|---|---:|---:|---:|
| plantillas (`Matcher.Match`, 62 000 frases) | 1,5 µs | 5,3 µs | 0 |
| Chispa intención (palabras + bigramas + n-gramas 3–5) | ~3 µs | ~10 µs | 0 |
| Chispa-slots (`Tag`) | 1,4 µs | — | 0 |
| palabras clave (línea base) | 3,6 µs | 11 µs | varias |
| **cascada completa** (plantillas → Chispa + huecos + normalización) | **4,7 µs** | **13 µs** | pocas (normalizar huecos reserva) |

Medido por frase sobre las 9 794 del test (`kling domotica eval`) y con
`go test -bench` (`BenchmarkMatch` 1,6 µs, `BenchmarkTag` 1,4 µs, ambos sin
reservas). Una llamada suelta desde la CLI tarda 15–90 µs porque paga el
arranque en frío. La capa 3 (~10 ms) es 2 000 veces más cara.

## Frases de reto (54, escritas a mano)

«Bien» = acierto confiado; «escala» = no confiado (lo decidirá una capa
mayor: aceptable); **«MAL»** = error confiado, lo único grave: la habitación
haría otra cosa.

| clase | n | plantillas (bien/escala/MAL) | reglas | Chispa sola | **cascada** |
|---|---:|---|---|---|---|
| indirecta («aquí hace frío») | 18 | 0 / 18 / 0 | 0 / 0 / **18** | 7 / 3 / **8** | 7 / 10 / **1** |
| varias órdenes («… y …») | 9 | 0 / 9 / 0 | 6 / 0 / **3** | 0 / 9 / 0 | 0 / 9 / 0 |
| casi fuera de ámbito («pon una alarma a las 7») | 15 | 0 / 15 / 0 | 13 / 0 / **2** | 13 / 2 / 0 | 0 / 15 / 0 |
| paráfrasis directa («kill the lights») | 12 | 1 / 11 / 0 | 8 / 0 / **4** | 8 / 2 / **2** | 8 / 3 / **1** |

Los dos errores confiados de la cascada:

- «hace muchísimo calor en el salón» → `get_temperature` (debería ser bajar la
  temperatura): Chispa ve «calor» + zona y lo lee como pregunta.
- «quiero la persiana a la mitad» → `cover_close` sin valor (debería ser
  `cover_set_position` 50 %).

Chispa acierta sola 7 de 18 indirectas porque MASSIVE trae algunas («está un
poquito oscuro, sube la luz»). Ninguna frase de reto la contesta mal la capa 1
(test de unidades).

## Clases de error

1. **Conflictos de taxonomía con MASSIVE** (el oro no es «lo que haría la
   habitación»): «apaga la alarma» / «turn off the alarm» es el despertador en
   MASSIVE y la alarma de seguridad en la demo; «qué temperatura hace» /
   «how hot is it in quebec» es el tiempo; «siguiente», «pon la canción
   anterior» son música (`play_*`), aquí `media_next/previous`. Son la mayoría
   de los errores confiados de la capa 1 (por eso su precisión en MASSIVE
   español es 0,891 y no 1,000) y no se han «arreglado» cambiando el oro.
2. **Ruido de etiqueta en MASSIVE**: «por favor sube el volumen» etiquetado
   `volume_down`; «enciende las luces» como `brightness_up`; «dónde está el
   coche» como `volume_up`.
3. **Relativo frente a absoluto**: «baja el volumen al cincuenta por ciento»
   (`volume_down` con paso en el oro, `volume_set` en la predicción), «sube el
   brillo al máximo», «baja las luces a la mitad». La frontera la decide la
   preposición; con pocos ejemplos, Chispa duda o falla.
4. **Intenciones con pocos datos**: `alarm_arm/disarm` (6–7 frases de train) y
   `temperature_up/down` (20, solo de la demo) nunca llegan al umbral en Chispa:
   las contesta la capa 1 si son de la demo y, si no, escalan. `set_temperature`
   (F1 0,71) se confunde con `cover_set_position` y `set_brightness` cuando la
   frase no nombra el dispositivo («ponlo a 22»).
5. **Dispositivo por defecto**: «apaga» a secas (MASSIVE, enchufe) sale como
   luz porque `turn_off` sin dispositivo es la luz por diseño.
6. **Zonas fuera del léxico** («porche», «cobertizo», «la habitación de mi
   hijo»): el etiquetador no las marca.

## Qué debe escalar a las capas 3 (codificador) y 4 (VON)

Ya escala hoy (`confident: false`, `escalate: "encoder"`):

- **Lenguaje indirecto**: «aquí hace frío», «no veo nada», «me voy a dormir»,
  «the sun is in my eyes». Chispa no está hecho para inferir la intención de un
  estado; el LLM sí. Es la mayor parte del trabajo de la capa 4.
- **Todo lo que Chispa da por fuera de ámbito**, por la misma razón (política por
  defecto).
- **Varias órdenes en una frase** (heurística: dos verbos de orden unidos por
  «y/and/luego/then»): la capa rápida solo devuelve una acción; VON puede
  devolver una lista.
- **Órdenes incompletas**: `set_temperature` sin valor, `set_color` sin color.
- **Paráfrasis raras y relativos** («dale caña al volumen», «a la mitad»)
  cuando Chispa no llega al umbral de la clase.

Que aún se cuela con confianza y la capa 3 debería atrapar:

- Frases de sensación con palabra de dispositivo o zona («hace calor en el
  salón» → `get_temperature`).
- Relativo frente a absoluto con valor («baja … al 50 %»).
- Conflictos de significado de «alarma» (seguridad frente a despertador) y
  «temperatura» (termostato frente al tiempo), que dependen del contexto de la
  habitación más que de las palabras.

Para la fase 3: el codificador tendría que batir estas cifras en MASSIVE
(exact 0,72 es / 0,78 en) y en las frases de reto (2 errores confiados de 54)
para ganarse su sitio entre Chispa y VON.

## Capa 3: el codificador

Diseño, modelo y cifras de latencia y memoria en [codificador.md](codificador.md).
Aquí, lo que cambia en la evaluación: multilingual-e5-small (MIT) congelado,
Q8_0, con una cabeza de una capa oculta (256) entrenada en train y calibrada
en validación, solo sobre lo que las capas rápidas escalan. Mismo test, mismos
modelos de las capas 1 y 2; `kling domotica eval -encoder head.jenc
-embed-cache e5.jemb`.

**La marca queda batida**: MASSIVE exact **0,745 es / 0,800 en** (0,718 / 0,782)
y **2** errores confiados en las frases de reto (los mismos dos de Chispa). Pero
la mejora en MASSIVE está dentro del ruido de 220 frases por idioma, y el
codificador congelado no resuelve lo indirecto.

| sistema | grupo | intent | slotF1 | **exact** | cover | prec@c |
|---|---|---:|---:|---:|---:|---:|
| cascada (capas 1–2) | dentro de ámbito | 0,960 | 0,992 | 0,947 | 88,1 % | 0,976 |
| **cascada + codificador** | dentro de ámbito | 0,968 | 0,993 | **0,956** | **90,7 %** | 0,976 |
| | es | 0,956 | 0,991 | 0,939 | 88,2 % | 0,964 |
| | en | 0,978 | 0,994 | 0,969 | 92,7 % | 0,985 |
| | MASSIVE es | 0,832 | 0,924 | **0,745** | 73,6 % | 0,840 |
| | MASSIVE en | 0,873 | 0,930 | **0,800** | 74,1 % | 0,877 |
| | Home Assistant | 0,982 | 0,998 | 0,979 | 92,0 % | 0,987 |
| | demo | 1,000 | 1,000 | 1,000 | 100 % | 1,000 |
| | fuera de ámbito | 0,990 | — | 0,990 | 0,4 % | — |
| | todo | 0,982 | 0,993 | 0,977 | 33,1 % | 0,968 |
| codificador solo | dentro de ámbito | 0,926 | 0,992 | 0,913 | 16,4 % | 0,924 |
| | MASSIVE es / en | 0,814 / 0,877 | | 0,723 / 0,800 | 12 % | 0,56 |

Comparaciones (test; lo elegido se eligió en validación):

- **Regresión logística** en vez de la capa oculta: MASSIVE 0,755 / 0,814,
  dentro de ámbito 0,957. En test sale un pelo mejor; en validación, peor
  (0,963 frente a 0,969). Ruido en los dos sentidos.
- **paraphrase-multilingual-MiniLM-L12-v2** (Apache-2.0), misma cabeza:
  MASSIVE 0,745 / 0,805, dentro de ámbito 0,954; intención de la cabeza 0,949
  frente a 0,961.
- **k-NN** (k=10, coseno) sobre los mismos vectores: intención 0,914 (la cabeza,
  0,961).

Frases de reto (bien / escala / MAL):

| clase | cascada | **cascada + codificador** | codificador solo |
|---|---|---|---|
| indirecta (18) | 7 / 10 / 1 | 7 / 10 / **1** | 0 / 15 / 3 |
| varias órdenes (9) | 0 / 9 / 0 | 0 / 9 / 0 | 0 / 9 / 0 |
| casi fuera de ámbito (15) | 0 / 15 / 0 | 0 / 15 / 0 | 12 / 3 / 0 |
| paráfrasis directa (12) | 8 / 3 / 1 | 8 / 3 / **1** | 3 / 9 / 0 |

Qué contesta de verdad la capa 3: de las 6 648 frases de test que le llegan,
94 con confianza (93 bien), casi todas `cover_open` y `cover_set_position` que
Chispa dudaba. 23 de las 28 clases no tienen umbral (en validación no hay bastantes
filas escaladas de esas clases para prometer 0,90), así que en el resto su papel
es la conjetura que acompaña a la escalada. Por eso la puerta del gateway cuenta
órdenes **contestadas** bien (+93, McNemar p = 1e-28, 105 errores confiados
frente a 104): contando también las conjeturas, +12 no es significativo
(p = 0,097).

**Indirectas nuevas** (40 escritas a mano para este trabajo, reparto de test de
`pkg/domotica/indirect.jsonl`; optimistas, son del mismo proyecto): la cascada
con codificador contesta 2 bien, escala 37 y falla 1 (una de Chispa). Con los
ejemplos de train de ese fichero (`train-encoder -indirect`), la intención que
propone la cabeza acierta el 60 % de esas indirectas (17,5 % sin ellos; en las
18 del reto, 61 % frente a 39 %), pero ninguna llega al umbral y la precisión
de lo confiado baja de 0,968 a 0,963: queda como opción, y como punto de
partida del ajuste fino con GPU (receta en codificador.md).

Lo que la capa 3 **no** arregla y sigue siendo de VON: lo indirecto, lo fuera de
ámbito (se escala por política), las órdenes múltiples, lo casi fuera de ámbito,
relativo frente a absoluto y los conflictos de taxonomía de MASSIVE. Los dos
errores confiados que quedan son de Chispa y la capa 3 no llega a verlos.
