# Datos de la tarea de domótica

Qué datos usa la decisión de domótica (`examples/domotica/internal/domotica`, `kindling-domotica`), de
dónde salen, con qué licencia, cómo se convierten a un esquema único y cómo se
reparten sin fugas. Los datos **no** están en el repositorio: los descarga y
genera `examples/domotica/cmd/domotica-data` en una caché. Aquí viven la herramienta, la
taxonomía y la atribución (también en [`NOTICE`](../NOTICE)).

Regla de la fase: solo datos que se pueden usar **y redistribuir** libremente,
con la licencia comprobada en la propia fuente.

## Fuentes

| fuente | versión fijada | licencia (comprobada en) | qué se usa |
|---|---|---|---|
| Amazon **MASSIVE** 1.0 | `amazon-massive-dataset-1.0.tar.gz`, sha256 `7df623fd2d300a4d235d6ee5bd396c9a28258d3a0ccb29abdb054506eba153f8` | **CC BY 4.0** — fichero `1.0/LICENSE` del propio archivo («Copyright Amazon.com Inc. or its affiliates. Attribution 4.0 International»). `1.0/NOTICE.md` añade que parte del texto viene de **SLURP**, también CC BY 4.0 | `es-ES` y `en-US`, 16 520 frases cada uno, repartos oficiales |
| **home-assistant/intents** (hoy `OHF-Voice/intents`; GitHub redirige) | etiqueta `2026.9.17` = commit `4af16c0ccc6f0567654c04554833fe3e5e7467ba`; tarball de codeload sha256 `3da1f44c65ea06232712adb714af91d55b794a939d4506bc7977f700a1b9e2be` | **CC BY 4.0** — `LICENSE.md` del repositorio («Attribution 4.0 International») y la API de GitHub (`license.spdx_id = CC-BY-4.0`) | plantillas de `sentences/{es,en}`, reglas `rules/{es,en}`, listas `lists/`, `lists/{es,en}` |
| órdenes de la demo | `examples/domotica/internal/domotica/demo.go` | la del proyecto | plantillas escritas para la habitación |
| frases de reto | `examples/domotica/internal/domotica/challenge.jsonl` | la del proyecto | 54 frases escritas a mano (indirectas, varias órdenes, casi-fuera-de-ámbito, paráfrasis) |

Nota sobre home-assistant/intents: el paquete de PyPI `home-assistant-intents`
(que trae las mismas frases ya compiladas a JSON) declara Apache-2.0 en sus
metadatos. No se usa: se parte del repositorio, cuya licencia es CC BY 4.0, y
es a esa a la que se atribuye.

Las dos licencias CC BY 4.0 permiten usar, adaptar y redistribuir (también
comercialmente) con **atribución** e **indicando los cambios**. Los cambios que
hace esta herramienta: se filtran los idiomas es/en, se re-etiquetan las
intenciones a la taxonomía de abajo, se recortan y normalizan los huecos, y en
Home Assistant se expanden las plantillas en frases concretas rellenando las
listas con zonas y dispositivos propios. Los datos derivados llevan una copia
de las licencias en `data/LICENSES/`.

Atribución requerida (la misma que en `NOTICE`):

- MASSIVE — FitzGerald et al., *MASSIVE: A 1M-Example Multilingual Natural
  Language Understanding Dataset with 51 Typologically-Diverse Languages*
  (2022), https://github.com/alexa/massive — CC BY 4.0. Incluye y deriva de
  SLURP — Bastianelli et al., *SLURP: A Spoken Language Understanding Resource
  Package* (EMNLP 2020), https://github.com/pswietojanski/slurp — CC BY 4.0.
- Home Assistant intents — © los contribuidores de
  https://github.com/OHF-Voice/intents (antes home-assistant/intents) — CC BY 4.0.

## Uso

```sh
go run ./examples/domotica/cmd/domotica-data fetch            # descarga (con tope de tamaño) y comprueba sha256
go run ./examples/domotica/cmd/domotica-data build            # escribe CACHE/data/{train,valid,test}.jsonl
```

