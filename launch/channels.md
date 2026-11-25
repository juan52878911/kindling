# Canales de distribución

Investigación del 25-nov-2026. Cada regla lleva su fuente; lo que no se pudo comprobar en la
fuente primaria va marcado **sin verificar**. Desde esta sesión `WebFetch` no podía abrir la
mayoría de webs (solo había búsqueda), así que varias reglas salen de copias, espejos o
resúmenes de buscador: antes de abrir cada PR hay que leer el CONTRIBUTING vivo.

Fechas que mandan sobre varios canales:

- **Primera release:** v0.1.0 del 2026-08-08 (`CHANGELOG.md`). Eso pone la regla de 4 meses de
  awesome-selfhosted en **2026-12-08** y la de 5 meses de awesome-go en **2027-01-08**.
- **Los SDKs propios no están publicados:** `kindling-sandbox` en PyPI y `@kindling/sandbox` en
  npm dan 404 (comprobado contra pypi.org y registry.npmjs.org el 25-nov-2026). Todavía no se
  sabe si el scope `@kindling` de npm está libre (**sin verificar**). Un usuario que llega desde
  una lista y no puede hacer `pip install` se va, así que publicarlos va antes que casi todo lo
  demás.
- **Daytona pasó a código cerrado** el 11-jun-2026 ([anuncio de Daytona](https://daytona.io/dotfiles/updates/daytona-is-going-closed-source);
  el fork comunitario [Nightona](https://cdn.jsdelivr.net/gh/nightona-co/nightona@main/README.md)
  sigue desde v0.190.0). Hoy hay gente buscando «una alternativa abierta y autoalojable a
  Daytona». Esa es la oportunidad en openalternative.co y alternativeto.net.

## Tabla priorizada

Esfuerzo: S = menos de 1 h, M = medio día, L = varios días. Alcance: estimación cualitativa (alto,
medio, bajo) de cuántos usuarios llegarían, sin datos de tráfico detrás.

| # | Canal | Esfuerzo | Alcance | Reglas (fuente) | Siguiente acción exacta |
|---|---|---|---|---|---|
| 1 | **PyPI y npm** (`kindling-sandbox`, `@kindling/sandbox`) | M | alto (lo necesitan todos los demás canales) | El registro oficial MCP usa `mcp-name:` en el README (PyPI) o `mcpName` en `package.json` (npm) para verificar la propiedad ([MCP registry, autenticación](https://modelcontextprotocol.io/registry/authentication)) | Comprobar los nombres, publicar 1.0.0-rc con Trusted Publishing desde `release.yml` y añadir `mcp-name: io.github.juan52878911/kindling-sandbox` al README del paquete de Python. |
| 2 | **Show HN** | S para preparar, más un día atento a los comentarios | alto, en un solo pico | La guía oficial no apareció en las búsquedas; las guías de terceros piden el título con `Show HN:`, algo que se pueda probar sin registro, el repo enlazado y el autor respondiendo ([daily.dev](https://business.daily.dev/resources/hacker-news-marketing-developer-tools-show-hn-launch-day-sustained-coverage/)). Las reglas oficiales están en `news.ycombinator.com/showhn.html` (**sin verificar** el texto actual). | Publicar cuando salga v1.0.0, un martes o miércoles por la mañana (hora de EE. UU.), con `kling try -- uname -a` como demo de 30 segundos y la tabla contra OSB 1.2.1 con sus avisos. Título: «Show HN: kindling – microVMs que despiertan de un snapshot, con APIs de E2B y OpenSandbox». |
| 3 | **Registro oficial MCP** (alimenta a Glama, PulseMCP y otros) | M | medio-alto | El login con GitHub solo da el namespace `io.github.<usuario>/*`; `mcp-publisher login github` no publica en namespaces de organización ([autenticación](https://modelcontextprotocol.io/registry/authentication), [quickstart](https://modelcontextprotocol.io/registry/quickstart)). Necesita un paquete verificable (npm, PyPI, OCI o MCPB). Varias guías dicen que Smithery, Glama y PulseMCP leen de ahí ([tallyfy](https://tallyfy.com/how-to-list-mcp-server-registry-smithery-glama-pulsemcp/), **sin verificar**). | Elegir el paquete: un `.mcpb` adjunto a la release de GitHub con `kling sbx mcp`, o el paquete de PyPI del punto 1 con un entrypoint. Escribir `server.json` (borrador abajo) y publicar desde CI con `mcp-publisher login github-oidc` ([GitHub Actions](https://modelcontextprotocol.io/registry/github-actions)). |
| 4 | **punkpeye/awesome-mcp-servers** | M (pide Glama primero) | alto en el nicho MCP | Según forks y commits espejo, el servidor tiene que estar en Glama y pasar sus comprobaciones (incluido un Dockerfile), y la entrada lleva un badge de puntuación de Glama; hay un workflow `check-glama` ([espejo de commits](https://git.cynarski.dev/Awesome/awesome-mcp-servers/commit/9757dd977515db7363cc7dc00dad2ba1eb3832f5); **sin verificar** contra el upstream). Leyenda: 🏎️ Go, 🏠 servicio local, ☁️ nube, 🍎/🐧/🪟 SO ([README](https://glama.ai/mcp/servers/@punkpeye/awesome-mcp-servers/blob/main/README.md)). | Dar de alta el repo en `glama.ai/mcp/servers`, añadir el Dockerfile que pida Glama y abrir el PR con las dos entradas de abajo: `kling sbx mcp` en ejecución de código y `kling mcp` en agregadores. |
| 5 | **Listas de sandboxes para agentes** | S cada una | medio, y es justo el público de E2B y OSB | [tizkovatereza/awesome-ai-sandboxes](https://github.com/tizkovatereza/awesome-ai-sandboxes): «cloud sandbox providers», cada afirmación enlazada a la doc del proveedor, con campos BYOC/self-host, licencia y precio. [restyler/awesome-sandbox](https://awesome.ecosyste.ms/projects/github.com%2Frestyler%2Fawesome-sandbox): guía con matriz de características. [fishman/awesome-agent-sandbox](https://github.com/fishman/awesome-agent-sandbox): microVMs, contenedores, harnesses. [fhiltscher/awesome-ai-coding-sandboxes](https://osrepos.com/repo/fhiltscher-awesome-ai-coding-sandboxes): ordena por aislamiento, egress y secretos (CC0). El CONTRIBUTING de cada una está **sin verificar**. | Abrir un PR por lista, en su formato y con cada afirmación enlazada a `docs/benchmarks.md`, `SECURITY.md` o `docs/e2b.md`. En la de fhiltscher, destacar el egress por SNI y las recetas que dejan la API key fuera de la VM. |
| 6 | **openalternative.co** | S | medio (SEO, «alternativa abierta a X») | Gratis con cuenta. Pide nombre, web, repo y el producto propietario al que reemplaza. Debe ser open source, mantenido y en inglés; hay cola gratuita y pagos para saltarla ([ideaproof](https://ideaproof.io/launch-directories/open-alternative), [nicklaunches](https://nicklaunches.com/resources/ai-directories/openalternative); de terceros). Hay una categoría «AI sandboxes» ([openalternative](https://openalternative.co/tags/ai-sandboxes)). | Enviarlo como alternativa a **E2B** y a **Daytona**, categoría AI sandboxes, con la línea de abajo. |
| 7 | **alternativeto.net** | S | medio (búsquedas «E2B alternative») | Lo envían los usuarios con cuenta; las reglas de moderación están **sin verificar** (no se pudo abrir la web). | Crear la ficha de kindling como alternativa a E2B, Daytona, Modal y OpenSandbox, con las etiquetas Open Source, Self-Hosted, Linux y Mac. |
| 8 | **Docs de los frameworks** («works with X») | M-L | alto y duradero | Ver `launch/integrations.md`. | Empezar por las tres que funcionan solo con variables de entorno (OpenAI Agents SDK, Deep Agents con `langchain-e2b`, PydanticAI con OpenSandbox), probarlas de verdad y publicar una página por integración en `docs/`. |
| 9 | **infracloudio/awesome-microvm** (no existe un «awesome-firecracker») | S | bajo-medio, técnico | Lista de proyectos y recursos de microVM (Firecracker, Kata, firecracker-containerd, Ignite…) ([repo](https://www.github.com/infracloudio/awesome-microvm)); no hay lista awesome-firecracker ([búsqueda](https://awesome.ecosyste.ms/lists/13081)). El CONTRIBUTING está **sin verificar**. | PR con la entrada de abajo en «Projects». Valorar también [myugan/awesome-microvm](https://www.context-awesome.com/list/myugan/awesome-microvm). |
| 10 | **awesome-sysadmin** (awesome-foss) | S | medio | Una entrada es un YAML en `software/` del repo `awesome-sysadmin-data`, hecho desde la plantilla de alta; la primera etiqueta decide la sección; pasa `awesome_lint` ([CONTRIBUTING](https://raw.githubusercontent.com/awesome-foss/awesome-sysadmin-data/master/CONTRIBUTING.md)). Hay una sección «Virtualization» ([README](https://raw.githubusercontent.com/awesome-foss/awesome-sysadmin/master/README.md)). Está mantenida por la misma gente que awesome-selfhosted, así que puede aplicar la misma regla de antigüedad (**sin verificar**). | Preparar el YAML de abajo y enviarlo después del 2026-12-08 por si aplica la regla de 4 meses. |
| 11 | **awesome-selfhosted** | S | alto (comunidad r/selfhosted) | La primera *release* debe tener más de 4 meses; hasta entonces se puede abrir un issue ([CONTRIBUTING, copia](https://notabug.org/awesome-selfhosted/awesome-selfhosted-data/src/master/CONTRIBUTING.md?lang=en-US)). Es una lista de «network services and web applications», así que encaja el gateway `kling sbx` o el gateway MCP, no el core (**sin verificar**). Se retira el software sin actividad en 6-12 meses. | **A partir del 2026-12-08**, PR al repo `awesome-selfhosted-data` con el YAML de abajo. |
| 12 | **awesome-go** | M (exige informes de calidad) | medio | Unos 5 meses de historia, licencia abierta, enlaces a pkg.go.dev, Go Report Card y cobertura en el PR ([resumen DeepWiki](https://deepwiki.com/avelino/awesome-go/2.2-quality-standards); **sin verificar** la cifra exacta de cobertura). | **A partir del 2027-01-08**, comprobar que `github.com/juan52878911/kindling/pkg/...` se ve bien en pkg.go.dev y tiene nota en Go Report Card, y abrir el PR con la línea de abajo. |
| 13 | **Product Hunt** | M | medio, en un solo pico | Su guía oficial dice «cuando estés listo» ([Product Hunt](https://www.producthunt.com/launch/preparing-for-launch)). Lo de lanzar a las 12:01 AM PT de martes a jueves es consejo de la comunidad ([playbook](https://dev.to/iris1031/product-hunt-launch-the-complete-2026-playbook-for-1-39ai)); un análisis de 2026 sostiene que gana el sábado y se discute ([Databox](https://www.producthunt.com/p/databox/i-ve-analyzed-all-2026-ph-launches-to-find-the-best-day-to-launch-saturday-wins-and-it-s-not-close-2)). | Lanzarlo una o dos semanas después del Show HN, ya con el SDK publicado y una integración «works with» de verdad. Un miércoles. |
| 14 | **CNCF landscape** | S | bajo hasta tener tracción | Los proyectos cloud native con unas **300 estrellas** que encajan en una categoría existente «generally» entran; se añaden por PR a `landscape.yml` con logo SVG; se retiran los proyectos sin commits en 3 meses ([cncf/landscape](https://github.com/cncf/landscape)). | **Esperar a 300 estrellas** (hoy hay unas 53). Entonces, PR en «Runtime → Container Runtime» o en la categoría de sandboxes si ya existe (**sin verificar**). |
| 15 | **Smithery / mcp.so / Glama** | S | medio | Glama indexa repos de GitHub por su cuenta y se reclama con OAuth ([geekflare](https://geekflare.com/guides/mcp-server-directories/)). En mcp.so las fuentes no coinciden (formulario `mcp.so/submit` o issue en `chatmcp/mcp-directory`). Según una fuente, el proxy de Smithery cerraba el 31-oct-2026 y ahora es solo un directorio ([apideck](https://www.apideck.com/blog/mcp-server-directories); **sin verificar**). | Reclamar la ficha de Glama (sirve para el punto 4); enviar a mcp.so por formulario; en Smithery, comprobar a mano si aún se puede enviar. |
| 16 | **e2b-dev/awesome-ai-agents** | — | bajo | Es una lista de *agentes*, no de sandboxes ([repo](https://www.Github.com/e2b-dev/awesome-ai-agents)), y la mantiene un competidor. | No enviar. |

## Entradas listas para pegar

Todas en inglés, como piden las listas. Cada formato sale de la fuente citada en la tabla. Si el
CONTRIBUTING vivo dice otra cosa, manda el CONTRIBUTING.

### punkpeye/awesome-mcp-servers

Dos entradas. Los nombres de categoría salen de memoria y están **sin verificar**: comprobar los
encabezados del README antes de abrir el PR. El badge necesita la ficha de Glama.

```markdown
<!-- Code Execution -->
- [juan52878911/kindling](https://github.com/juan52878911/kindling) [![juan52878911/kindling MCP server](https://glama.ai/mcp/servers/juan52878911/kindling/badges/score.svg)](https://glama.ai/mcp/servers/juan52878911/kindling) 🏎️ 🏠 🐧 🍎 - `kling sbx mcp`: create, exec, read/write files, run code, pause, fork and set egress on self-hosted Firecracker/Virtualization.framework microVM sandboxes (16 tools).

<!-- Aggregators -->
- [juan52878911/kindling](https://github.com/juan52878911/kindling) 🏎️ 🏠 🐧 🍎 - `kling mcp`: runs any npm or PyPI MCP server as an on-demand service that wakes from a microVM snapshot, behind one gateway with a single tool inventory.
```

### Registro oficial MCP (`server.json`, borrador)

El esquema y su URL cambian: validar con `mcp-publisher validate` antes de publicar. Los campos
del paquete MCPB (url, sha256) salen de la release; los de abajo son de relleno.

```json
{
  "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
  "name": "io.github.juan52878911/kindling-sandbox",
  "title": "kindling sandboxes",
  "description": "Self-hosted microVM sandboxes for code agents: exec, files, code, pause, fork, egress.",
  "repository": { "url": "https://github.com/juan52878911/kindling", "source": "github" },
  "version": "1.0.0",
  "packages": [
    {
      "registryType": "mcpb",
      "identifier": "https://github.com/juan52878911/kindling/releases/download/v1.0.0/kling-sbx-mcp-linux-amd64.mcpb",
      "fileSha256": "<sha256>",
      "transport": { "type": "stdio" },
      "environmentVariables": [
        { "name": "KLING_SANDBOX_URL", "isRequired": true, "description": "Sandbox gateway URL" },
        { "name": "KLING_SANDBOX_TOKEN", "isRequired": true, "isSecret": true, "description": "Tenant token" }
      ]
    }
  ]
}
```

(La URL `$schema` y el soporte de `mcpb` están **sin verificar**: copiarlos del
[quickstart](https://modelcontextprotocol.io/registry/quickstart) vigente.)

### awesome-ai-sandboxes (tizkovatereza)

La lista usa campos estructurados (BYOC/self-host, licencia, precio). Adaptar al formato real de
su tabla:

```markdown
| [kindling](https://github.com/juan52878911/kindling) | Firecracker microVM (Linux/KVM), Virtualization.framework (macOS) | Self-host only (single daemon; Helm chart for the gateway) | Apache-2.0 | Free (your hardware) | E2B- and OpenSandbox-compatible APIs; snapshot restore 76–91 ms, pool claim 0.8–1.1 ms on nested EC2 ([benchmarks](https://github.com/juan52878911/kindling/blob/main/docs/benchmarks.md)) |
```

### restyler/awesome-sandbox, fishman/awesome-agent-sandbox, fhiltscher/awesome-ai-coding-sandboxes

```markdown
- [kindling](https://github.com/juan52878911/kindling) - Self-hosted microVM sandboxes (Firecracker on Linux, Virtualization.framework on macOS) that wake from golden snapshots; speaks the E2B and OpenSandbox APIs, per-sandbox egress allowlists by SNI, Apache-2.0.
```

### infracloudio/awesome-microvm

```markdown
- [kindling](https://github.com/juan52878911/kindling) - One daemon and CLI that runs Firecracker (Linux) and Virtualization.framework (macOS) microVMs from frozen golden snapshots, with extensions for agent sandboxes, on-demand MCP servers and disposable databases.
```

### awesome-selfhosted (`software/kindling.yml` en awesome-selfhosted-data, desde el 2026-12-08)

La etiqueta tiene que ser una de las que existen. «Software Development - Testing» es una
propuesta **sin verificar**: revisar `tags/` del repo.

```yaml
name: "kindling"
website_url: "https://kindling.asccilabs.com"
source_code_url: "https://github.com/juan52878911/kindling"
description: "Run microVMs that wake in milliseconds from frozen snapshots, with a multi-tenant sandbox gateway for code agents and on-demand MCP servers."
licenses:
  - Apache-2.0
platforms:
  - Go
tags:
  - Software Development - Testing
```

### awesome-sysadmin (`software/kindling.yml` en awesome-sysadmin-data)

```yaml
name: "kindling"
website_url: "https://kindling.asccilabs.com"
source_code_url: "https://github.com/juan52878911/kindling"
description: "Firecracker and Virtualization.framework microVMs that are frozen to disk (no CPU, no RAM) and thawed in tens of milliseconds; one daemon and one CLI."
licenses:
  - Apache-2.0
platforms:
  - Go
tags:
  - Virtualization
```

### awesome-go (desde el 2027-01-08)

Sección propuesta: «DevOps Tools» (**sin verificar**). El PR tiene que llevar los enlaces a
pkg.go.dev, Go Report Card y cobertura.

```markdown
- [kindling](https://github.com/juan52878911/kindling) - Run Firecracker and Virtualization.framework microVMs that wake in milliseconds from frozen snapshots.
```

### openalternative.co

- **Alternative to:** E2B, Daytona
- **Tagline:** `Self-hosted microVM sandboxes for AI agents, compatible with the E2B and OpenSandbox SDKs.`

### alternativeto.net

- **Alternative to:** E2B, Daytona, Modal, OpenSandbox
- **Descripción:** `kindling runs Firecracker (Linux) and Virtualization.framework (macOS) microVMs that wake from frozen snapshots. Its sandbox gateway speaks the E2B and OpenSandbox APIs, so existing SDKs and agents work by changing an environment variable. Apache-2.0, self-hosted, one daemon and one CLI.`

## Canales en español

Reach más pequeño, pero apenas hay competencia en español.

| Canal | Esfuerzo | Siguiente acción |
|---|---|---|
| README.es.md y docs en español (ya existen) | — | Enlazarlos en cada envío a una comunidad hispana. |
| Charlas (Nerdearla, T3chFest, PyConES) | L | Ver `launch/events.md`. |
| Comunidades (Slack/Discord de Python España, Gophers LATAM, Nerdearla) | S | Un mensaje por comunidad cuando salga la 1.0, con la receta de Claude Code/Codex dentro del sandbox (`docs/recetas/`). Leer antes las reglas de autopromoción de cada una (**sin verificar** cuáles las tienen). |
