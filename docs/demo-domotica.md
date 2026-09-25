# La habitación de demo

La demo de producto de kindling es una habitación manejada con órdenes de voz
(como texto, en español o inglés) cuyas decisiones toman modelos que viven en
microVMs **serverless**: congelados a coste cero, descongelados en
milisegundos por la orden que los necesita y congelados otra vez al quedarse
ociosos. Es un ejemplo que usa kindling, con su propio `main`:
[examples/domotica](../examples/domotica/README.md) (cómo montarla en un Mac o
en el servidor x86, sus servicios de systemd y sus flags).

## Qué enseña

- Un plano en SVG con luces por zona (brillo y color), termostato (actual y
  objetivo, que se mueve solo), persianas, tele, altavoz, cerradura, alarma,
  ventilador y enchufe. El estado vive en un simulador en Go y llega por SSE.
- Botones con órdenes de ejemplo agrupadas por la capa que se espera que las
  decida (directas, paráfrasis, indirectas y dobles, fuera de la habitación) y
  un cuadro de texto libre.
- Por cada orden: la animación del dispositivo, y una **traza por capas**
  (plantillas → Chispa → codificador → LLM) con lo que dijo cada una, su
  confianza, su latencia (µs, ms o s), si su microVM estaba congelada y se
  descongeló (y en cuánto) y la acción en JSON; la frase que contestaría.
- El **panel de microVMs** de cada capa (despierta o congelada, memoria) y
  contadores: decisiones por capa, p50 de latencia, despertares.
- Español con cambio a inglés, claro u oscuro según el sistema, usable con
  teclado y lector de pantalla, y en el móvil.

## Cómo está hecha

```
navegador ──HTTP/SSE──> examples/domotica ──POST /v1/decide────> kling ai serve
                         room/: simulador,     (capas 1–3)          ├─ plantillas + Chispa: en su proceso (µs)
                         API JSON + SSE,     ──POST /v1/generate──>  ├─ codificador: réplica de un dorado kind embed (ms)
                         página embebida        (capa 4)            └─ LLM: réplica de un dorado VON (s)
                               │             ──GET /metrics──────>     (despertares por modelo: thaw / restore)
                               └──── pkg/api: GET /machines, /procstats ──> daemon de kindling
```

- La demo **no carga modelos**: pregunta al gateway (capas 1–3 en
  `/v1/decide`, la 4 en `/v1/generate`) y valida la respuesta del LLM contra
  la taxonomía antes de ejecutar nada (`pkg/domotica`: `ParseLLM`, `Veto`).
  Con `-offline` sirve las capas 1 y 2 en su proceso, como reserva.
- Qué microVM despertó cada orden sale de comparar `/metrics` del gateway
  antes y después (`kling_ai_von_wake_seconds` por modelo y cómo): con una
  orden a la vez, la atribución es exacta.
- La capa 4 solo decide si su evaluación la respalda, y con el alcance que
  respalda ([DOMOTICA-EVAL.md](DOMOTICA-EVAL.md#capa-4-el-llm-von)): con
  Qwen2.5-1.5B, solo lo que Chispa duda. La traza lo dice («LLM saltada» en lo
  que Chispa da por fuera de ámbito).
- Seguridad: escucha en loopback por defecto (y rechaza un `Host` ajeno);
  `-listen 0.0.0.0:8088` para la LAN. Cuerpos de 4 KiB, JSON estricto, una
  orden a la vez, CSP sin nada de fuera; el socket del daemon y el del gateway
  no salen del host.
