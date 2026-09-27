# Tareas de intención: una orden corta → intención y huecos

Una **tarea de intención** del gateway de IA (`"intent"` en `ai.json`) decide
qué orden hay en un texto corto —«abre un ticket para facturación», «pon la
luz del salón en azul»— como una intención y sus huecos, con capas de coste
creciente, y dice a dónde escalar lo que ninguna contesta con confianza. Es la
cascada de [`pkg/intent`](../pkg/intent), servida en `POST /v1/decide`:

| capa | qué | coste | cuándo |
|---|---|---|---|
| 1. plantillas | órdenes conocidas, coincidencia exacta | µs | siempre primero |
| 2. Chispa + Chispa-slots | intención ([chispa.md](chispa.md)) y huecos (`.chispas`, `pkg/chispa/slots`) | µs en el proceso; ms serverless | si no encaja una plantilla |
| 3. codificador | un codificador de frases (kind `embed`) y su cabeza `.jenc` ([codificador.md](codificador.md)) | ms, en una microVM | si su evaluación la respalda |
| 4. — | lo que queda sale con `escalate: "von"` | | lo decide quien llama |

Lo que no sabe el gateway lo pone el **dominio**: qué plantillas hay, cómo un
hueco marcado se convierte en un valor, qué necesita cada intención para poder
ejecutarse, cuál es la etiqueta de «fuera de ámbito». El núcleo no trae ningún
dominio: o se describe en un fichero de datos, o lo registra en Go el programa
que embebe el gateway.

## La tarea

```json
{
  "models": {
    "tickets-intent": {"kind": "chispa", "path": "tickets/intent.chispa"},
    "enc":            {"kind": "embed",  "snapshot": "x86-enc-e5"}
  },
  "tasks": {
    "tickets": {
      "intent": {
        "model":  "tickets-intent",
        "schema": "tickets/schema.json",
        "slots":  "tickets/slots.chispas",
        "encoder": "enc",
        "head":   "tickets/head.jenc"
      }
    }
  }
}
```

| campo | qué |
|---|---|
| `model` | el modelo Chispa de intención; `"backend": "microvm"` lo sirve serverless (`kling ai chispa deploy … -slots`, [chispa-serverless.md](chispa-serverless.md)) |
| `schema` / `domain` | el dominio: un fichero de datos (abajo) o el nombre de uno en Go (`aigw.Options.Domains`). Uno de los dos |
| `slots` | el etiquetador de huecos (`.chispas`); con la intención serverless puede venir dentro del dorado |
| `encoder`, `head` | la capa 3: los dos o ninguno |
| `encoder_force` | enciende la capa 3 sin evaluación que la respalde (queda escrito) |
| `final_oos` | da por buena una respuesta «fuera de ámbito» confiada; por defecto escala, porque ahí caen las órdenes indirectas («aquí hace frío») |

Las rutas son relativas al registro. Una tarea de intención no lleva nada más
(ni `chispa`, ni `labels`, ni prompt): `/v1/classify` sobre ella da 400.

## El dominio como datos: `schema`

```json
{
  "out_of_scope": "other",
  "lang": "en",
  "intents": [
    {"name": "open_ticket", "slots": ["queue", "priority"], "required": ["queue"],
     "defaults": {"priority": "normal"}},
    {"name": "close_ticket", "slots": ["id"], "required": ["id"]}
  ],
  "values": {"queue": {"billing": ["invoices", "billing team"], "support": ["help desk"]}},
  "templates": [
    {"text": "open a billing ticket", "intent": "open_ticket", "slots": {"queue": "billing"}}
  ],
  "multi_command": {"verbs": ["open", "close"], "joiners": ["and", "then"]}
}
```

- Los huecos son un objeto `{nombre: valor}` de cadenas.
- Una **plantilla** encaja si el texto, normalizado como lo tokeniza
  Chispa-slots (minúsculas, sin tildes ni puntuación), es exactamente el suyo.
- Un hueco marcado vale su forma canónica de `values`; si el hueco tiene tabla y
  la forma no está, se ignora; sin tabla, vale su texto normalizado.
- `slots` limita los huecos que acepta una intención, `defaults` completa los
  que faltan (igual en la predicción y en el oro al evaluar) y `required` dice
  cuáles necesita: sin ellos la decisión no es confiada (`missing_slot`).
- `multi_command` detecta «verbo … y verbo …»: dos órdenes no las contesta una
  capa de una etiqueta (`multi_command`), y ni pasan por el codificador.
- `lang` es el idioma que recibe Chispa cuando la petición no lo dice.

## El dominio en Go: `domain`

Cuando el dominio no cabe en datos (plantillas con gramática, números
escritos en palabras, léxicos con sinónimos), un programa embebe el gateway y
le pasa su `intent.Domain`:

```go
g, err := aigw.New(aigw.Options{Client: client, ConfigPath: "ai.json",
	Domains: map[string]intent.Domain{"smart-room": room.Domain{Matcher: m}}})
```

y la tarea lo nombra: `"intent": {"model": "…", "domain": "smart-room"}`.
`kling ai serve` no trae ninguno; una tarea que nombra un dominio que el
gateway no tiene contesta 503 y lo dice. El ejemplo completo es
[`examples/domotica`](../examples/domotica/README.md): `kindling-domotica
gateway` es el gateway de kindling con la habitación de demo dentro.

## `/v1/decide`

```sh
kling ai test tickets "open a billing ticket"
```

```json
{"task": "tickets", "decision": "open_ticket", "lang": "en", "intent": "open_ticket",
 "slots": {"priority": "normal", "queue": "billing"}, "layer": "template", "confident": true,
 "prob": 1, "latency_us": 3.1, "latency_ms": 0.02}
```

`layer` es la capa que decidió (`template`, `chispa`, `encoder`, `none`);
`confident: false` viene con `reason` (`low_probability`, `missing_slot`,
`multi_command`, `out_of_scope`, `chispa_error`, `encoder_error`) y `escalate`
(`encoder` si la capa 3 está apagada; `von` si ya pasó por ella). Con Chispa
serverless, `chispa_replica` dice si su microVM estaba congelada, pausada o
despierta y cuánto costó despertarla; `encoder` dice si la capa 3 estaba
encendida y, si no, por qué.

## La puerta de la capa 3

```sh
kling ai eval tickets -data test.jsonl      # filas {"text","lang","intent","slots"}
```

Pasa las filas por la cascada sin y con el codificador y guarda el registro en
`ai-evals/tickets.json`. La capa se enciende solo si contesta bien (confiada y
con la orden **completa**: intención y huecos) más órdenes donde discrepan
(McNemar de una cola, p < 0,05) sin sumar más errores confiados que el 1 % de
lo que gana. El registro va atado al dominio (su nombre o el sha256 del
esquema), a los sha256 de la intención, los huecos y la cabeza, al dorado del
codificador y a `final_oos`: si algo cambia, la capa vuelve a apagarse hasta
repetir la evaluación.

La mejora continua (`"learn"`, [mejora-continua.md](mejora-continua.md)) funciona
igual que en una clasificación: se capturan las órdenes que Chispa escaló por la
intención, con el voto del codificador si contestó confiado, y `kling ai retrain
tickets -eval rows.jsonl` reentrena, promociona con su puerta y vuelve a evaluar
la capa 3.
