# Habitación de demo: domótica con modelos serverless en kindling

Una página web con una habitación dibujada —luces por zona con brillo y color,
termostato, persianas, tele, altavoz, cerradura, alarma, ventilador y enchufe—
que se maneja con órdenes de voz (como texto) en español o inglés. Es un
**ejemplo que usa kindling**, no parte de él: no decide nada por sí mismo. Cada
orden va al gateway de IA (`kling ai serve`), que la pasa por las capas, y la
página enseña, orden a orden:

- qué capa decidió (plantillas, Chispa, codificador o LLM), con su confianza y
  su latencia (µs las rápidas, ms el codificador, segundos el LLM);
- si la microVM de esa capa estaba **congelada y se descongeló** (y cuánto
  tardó), se creó desde su dorado o ya estaba despierta;
- el panel de **microVMs de cada capa**: despiertas o congeladas (0 CPU) y la
  memoria que usan, leído del daemon;
- la acción en JSON y lo que haría la habitación, animado en el plano.

```
navegador ──HTTP/SSE──> examples/domotica ──/v1/decide───> kling ai serve ──> plantillas + Chispa (en su proceso, µs)
 (la habitación)        (simulador + página)  /v1/generate      │                └─> codificador: microVM, dorado kind embed (ms)
                              │                /metrics         └─> LLM (VON): microVM, dorado con el prompt ya evaluado (s)
                              └──── pkg/api ───────────────> daemon de kindling: máquinas (despiertas/congeladas) y memoria
```

## Las capas

| capa | dónde | qué decide | cuándo entra |
|---|---|---|---|
| 1. plantillas | proceso del gateway | las órdenes de la demo, exactas (`kling domotica templates`) | siempre primero |
| 2. Chispa | proceso del gateway | intención + huecos de lo que se parece a una orden | si no encaja una plantilla |
| 3. codificador | microVM (`kind embed`) | intención de frases que Chispa duda | si su evaluación la respalda (`kling ai eval room`) |
| 4. LLM | microVM (VON) | varias órdenes en una, valores relativos, paráfrasis raras | si su evaluación la respalda (`kling domotica eval-llm`), y **solo con el alcance que respalda** |

