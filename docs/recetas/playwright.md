# Receta: Playwright (Chromium) en un sandbox

Una captura de pantalla con Chromium dentro de una microVM que solo sale a un dominio. No lleva credencial: el navegador no necesita ninguna clave; lo que protege aquí es el egress.

```sh
scripts/recetas/playwright.sh --dry-run example.org
scripts/recetas/playwright.sh example.org      # deja captura.png en el directorio actual
```

## Pasos

1. **Plantilla** (una vez): `kling image import mcr.microsoft.com/playwright:v1.49.1-noble -warm -template playwright -egress internet -mem 2G -step '… npm install playwright@1.49.1'`. La imagen oficial ya trae Chromium y sus librerías. (El servidor MCP de `ext/mcp` comparte un único Chromium entre sesiones; para una captura suelta no hace falta.)
2. **Sandbox**: `kling sandbox create -from playwright -egress allowlist -allow example.org -ttl 30m -wait-ready -q`.
3. **Script**: `captura.js` entra por `kling cp - <id>:/work/captura.js` y se ejecuta con `kling exec -w /work <id> -- node captura.js https://example.org`.
4. **Resultado**: `kling cp <id>:/work/captura.png - > captura.png`.
5. **Cierre**: `kling sandbox rm -f <id>`.

## Por qué es más seguro

Una página hostil ejecuta JavaScript dentro de Chromium. Con `allowlist` de un solo dominio no puede enviar lo que lea a otro sitio, y la VM no tiene claves que robar. Si el agente que lo maneja sí necesita una API (un LLM que decide qué capturar), usa la receta de [Claude Code](claude-code.md) o de [Codex](codex.md) en el mismo sandbox: la clave sigue fuera.

Chromium se lanza con `--no-sandbox` porque corre como root dentro de la VM, que es el aislamiento. Sin verificar: la versión exacta de la imagen y que el sandbox necesite más de 1 GiB de memoria en páginas pesadas (la plantilla fija 2 GiB).
