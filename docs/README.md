# Documentación de kindling

Punto de entrada: el [README](../README.md) ([English](../README.md) ·
[Español](../README.es.md)), que es la versión corta. Aquí está lo que no cabe en él: las
guías paso a paso, el manual largo con cada medida, las auditorías con números y las notas
de campo que evitan repetir horas de depuración.

## ¿Qué guía necesito?

Las guías están en inglés ([`guides/`](guides/)); son copiables y llevan la salida esperada.

| Quiero… | Guía |
|---|---|
| Probar kindling en una máquina Linux con KVM | [Quickstart on Linux](guides/quickstart-linux.md) |
| Correr microVMs en mi Mac con Apple Silicon | [Quickstart on macOS (vz)](guides/quickstart-macos.md) |
| Tener el daemon en un servidor y manejarlo desde el portátil | [Remote daemon over SSH](guides/remote-daemon-ssh.md) |
| Alojar servidores MCP para Claude Code, opencode o Cursor | [Host MCP servers](guides/mcp-servers.md) |
| Que un agente ejecute código sin tocar mi máquina | [Safe sandboxes for AI agents](guides/sandboxes-for-agents.md) |
| Clasificar en microsegundos y servir un LLM pequeño con escala a cero | [Serverless AI](guides/serverless-ai.md) |
| Pedir sandboxes con `kubectl apply` | [Kubernetes operator](guides/kubernetes-operator.md) |
| Añadir un subcomando a `kling` | [Writing an extension](guides/writing-an-extension.md) |
| Arreglar algo que sale en rojo | [Troubleshooting](guides/troubleshooting.md) |
| Saber en qué se diferencia de E2B, Daytona, microsandbox, ToolHive… | [`compare.md`](compare.md) |
| Ver enlaces a benchmarks, ejemplos, releases y comunidad | [`resources.md`](resources.md) |

## El manual

| Documento | Qué cubre |
|---|---|
| [`handbook.md`](handbook.md) · [`handbook.es.md`](handbook.es.md) | La versión larga del README: el CLI con transcripciones, snapshots dorados, red, sandboxes, extensiones, volúmenes, carpetas compartidas, VON, Chispa, el gateway de IA, rendimiento y densidad, seguridad, operación, requisitos y scripts. Cada número con su medida |
| [`api.md`](api.md) | Todas las rutas del daemon, las capacidades, el protocolo de los constructores de imágenes y el proxy al invitado |
| [`extensions.md`](extensions.md) | El manifiesto campo a campo (con `companions`), descubrimiento, ganchos, publicación |
| [`../ext/mcp/README.md`](../ext/mcp/README.md) ([es](../ext/mcp/README.es.md)) · [`../ext/sandbox/README.md`](../ext/sandbox/README.md) | Las extensiones oficiales: servidores MCP bajo demanda y sandboxes multiinquilino |

## Por tema