La capa 4 contesta con JSON restringido por un esquema (`{"kind", "reply",
"actions":[{"intent","device","area","value","color"}]}`) y la demo lo valida
contra la taxonomía antes de tocar nada: intención conocida, dispositivo que
puede hacerla, valor en rango, zona, color y número que la frase **nombra**
(si no, se quitan o se pregunta), y nunca abrir la puerta ni desarmar la
alarma sin que la frase lo diga. Si algo falla, no hace nada y pide
aclaración. Las cifras, en [docs/DOMOTICA-EVAL.md](../../docs/DOMOTICA-EVAL.md#capa-4-el-llm-von).

**Lo que dice la evaluación y enseña la demo**: con Qwen2.5-1.5B Q4_K_M, la
capa 4 **gana** a «escalar y no hacer nada» en lo que Chispa duda (31 órdenes
de MASSIVE bien, ninguna acción donde no había que hacer nada, p = 5·10⁻¹⁰),
pero **pierde** si también se le pregunta por lo que Chispa da por fuera de
ámbito: ahí están las órdenes indirectas («aquí hace frío»), sí, pero también
toda la charla que no es para la habitación, y el LLM pequeño actúa en el 1,5 %
de ella. Así que la demo la enciende con el alcance `uncertain`: «aquí hace
frío» se queda sin hacer (la traza lo dice: LLM saltada) hasta que la capa 3
aprenda lo indirecto o haya un LLM mejor. `-layer4-force` la enciende para todo.

## En el Mac o en un Linux, para probar

```sh
# 1. modelos de las capas rápidas (docs/domotica.md) y el LLM
kling models add von-qwen15-dom -model qwen2.5-1.5b-instruct -quant q4_k_m \
    -prefix <(jq -r '.tasks["room-llm"].system' examples/domotica/ai.json)   # el prompt, ya evaluado en el dorado

# 2. el registro del gateway: ai.json de este directorio, con las rutas y los dorados de tu host
mkdir -p ~/.config/kling/domotica && cp intent.jev slots.jevs ~/.config/kling/domotica/
cp examples/domotica/ai.json ~/.config/kling/ai.json     # quita "encoder"/"head" si no tienes codificador
kling ai serve &                                          # socket 0600 en ~/.config/kling/ai.sock

# 3. la puerta de la capa 4 (10 min en un M4): escribe layer4-room-llm.json
kling domotica eval-llm -gateway ~/.config/kling/ai.sock -llm-task room-llm -decide-task room \
    -data ~/Library/Caches/kindling/domotica/data/test.jsonl

# 4. la demo
go run ./examples/domotica                                # http://127.0.0.1:8088/
```

Sin gateway, `-offline` sirve las capas 1 y 2 en el propio proceso (con
`intent.jev` y `slots.jevs` de la carpeta de modelos): las demás salen «no
disponible».

| flag | por defecto | qué hace |
|---|---|---|
| `-listen` | `127.0.0.1:8088` | dirección de la página; `0.0.0.0:8088` para verla desde la red |
| `-gateway` | `~/.config/kling/ai.sock` | el gateway: su socket o `http://host:puerto` (con `-token-file` o `$KLING_AI_TOKEN`) |
| `-H` | el daemon configurado | daemon para el panel de máquinas (`none`: sin panel) |
| `-decide-task` / `-llm-task` | `room` / `room-llm` | las tareas del registro: capas 1–3 y capa 4 |
| `-layer4-record` | `layer4-<llm-task>.json` en la carpeta de modelos | la evaluación que enciende la capa 4 |
| `-layer4-force` | — | capa 4 para todo lo que escala, sin respaldo |
| `-offline` | — | sin gateway: capas 1–2 en el proceso |

## En el servidor x86 (CT 105 `fc-test`, i7-8700T)

El daemon ya corre como `kling.service`. Todo lo de la demo vive en
`~/domotica` del usuario `juan` y no toca el binario del sistema; el gateway y
la demo son dos servicios de systemd. Presupuesto de memoria (el host también
es el router de casa), medido: gateway y demo ~40 MiB; codificador despierto
317 MiB (PSS); LLM despierto 1 053–1 089 MiB; congelados, nada. Con `-idle 2m`
casi siempre están congelados.

Medido allí (i7-8700T, 4 núcleos del CT, Firecracker sin anidar):

| | |
|---|---|
| plantilla | 0,3 ms de punta a punta (navegador → demo → gateway) |
| codificador congelado → descongelado → decide | 240 ms (descongelar 141 ms) |
| codificador despierto | 18 ms (p50 del codificador en la evaluación: 9,3 ms) |
| LLM congelado → descongelado → decide (dos órdenes) | 6,9 s (descongelar **134 ms**; el resto, generar ~70 tokens) |
| LLM despierto | p50 2,3 s, p90 4,1 s |
| puerta de la capa 3 (`kling ai eval room`) | pasa: +104 órdenes contestadas bien, p = 5·10⁻³² |
| puerta de la capa 4 (`eval-llm`, con el codificador delante) | `uncertain` pasa (30 / 0, p = 9·10⁻¹⁰; exact 0,964 → 0,969); `all` no (0,960) |

```sh
# desde el Mac: binarios linux/amd64 y datos
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/kling ./cmd/kling
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/domotica-demo ./examples/domotica
ssh ct105 'mkdir -p ~/domotica/models ~/domotica/data'
scp /tmp/kling /tmp/domotica-demo examples/domotica/ai.json ct105:domotica/
scp $M/intent.jev $M/slots.jevs ct105:domotica/models/        # M: carpeta de modelos de domótica
scp $D/train.jsonl $D/valid.jsonl $D/test.jsonl ct105:domotica/data/

# en el CT: el LLM, con el prompt de la capa 4 ya evaluado en el dorado. Sin
# imagen nueva: una réplica del dorado x86-qwen15-q4, una petición con el prompt
# (1 155 tokens, 15 s en frío) y commit. (`kling models add … -prefix system.txt`
# hace lo mismo, pero construye otra imagen de 1,1 GB.)
cd ~/domotica
./kling run -from x86-qwen15-q4 -name dom-warm
IP=$(./kling inspect dom-warm | jq -r .ip)
jq '{messages:[{role:"system",content:.tasks["room-llm"].system},{role:"user",content:"Request: enciende la luz"}],
     max_tokens:200,temperature:0,json_schema:.tasks["room-llm"].json_schema}' ai.json > body.json
curl -s http://$IP:8000/v1/chat/completions -H 'Content-Type: application/json' -d @body.json >/dev/null
./kling squeeze dom-warm && ./kling commit -replace dom-warm x86-qwen15-dom && ./kling rm dom-warm

# el codificador: GGUF convertido y verificado (python3 y red; ~1,8 GB en WORK, bórralo después).
# UV_SHA256_X86: el .sha256 del release de uv 0.12.18 en GitHub.
sudo env UV_SHA256_X86=89eadd7c76fc063887959510d5ba0ab1264dfd5f1143b925ddb73021a40acf16 \
    WORK=$HOME/.cache/kindling/encoder-gguf scripts/encoder-gguf.sh multilingual-e5-small -install
sudo rm -rf ~/.cache/kindling/encoder-gguf
./kling models add x86-enc-e5 -model multilingual-e5-small
./kling run -from x86-enc-e5 -name enc-train && A=$(./kling inspect enc-train | jq -r .ip)
./kling domotica embed -url http://$A:8000 -model multilingual-e5-small \
    -data data/train.jsonl,data/valid.jsonl,data/test.jsonl -o models/e5.jemb
./kling domotica train-encoder -data data/train.jsonl -valid data/valid.jsonl -cache models/e5.jemb \
    -o models/head.jenc -intent models/intent.jev -slots models/slots.jevs
./kling rm enc-train

# el registro: rutas relativas a él; los dorados de este host
sed -i 's#"domotica/#"models/#' ai.json
mkdir -p ~/.config/kling
sudo cp kindling-domotica-gateway.service kindling-domotica.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kindling-domotica-gateway
./kling ai eval room -data data/test.jsonl -socket ~/domotica/ai.sock              # la puerta de la capa 3
./kling ai reload -socket ~/domotica/ai.sock
KLING_DOMOTICA_MODELS=~/domotica/models ./kling domotica eval-llm \
    -gateway ~/domotica/ai.sock -llm-task room-llm -decide-task room -data data/test.jsonl  # la de la capa 4 (~20 min)
sudo systemctl enable --now kindling-domotica                                      # lee el registro al arrancar
```

Los dos servicios: [`kindling-domotica-gateway.service`](kindling-domotica-gateway.service)
y [`kindling-domotica.service`](kindling-domotica.service). La demo escucha en
`0.0.0.0:8088` a propósito: **http://192.168.2.61:8088** desde cualquier
navegador de la red de casa. Solo mueve dispositivos simulados, pero cada
orden puede despertar una microVM: no la publiques fuera de la LAN. El socket
del daemon y el del gateway no salen del host (0600).

## Seguridad

- La página no carga nada de fuera (CSP `default-src 'self'`); todo lo que
  viene del usuario o del LLM entra con `textContent`.
- Cuerpos acotados (4 KiB), JSON estricto, órdenes de 300 caracteres como
  mucho, una orden a la vez (429 si hay otra), 32 visores SSE como mucho.
- En loopback rechaza cualquier `Host` que no sea de loopback (DNS
  rebinding); los POST exigen `application/json` (un formulario de otra web no
  puede mandarlo sin preflight).
- El token del gateway por TCP sale de un fichero 0600 o de
  `$KLING_AI_TOKEN`, nunca de la línea de comandos.

## Piezas

| fichero | qué |
|---|---|
| `main.go` | flags, cliente del gateway, puerta de la capa 4, panel de máquinas (pkg/api) |
| `room/sim.go` | el simulador de dispositivos y lo que hace cada intención |
| `room/server.go` | API JSON + SSE (`/api/state`, `/api/command`, `/api/events`, `/api/machines`) |
| `room/web/` | la página: plano en SVG, traza por capas, números; es/en, claro/oscuro, accesible |
| `room/presets.json` | los botones de órdenes (los «direct» son plantillas; un test lo comprueba) |
| `ai.json` | registro de ejemplo del gateway: tarea `room` (capas 1–3) y `room-llm` (capa 4, con el prompt y el esquema de `pkg/domotica`; un test lo comprueba) |
