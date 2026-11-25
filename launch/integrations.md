# Integraciones con frameworks de agentes

Investigación del 25-nov-2026. Hay dos niveles de evidencia:

- **Código leído:** se descargó el paquete publicado (wheel de PyPI o tarball de npm) y se leyó qué
  llamadas del SDK de E2B u OpenSandbox hace. Es lo que decide si basta con cambiar variables de
  entorno. Se cita el paquete, la versión y el módulo.
- **Doc o búsqueda:** sale de la documentación o de resultados de búsqueda; las páginas no se
  pudieron abrir directamente desde esta sesión.

Los enlaces a ficheros de GitHub se construyeron a partir de la ruta dentro del paquete y del
`Project-URL` de sus metadatos. La ruta exacta en el repo (sobre todo el prefijo `src/` o
`libs/`) está **sin verificar**.

**Nada de esto se ha ejecutado contra kindling.** «Sí (a probar)» quiere decir que el framework
solo usa llamadas que el shim declara soportadas (`docs/e2b.md`, `docs/opensandbox.md`).
Hace falta una prueba real antes de anunciar «works with X». En paralelo se está verificando el
shim de E2B con el SDK real.

## Lo que decide «funciona solo con variables de entorno»

1. **Los SDK de E2B leen `E2B_DOMAIN`, `E2B_API_KEY`, `E2B_API_URL`, `E2B_SANDBOX_URL` y `E2B_DEBUG`**
   (código leído: `e2b` 2.64.0, `e2b/connection_config.py`; el SDK de JS 2.64.0 lee las mismas).
   **`E2B_API_URL` y `E2B_SANDBOX_URL` están marcadas como obsoletas** («will be removed in the next
   major version», mismo fichero). Solo sirven para pruebas locales. Un «works with» que dure
   tiene que usar `E2B_DOMAIN` con DNS comodín y TLS (`docs/e2b.md`, «En serio»).
2. **`e2b_code_interpreter` no funciona con el shim.** `Sandbox.run_code()` hace `POST /execute`
   al puerto 49999 (Jupyter), con la cabecera `E2b-Sandbox-Port: 49999` (código leído:
   `e2b_code_interpreter` 2.11.10, `code_interpreter_async.py`). El shim
   (`ext/sandbox/internal/e2b/e2b.go`) no tiene esa ruta y devuelve 404. Lo que solo usa
   `commands.run` y `files.*` del paquete `e2b` funciona; lo que usa `run_code` no.
3. **Para OpenSandbox basta `OPEN_SANDBOX_DOMAIN` y `OPEN_SANDBOX_API_KEY`** (código leído:
   `opensandbox-mcp` 1.2.1, `pydantic-ai-harness` 0.74.0) y un `-image-map` en el shim
   (`docs/opensandbox.md`).

**La palanca más grande está en kindling, no en otro repo:** que el shim de E2B traduzca
`POST /execute` (y `/contexts`) del puerto 49999 al `/code` del gateway, que ya existe y es
experimental (`docs/sandbox-api.md`). Con eso funcionarían sin tocar nada smolagents
(`executor_type="e2b"`), `E2BPythonTool` de CrewAI, `run_python_code` de Agno, la herramienta
E2B de Flowise, el plugin E2B de Dify y cualquier código que use `@e2b/code-interpreter`. El
protocolo de `/execute` es un stream de líneas JSON con eventos `result`, `stdout`, `stderr`,
`error` y `number_of_executions` (código leído: `e2b_code_interpreter/models.py`). También
están `/contexts`, `/contexts/{id}` y `/contexts/{id}/restart` (`code_interpreter_async.py`).
Falta compararlo con `/code` campo a campo (**sin verificar**).

## Tabla de frameworks

Orden: primero lo que llega a más gente con menos esfuerzo.