| Documento | Qué cubre | Léelo si… |
|---|---|---|
| [`mac.md`](mac.md) | kindling nativo en macOS con el backend `vz`: requisitos, instalación, launchd, cómo se llega a cada máquina y los límites frente a Linux | vas a correr el daemon en un Mac |
| [`backend-vz.md`](backend-vz.md) · [`vz-mac-prototipo.md`](vz-mac-prototipo.md) | El contrato entre el daemon y `kling-vz`, y las medidas del prototipo que justifican el diseño | vas a tocar el backend de macOS |
| [`mac-arm64.md`](mac-arm64.md) | La otra opción en Mac: una VM Lima con virtualización anidada (M3+), sus límites (~16 s de arranque en frío) y las palancas medidas para bajarlo a ~2,5 s | prefieres Firecracker de verdad dentro de una VM Linux en el Mac |
| [`exec-sandbox.md`](exec-sandbox.md) | Sandboxes para agentes: `kling sandbox`, `exec` en streaming, `cp`, plantillas desde snapshot y la puerta `allow_exec` | quieres ejecutar código de un agente sin tocar tu máquina |
| [`compartir.md`](compartir.md) | Carpetas del host dentro de una máquina: copia, `ro`, `rw`; FUSE propio, `os.Root`, límites y modelo de amenaza | vas a usar `-share` |
| [`kubernetes.md`](kubernetes.md) | `kindling-operator`: sandboxes gestionados desde Kubernetes, con su CRD, su `status` y lo que NO es | quieres pedir sandboxes con `kubectl` |
| [`von.md`](von.md) · [`von-cpu.md`](von-cpu.md) | LLM pequeños servidos desde dorados con `kling ai model`: uso, diseño, semillas, cifras en Linux (i7-8700T sin anidar) y macOS, y el plan de GPU | quieres servir un modelo pequeño con escala a cero |
| [`chispa.md`](chispa.md) · [`CHISPA-EVAL.md`](CHISPA-EVAL.md) · [`chispa-serverless.md`](chispa-serverless.md) | Chispa: características hasheadas, pesos int16, calibración, formato `.chispa`; su evaluación con 4 304 commits; y Chispa como tarea serverless con thaw medido | quieres clasificar o enrutar en microsegundos |
| [`ai-gateway.md`](ai-gateway.md) · [`intent.md`](intent.md) · [`codificador.md`](codificador.md) · [`mejora-continua.md`](mejora-continua.md) | El gateway de IA: Chispa clasifica, VON genera, la cascada solo con evaluación que la respalde; tareas de intención con huecos; el codificador de frases como capa 3; y el ciclo revisar → etiquetar → reentrenar → promover | vas a poner modelos detrás de una sola API |
| [`despertar.md`](despertar.md) | De réplica dormida a primera respuesta, fase por fase: 152 → 27 ms congelada y 2,2 ms pausada | quieres saber dónde se va el tiempo de un thaw |
| [`three-layers.md`](three-layers.md) | Imágenes por capas: base por familia de runtime + capa de servicio + overlay; 1300 → 433 MiB | quieres entender `-base` y las familias `node`/`python` |
| [`densidad-zram.md`](densidad-zram.md) | Swap comprimido en RAM para densificar el host: cuándo ayuda y cómo medirlo | quieres más microVMs sin más RAM |
| [`estabilidad.md`](estabilidad.md) | La auditoría de estabilidad: el commit prematuro, seis fallos de robustez, el hasheo que costaba el 67 % del despertar, 142 microVMs en 3,9 GB | quieres saber por qué v0.4 es 9,5× más rápida bajo carga |
| [`cifrado.md`](cifrado.md) | Cifrado en reposo: es cosa del disco, no de kindling; la receta con dm-crypt | te importa quién puede leer la memoria de un snapshot |
| [`domotica.md`](domotica.md) · [`domotica-datos.md`](domotica-datos.md) · [`DOMOTICA-EVAL.md`](DOMOTICA-EVAL.md) · [`demo-domotica.md`](demo-domotica.md) | El ejemplo de domótica (`examples/domotica`, un programa aparte): capas, datos libres y licencias, evaluación, y la habitación de demo | vas a tocar la demo o la cascada de decisión |
| [`CI-TRIAGE-EVAL.md`](CI-TRIAGE-EVAL.md) | El triaje de fallos de CI de `examples/ci-triage`: Chispa localiza y categoriza, VON solo lee el trozo | quieres usar Chispa sobre logs |
| [`hallazgos.md`](hallazgos.md) | Notas de campo: overlays, ficheros dispersos, namespaces, cgroups, trampas de medición | algo se comporta raro y sospechas que ya le pasó a alguien |
| [`releases.md`](releases.md) | Una etiqueta, una release: todos los assets, el `SHA256SUMS`, los módulos y cómo crear una release | mantienes el proyecto o compilas desde fuentes |
| [`archivo-repos.md`](archivo-repos.md) · [`RELEASE-v0.3.0.md`](RELEASE-v0.3.0.md) · [`RELEASE-v0.2.0.md`](RELEASE-v0.2.0.md) | Historia: la fusión de los repos viejos y las notas de las primeras releases | quieres el contexto |

## Imágenes

[`img/`](img/): diagramas SVG (arquitectura, ciclo de vida, gateway MCP, cascada de IA)
legibles en claro y oscuro, y las demos de terminal grabadas con `vhs` contra un daemon
real; cada `.gif` va con su `.tape` para volver a grabarla.

## Seguridad y contribuir

- [`../SECURITY.md`](../SECURITY.md): el modelo de amenaza, las barreras y lo que NO está
  resuelto; cómo avisar de una vulnerabilidad.
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md): cómo compilar, probar y proponer cambios.
- [`../CHANGELOG.md`](../CHANGELOG.md): todas las versiones.