La caché es `$KLING_DOMOTICA_CACHE` o `<user cache dir>/kindling/domotica`
(`~/Library/Caches/...` en macOS, `~/.cache/...` en Linux); `-cache` y `-out`
la cambian. `fetch` descarga a un `.part`, limita los bytes y solo renombra
si el sha256 coincide; `build` vuelve a comprobar el sha256 antes de abrir
nada, y lee los `.tar.gz` con topes por entrada y por total descomprimido.
Sin dependencias: el YAML de Home Assistant se lee con un analizador propio
del subconjunto que usan esos ficheros (`examples/domotica/cmd/domotica-data/yaml.go`).

`build` es determinista: dos ejecuciones dan ficheros idénticos byte a byte
(semillas fijas por plantilla, splitmix64). Mandos: `-ha-k` (frases por
plantilla de HA dentro de ámbito, 30), `-ha-k-oos` (fuera de ámbito, 4),
`-demo-k` (por plantilla de la demo, 20).

## Esquema unificado

Una línea JSON por frase:

```json
{"text": "pon la temperatura del salón a veintidós grados", "lang": "es",
 "intent": "set_temperature",
 "slots": {"device": "thermostat", "area": "living_room", "value": 22, "unit": "°C"},
 "spans": [{"slot": "area", "start": 23, "end": 29}, {"slot": "value", "start": 32, "end": 42}],
 "source": "demo", "family": "demo/17", "split": "train"}
```

- `slots`: valores **normalizados** — dispositivo y zona canónicos, número,
  unidad (`%`, `°C`, `°F`), color canónico (nombre inglés). Es lo que se compara
  al evaluar. Los implícitos se completan con la taxonomía (`Resolve`): «apaga
  la cocina» es `turn_off {device: light, area: kitchen}`.
- `spans`: huecos sobre el texto, **en bytes** (UTF-8), no en caracteres. Es lo
  que aprende el etiquetador.
- `source`: `massive`, `ha` o `demo`. `family`: grupo que no cruza de reparto.
- Además se escribe `chispa/{train,valid,test}.jsonl` en el formato de
  `kling ai chispa train` (`{"text", "label", "fields": {"lang"}}`).

## Taxonomía de la habitación de demo

Dispositivos: `light`, `thermostat`, `blinds`, `tv`, `lock`, `alarm`, `fan`,
`speaker`, `plug`. Zonas canónicas: `living_room`, `kitchen`, `bedroom`,
`room`, `bathroom`, `office`, `dining_room`, `hallway`, `garage`, `garden`,
`house`, `kids_room`, `guest_room` (una zona desconocida se queda como texto
normalizado).

| intención | dispositivo implícito | valor | MASSIVE | Home Assistant |
|---|---|---|---|---|
| `turn_on` / `turn_off` | light | — | `iot_hue_lighton/off` (light), `iot_wemo_on/off` (plug) | `HassTurnOn/Off` con dominio light, fan, switch→plug, media_player→tv/speaker, climate→thermostat |
| `brightness_up` / `brightness_down` | light | paso opcional (%) | `iot_hue_lightup` / `iot_hue_lightdim` | — |
| `set_brightness` | light | obligatorio (%) | — | `HassLightSet` con `{brightness}` |
| `set_color` | light | color obligatorio | `iot_hue_lightchange` | `HassLightSet` con `{color}` |
| `set_temperature` | thermostat | obligatorio (°C) | — | `HassClimateSetTemperature` |
| `temperature_up` / `temperature_down` | thermostat | paso opcional | — | — (solo demo) |
| `get_temperature` | thermostat | — | — | `HassClimateGetTemperature` |
| `cover_open` / `cover_close` | blinds | — | — | `HassTurnOn/Off` con clase blind, curtain, shade, shutter, awning |
| `cover_set_position` | blinds | obligatorio (%) | — | `HassSetPosition` (cubiertas) |
| `lock` / `unlock` | lock | — | — | `HassTurnOn/Off` con dominio lock |
| `alarm_arm` / `alarm_disarm` | alarm | — | — | — (solo demo) |
| `media_pause` / `media_resume` / `media_next` / `media_previous` | — | — | — | `HassMediaPause` / `Unpause` / `Next` / `Previous` |
| `volume_up` / `volume_down` | — | paso opcional (%) | `audio_volume_up` / `down` | `HassSetVolumeRelative` (up/down) |
| `volume_set` | — | obligatorio (%) | `audio_volume_other` | `HassSetVolume` |
| `volume_mute` / `volume_unmute` | — | — | `audio_volume_mute` | `HassMediaPlayerMute` / `Unmute` |
| `fan_set_speed` | fan | obligatorio (%) | — | `HassFanSetSpeed` |
| `out_of_scope` | — | — | **todo lo demás** | todo lo demás |