| Framework | Cómo usa E2B/OSB (evidencia) | ¿Basta con variables de entorno? | ¿PR? | Configuración | Fichero relevante |
|---|---|---|---|---|---|
| **OpenAI Agents SDK** (Python) | `E2BSandboxClient` con `sandbox_type="e2b"` usa el paquete `e2b`: `commands.run`, `files.*`, `pty.create`, `pause`, `get_host`. Con `"e2b_code_interpreter"` usa ese paquete. Por defecto `on_timeout="pause"` y la persistencia es `tar` por comandos. (Código leído: `openai-agents` 0.26.0, `agents/extensions/sandbox/e2b/sandbox.py`.) Proveedores integrados: Blaxel, Cloudflare, Daytona, E2B, Modal, Runloop, Vercel ([OpenAI, 15-abr-2026](https://openai.com/index/the-next-evolution-of-the-agents-sdk/)). | **Sí (a probar)** con `sandbox_type="e2b"`. Las herramientas que abren PTY fallan: el shim da 501 a `pty`. | Opcional: un cliente nativo (ver PR 1). | `E2B_DOMAIN=e2b.midominio E2B_API_KEY=<token>`; `E2BSandboxClientOptions(sandbox_type="e2b", template="base")` | [`src/agents/extensions/sandbox/e2b/sandbox.py`](https://github.com/openai/openai-agents-python/blob/main/src/agents/extensions/sandbox/e2b/sandbox.py); interfaz [`sandbox/session/sandbox_client.py`](https://github.com/openai/openai-agents-python/blob/main/src/agents/sandbox/session/sandbox_client.py) (`BaseSandboxClient`) |
| **LangChain Deep Agents** | `langchain-e2b` 0.0.7 (de E2B) usa `e2b.Sandbox.create/connect`, `commands.run`, `files.get_info/read/write`. (Código leído: `langchain_e2b/sandbox.py`, `provider.py`.) Proveedores con paquete propio: Daytona, Modal, Runloop; E2B como tercero ([docs](https://docs.langchain.com/oss/python/deepagents/sandboxes.md)). También existe el comunitario `langchain-opensandbox` 0.1.0. | **Sí (a probar)** por las dos vías: E2B y OSB. | Opcional: paquete partner (PR 2). | `E2B_DOMAIN`, `E2B_API_KEY`; `E2BProvider` de `langchain_e2b` | [`langchain_e2b/provider.py`](https://github.com/e2b-dev/langchain-e2b); interfaz [`libs/deepagents/deepagents/backends/sandbox.py`](https://github.com/langchain-ai/deepagents) (`BaseSandbox`: `execute`, `id`, `upload_files`, `download_files`); partners en `libs/partners/<nombre>` (`Project-URL` de `langchain-daytona`) |
| **PydanticAI (harness)** | Trae `OpenSandbox` (lee `OPEN_SANDBOX_DOMAIN` y `OPEN_SANDBOX_API_KEY`) y `E2BSandbox` (paquete `e2b`: `commands.run`, `files.list/get_info/read/write/exists/remove/make_dir`, con `user=`, que el shim ignora). Hay más de diez backends: boxd, boxlite, daytona, docker, e2b, kubernetes, modal, opensandbox, render, smol, sprites, ssh. (Código leído: `pydantic-ai-harness` 0.74.0.) | **Sí (a probar)** por las dos vías. La de OSB es la más directa. | Opcional: backend propio (PR 3). | `OPEN_SANDBOX_DOMAIN=http://shim:8080 OPEN_SANDBOX_API_KEY=<token>`; `OpenSandbox(image="python:3.12")` con `-image-map python:3.12=py` en el shim | [`pydantic_ai_harness/opensandbox_sandbox/_backend.py`](https://github.com/pydantic/pydantic-ai) |
| **Mastra** (TS) | `@mastra/e2b` 0.18.1 usa el paquete `e2b` (`commands.run` en 43 sitios; `files.list/makeDir/read/remove/write`) y acepta la opción `domain`. Tiene montajes por FUSE (gcsfuse, s3) que ejecutan comandos. (Código leído: el tarball de npm.) | **Sí (a probar)**; los montajes FUSE dependen de la plantilla. | No | `E2B_DOMAIN`, `E2B_API_KEY`, o `new E2BSandbox({ domain })` | [`workspaces/e2b`](https://github.com/mastra-ai/mastra/tree/main/workspaces/e2b) (de `package.json` → `repository.directory`) |
| **Vercel AI SDK 7** | `@e2b/ai-sdk-sandbox` 0.3.0 implementa la interfaz de sandbox de AI SDK 7 con `commands.run` y `files.read/write`. (Código leído; la interfaz viene de [docs de E2B](https://docs.e2b.dev/agents/vercel-ai-sdk.md).) | **Sí (a probar)** | No. Un `@kindling/ai-sdk-sandbox` propio tampoco necesitaría PR. | `E2B_DOMAIN`, `E2B_API_KEY` | [e2b-dev/ai-sdk-sandbox](https://github.com/e2b-dev/ai-sdk-sandbox) |
| **AG2** (sucesor de AutoGen) | `ag2.extensions.e2b.E2BSandbox` usa `AsyncSandbox.create`, `commands.run`, `files.write/remove/make_dir` y pasa `domain` y `api_key`. (Código leído: `ag2` 1.3.4.) | **Sí (a probar)** | No | `E2B_DOMAIN`, `E2B_API_KEY` | [`ag2/extensions/e2b/sandbox.py`](https://github.com/ag2ai/ag2) |
| **CrewAI** | `E2BExecTool` y `E2BFileTool` usan `e2b` y aceptan `domain` o `E2B_DOMAIN`. `E2BPythonTool` usa `e2b_code_interpreter.run_code`. (Código leído: `crewai-tools` 1.15.45, `tools/e2b_sandbox_tool/`.) | **En parte:** shell y ficheros sí (a probar); Python **no** mientras falte `/execute`. `files.list` va por ListDir; `exists` por Stat. | No (o el `/execute` en kindling) | `E2B_DOMAIN`, `E2B_API_KEY` | [`crewai_tools/tools/e2b_sandbox_tool/`](https://github.com/crewAIInc/crewAI) |
| **Agno** | `E2BTools` crea el sandbox con `e2b_code_interpreter.Sandbox`. `run_python_code` usa `run_code`; también usa `commands.run`, `files.*` y `files.watch_dir`. (Código leído: `agno` 3.2.1, `agno/tools/e2b.py`.) | **En parte:** comandos y ficheros sí (a probar); `run_python_code` no; `watch_dir` da 501. | No (o el `/execute`) | `E2B_DOMAIN`, `E2B_API_KEY`; `E2BTools(include_tools=[...])` | [`agno/tools/e2b.py`](https://github.com/agno-agi/agno) |
| **smolagents** (HF) | `E2BExecutor` usa `e2b_code_interpreter.Sandbox.run_code`. `executor_type` acepta `local`, `blaxel`, `e2b`, `modal` y `docker`, y el parámetro `executor=` acepta un `PythonExecutor` propio. (Código leído: `smolagents` 1.26.0, `remote_executors.py`, `agents.py`.) | **No** mientras falte `/execute`. Con él, sí. | No, si se añade `/execute`. Para un `executor_type="kindling"` sí hace falta PR. | `E2B_DOMAIN`, `E2B_API_KEY`; `CodeAgent(..., executor_type="e2b")` | [`src/smolagents/remote_executors.py`](https://github.com/huggingface/smolagents) |
| **mini-swe-agent** (SWE-bench) | `environment_class` acepta una ruta con puntos a cualquier clase. El protocolo `Environment` pide `execute(action, cwd, timeout)`, `get_template_vars` y `serialize`. (Código leído: `mini-swe-agent` 2.4.6, `environments/__init__.py`.) | No hay backend E2B. Con una clase dentro de `kindling-sandbox`, **no hace falta PR**. | Opcional: alias `"kindling"` en `_ENVIRONMENT_MAPPING`. | `environment_class: kindling_sandbox.integrations.minisweagent.KindlingEnvironment` | [`src/minisweagent/environments/__init__.py`](https://github.com/SWE-agent/mini-swe-agent) |
| **SWE-ReX / SWE-agent** | Despliegues: local, docker, modal, fargate, remote, dummy y daytona (WIP). No hay E2B. La interfaz es `AbstractDeployment` (`start`, `stop`, `is_alive`, `runtime`). (Código leído: `swe-rex` 1.4.0.) | **No** | Sí: `KindlingDeployment` y su `Config` (`type: "kindling"`). Otra opción: arrancar `swerex-remote` dentro de una microVM y usar `RemoteDeployment` sin PR. | — | [`src/swerex/deployment/abstract.py`](https://github.com/SWE-agent/SWE-ReX) |
| **OpenHands** (V1, software-agent-sdk) | Workspaces: Docker, Apptainer, Cloud, `APIRemoteWorkspace` (la «runtime API» de OpenHands: `/start`, `/pause`, `/resume`, `/sessions/{id}`, cabecera `X-API-Key`) y `AgentSandboxWorkspace` (kubernetes-sigs/agent-sandbox). No hay E2B en V1. (Código leído: `openhands-workspace` 1.64.1.) El runtime E2B era de V0 ([docs V0](https://docs.openhands.dev/openhands/usage/v0/runtimes/V0_e2b)). | **No**, salvo que kindling implemente la runtime API. | O un shim `kling sbx openhands` que hable esa API (con él basta `runtime_api_url`), o un PR con `KindlingWorkspace`. Sin nada: imagen `agent-server` en una microVM + `RemoteWorkspace(host=...)`. | `APIRemoteWorkspace(runtime_api_url=..., runtime_api_key=..., server_image=...)` | [`openhands-workspace/openhands/workspace/remote_api/workspace.py`](https://github.com/OpenHands/software-agent-sdk) |
| **Claude Managed Agents** | «Self-hosted sandboxes» en beta pública: un entorno `self_hosted` es una cola de trabajo y un *worker* tuyo la consume y ejecuta las herramientas en tu infraestructura ([docs de Anthropic](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes); Cloudflare tiene una implementación de referencia: [cloudflare/claude-managed-agents](https://github.com/cloudflare/claude-managed-agents)). (Doc/búsqueda.) | No aplica: hay que escribir un worker. | No es un PR: es un producto. Un `kling sbx claude-worker` que lance una microVM por sesión. Los detalles del protocolo están **sin verificar**. | — | [self-hosted-sandboxes-workers](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-workers.md) |
| **Claude Agent SDK** | No tiene interfaz de proveedor: su guía de hosting enumera proveedores de contenedores y usa Modal como demo ([hosting](https://platform.claude.com/docs/en/agent-sdk/hosting)). (Doc/búsqueda.) | n/a | No. Escribir una receta: el SDK dentro de una microVM con la API key fuera, como en `docs/recetas/claude-code.md`. | — | — |
| **Goose, Cline, Roo, otros clientes MCP** | Goose no trae sandbox; la vía documentada es un servidor MCP de OpenSandbox como extensión ([AAIF](https://aaif.io/blog/give-goose-sandbox-powers-with-opensandbox-and-mcp)). `opensandbox-mcp` 1.2.1 lee `OPEN_SANDBOX_DOMAIN`/`OPEN_SANDBOX_API_KEY` y usa comandos, ficheros, búsqueda, `replace`, métricas y endpoints, todos cubiertos por el shim (código leído). | **Sí (a probar)**: `opensandbox-mcp` contra `kling sbx osb`, o directamente `kling sbx mcp`. | No | `OPEN_SANDBOX_DOMAIN=http://shim:8080 opensandbox-mcp --transport stdio` | — |
| **Dify** | Plugin oficial `langgenius/e2b` v0.7 (Python/JS); según un artículo no admite plantillas propias ([marketplace](https://marketplace.dify.ai/plugin/langgenius/e2b), [qiita](https://qiita.com/nishiemon/items/f1b48d53cac86e928df7)). Dify autoalojado ejecuta los nodos de código en `dify-sandbox`, configurado con `CODE_EXECUTION_ENDPOINT` y `CODE_EXECUTION_API_KEY` ([pigsty](https://pigsty.io/docs/app/dify/)). | El plugin, **sin verificar** (no consta que acepte dominio). La vía de `CODE_EXECUTION_ENDPOINT` funcionaría si kindling implementa la API de `dify-sandbox` (forma **sin verificar**). | Un shim `kling sbx dify` haría que cambiar una variable bastara para todo Dify autoalojado. | `CODE_EXECUTION_ENDPOINT=http://kling-dify:8194` | — |
| **Flowise** | Herramienta «Code Interpreter by E2B» ([docs](https://docs.flowiseai.com/integrations/langchain/tools/python-interpreter)), que usa el intérprete. Hay un issue que pide un nodo de código autoalojado ([#5894](https://github.com/FlowiseAI/Flowise/issues/5894)). | **No** mientras falte `/execute`. | — | — | — |
| **n8n** | Hay una integración E2B listada ([n8n](https://n8n.io/integrations/e2b/)); qué hace está **sin verificar**. | sin verificar | — | — | — |
| **LlamaIndex** | No hay herramienta E2B; la de code interpreter ejecuta en local ([PyPI](https://pypi.org/project/llama-index-tools-code-interpreter/)). | n/a | Podría ser un `llama-index-tools-kindling`, pero llega a menos gente. | — | — |
| **Aider, Open Interpreter** | No se encontró una interfaz de sandbox (**sin verificar**). | n/a | — | — | — |

Configuración común para todo lo que es «Sí (a probar)» por E2B:

```sh
kling sbx e2b -listen 0.0.0.0:443 -domain e2b.example.test -tls-cert c.pem -tls-key k.pem
export E2B_DOMAIN=e2b.example.test      # *.e2b.example.test → el shim
export E2B_API_KEY=<token del inquilino>  # o el KLING_E2B_KEY del shim
export SSL_CERT_FILE=ca.pem               # Python; NODE_EXTRA_CA_CERTS=ca.pem en JS
```

## Las tres primeras

Criterio: cuánta gente las usa, que ya funcionen solo con variables de entorno (la afirmación
«works with X» sale en cuanto pase la prueba) y que el PR sea pequeño y con precedentes.

### 1. OpenAI Agents SDK

- **Ya:** `E2BSandboxClient` con `sandbox_type="e2b"` contra el shim. Prueba mínima: un agente
  que escribe un fichero y lo lee; otra con `pause_on_exit=True`; otra con la persistencia `tar`.
  Hay que anotar qué herramientas usan PTY (el shim da 501) para documentarlo.
- **PR (opcional, después):** `src/agents/extensions/sandbox/kindling/` con `KindlingSandboxClient`,
  `KindlingSandboxClientOptions` y `KindlingSandboxSession`, siguiendo las extensiones que ya
  existen (`blaxel`, `cloudflare`, `daytona`, `e2b`, `modal`, `vercel`), y el extra
  `openai-agents[kindling]` → `kindling-sandbox`.
  - `BaseSandboxClient` en `agents/sandbox/session/sandbox_client.py` y `BaseSandboxSession` en
    `agents/sandbox/session/base_sandbox_session.py` (6 métodos abstractos en 0.26.0).
  - Correspondencia: exec → `POST /v1/sandboxes/{id}/exec`; ficheros → `/files`; pausa →
    `/pause`; snapshot → `freeze` (el gateway no hace snapshots por inquilino: declararlo no
    soportado); puertos expuestos → `/endpoints`.
  - Tests con un gateway simulado, como los de las otras extensiones.
- **Antes de abrir el PR:** que el paquete `kindling-sandbox` esté en PyPI y que haya un issue en
  el repo preguntando si aceptan proveedores nuevos (no abrirlo desde aquí).

### 2. LangChain Deep Agents

- **Ya:** `langchain-e2b` (de E2B) con `E2B_DOMAIN`; o `langchain-opensandbox` contra
  `kling sbx osb`.
- **PR:** paquete partner `langchain-kindling` en `libs/partners/kindling` (es la ruta de
  `langchain-daytona` según sus metadatos), o publicado aparte como hizo E2B con
  `langchain-e2b`. El nombre `langchain-kindling` está libre en PyPI (404 el 25-nov-2026).
  - `KindlingSandbox(BaseSandbox)`: `execute(command, timeout)` → `POST …/exec?wait=1`;
    `upload_files` y `download_files` → `PUT` y `GET …/files`; `id` → el id del gateway.
    `ls`, `read`, `write`, `edit`, `grep` y `glob` ya vienen implementados encima de `execute`
    en `BaseSandbox`.
  - `KindlingProvider(SandboxProvider)` con `metadata` y `get_or_create(sandbox_id=...)`, copiando
    `langchain_e2b/provider.py`, para que la CLI lo acepte como proveedor declarado por
    configuración.
  - Página en las docs de integraciones (`docs.langchain.com/oss/python/integrations/sandboxes`).
- **Por qué segunda:** Deep Agents tiene página de sandboxes con lista de proveedores; salir ahí
  es visibilidad duradera.

### 3. PydanticAI (harness)

- **Ya:** `OpenSandbox` con `OPEN_SANDBOX_DOMAIN` y `OPEN_SANDBOX_API_KEY`, contra `kling sbx osb`
  con `-image-map`. Es la que menos configuración pide: no necesita DNS comodín. También
  `E2BSandbox` por `E2B_DOMAIN`.
- **PR:** `pydantic_ai_harness/kindling_sandbox/` con `KindlingSandbox` (la capability) y
  `KindlingSandboxBackend(WorkspaceBackend, SupportsCommands, SupportsFilesystem)`, el extra
  `pydantic-ai-harness[kindling]`, el README de la carpeta y una fila en la tabla del README
  principal («Commands and files in a microVM on your own kindling host»).
  - Precedente: el harness ya acepta backends de proyectos pequeños (`boxd_sandbox`,
    `boxlite_sandbox`, `smol_sandbox`); la tabla del README los lista todos.
  - Prueba en vivo: `smol_sandbox` tiene un nivel `TestLive…` que corre en `ubuntu-latest` con
    KVM (lo dice el docstring de `_smolvm_cli.py`). Uno equivalente para kindling demostraría
    que funciona de verdad.

### Después de las tres

- **`/execute` en el shim de E2B** (trabajo en kindling): activa smolagents, CrewAI Python, Agno,
  Flowise y el plugin de Dify sin PR.
- **mini-swe-agent:** `KindlingEnvironment` dentro de `kindling-sandbox`, que se usa con la ruta
  con puntos. Llega a quien corre SWE-bench sin tener que convencer a nadie.
- **Claude Managed Agents:** un worker `self_hosted` que lance una microVM por sesión. Es nuevo
  (beta pública) y casi no tiene implementaciones autoalojadas aparte de la de Cloudflare.
