# Recetas: agentes dentro de un sandbox

Tres recetas para correr un agente de código dentro de una microVM de kindling, cada una con su script en `scripts/recetas/` (`--dry-run` imprime los comandos sin ejecutarlos).

| Receta | Agente | Credencial | Script |
|---|---|---|---|
| [claude-code.md](claude-code.md) | `claude -p` | `api.anthropic.com` | `scripts/recetas/claude-code.sh` |
| [codex.md](codex.md) | `codex exec` | `api.openai.com` | `scripts/recetas/codex.sh` |
| [playwright.md](playwright.md) | Chromium + script de captura | ninguna | `scripts/recetas/playwright.sh` |

## Por qué el proxy de credenciales

Un agente ejecuta código que escribe un modelo y lee páginas, issues y dependencias que no controlas: es el caso típico de inyección de instrucciones. Lo que importa es qué puede robar.

| | Sin credproxy (`-e CLAVE=…`) | Con credproxy (estas recetas) |
|---|---|---|
| Qué hay en la VM | la clave real en el entorno | un marcador `kling-cred-…` |
| Si el agente la imprime o la sube | se filtra la clave | se filtra un marcador, inútil fuera de esa VM |
| Dónde se usa | contra cualquier dominio permitido | solo hacia `https://<dominio>` y, con `-allow-request`, solo las rutas listadas (otras: 403 del proxy) |
| Tras borrar la VM | la clave sigue válida en cualquier sitio donde se copiara | no hay nada que revocar |
| Rastro | el del proveedor | `kling machine audit <ref>`: método, host, ruta enmascarada, estado; nunca la clave |
| Argv/`ps` | `-e K=V` se ve en `ps` | la clave entra por stdin del CLI y no va en ningún argv |

Coste: el SDK habla `http://<dominio>` con el proxy (que pone el TLS), así que cada receta fija la variable `*_BASE_URL`. Y `-egress allowlist` es obligatorio.

## Qué se ha verificado

`scripts/recetas/test-dry-run.sh` ejecuta los tres `--dry-run` y un modo real con un `kling` falso: comprueba que la clave falsa no sale en la salida ni en ningún argv y que llega por stdin. **No** se ha probado contra un daemon con los agentes reales: las banderas de `claude`, `codex` y la imagen de Playwright son las que documentan sus proyectos y pueden cambiar con la versión. Los comandos `kling` están contrastados con `cmd/kling` y el test `docs_comandos_test.go`.