Decisiones que conviene conocer:

- **`alarm_*` de MASSIVE es el despertador** («despiértame a las siete», «apaga
  la alarma» del reloj), no la alarma de seguridad: va a `out_of_scope`. La
  demo, en cambio, dice «activa/desactiva la alarma» para la de seguridad. Es
  un conflicto real de significado y la evaluación lo muestra (ver
  DOMOTICA-EVAL).
- `iot_coffee`, `iot_cleaning` (cafetera, aspirador), `play_*`, `weather_*`,
  temporizadores, listas, estados («¿está encendida la luz?»), escenas,
  válvulas, puertas de garaje y ventanas → `out_of_scope`: no están en la
  habitación o no son una orden.
- Home Assistant: las plantillas con `{floor}` (199 de 1631) se omiten; las
  cubiertas que no son persianas/cortinas (puerta, garaje, verja, ventana) se
  descartan; la temperatura de color (`HassLightSet` con Kelvin) se descarta;
  una frase que es solo un nombre con artículo («la lámpara», que HA acepta
  como «enciende») se descarta.
- Huecos de MASSIVE que pasan: `house_place`→area, `device_type`→device (si es
  un dispositivo de la habitación), `color_type`→color, `change_amount`→value
  (recortado al número: «a treinta y cinco por ciento» → «treinta y cinco»;
  sin número, como «un poco», no hay valor). El resto (time, date…) se ignora.
- En las frases dentro de ámbito se marcan como `device` las menciones del
  léxico que la fuente dejó sin anotar («las **luces**»): sin eso la misma
  palabra sería hueco en unas frases y nada en otras.

## Repartos sin fugas

- MASSIVE conserva sus repartos oficiales (train/dev/test → train/valid/test).
- Home Assistant: la **familia es la plantilla** (todas las frases que salen
  de una misma cadena de `sentences:` van juntas). Las familias de cada
  (fuente, intención) se ordenan por hash y se reparten 7 train / 1 valid /
  2 test de cada 10: estratificar así evita que una intención con pocas
  plantillas se quede sin test.
- Demo: la familia es la plantilla, igual.
- Una frase de HA o de la demo que ya existe (normalizada, mismo idioma) en
  otra fuente o reparto se descarta (282 de HA y 121 de la demo).

## Tamaños (build con los valores por defecto)

| reparto | filas | MASSIVE es/en | HA es/en | demo es/en |
|---|---:|---|---|---|
| train | 35 765 | 11 514 / 11 514 | 4 685 / 7 078 | 430 / 544 |
| valid | 6 014 | 2 033 / 2 033 | 794 / 945 | 118 / 91 |
| test | 9 836 | 2 974 / 2 974 | 1 445 / 1 975 | 280 / 188 |

En total 51 615 frases (17 MB de JSONL); `out_of_scope` es el 67 % (casi todo
MASSIVE). Home Assistant: 413 ficheros leídos (1 con anclas YAML, omitido),
712 bloques, 1 631 plantillas, 17 204 frases generadas. Por intención, en
train/valid/test, van desde `turn_off` (2 216 / 261 / 661) hasta `alarm_arm`
(6 / 0 / 4) y `temperature_up/down` (20 / 0 / 20): estas últimas solo existen
en la demo.
