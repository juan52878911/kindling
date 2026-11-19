# Receta: Claude Code en un sandbox

Claude Code no interactivo (`claude -p`) en una microVM sin red salvo `api.anthropic.com`, y con la clave de Anthropic **en el host**, no en la VM.

```sh
export ANTHROPIC_API_KEY=...        # solo en el host
scripts/recetas/claude-code.sh --dry-run "tu prompt"   # ver los comandos
scripts/recetas/claude-code.sh "tu prompt"             # necesita el daemon
```

## Pasos

1. **Plantilla** (una vez): imagen pública de node con el agente instalado y guardada como plantilla, que arranca en ms.
   `kling image import node:22-slim -warm -template claude-code -egress internet -step 'npm install -g @anthropic-ai/claude-code'`
   Esta fase tiene internet para npm, pero aún no hay ninguna clave.
2. **Sandbox**: `kling sandbox create -from claude-code -egress allowlist -allow api.anthropic.com -ttl 30m -wait-ready -q` imprime el id.
3. **Credencial**: la clave entra por stdin a `kling machine credential <id> -domain api.anthropic.com -env ANTHROPIC_API_KEY -allow-request 'POST /v1/messages'`. En la VM, `ANTHROPIC_API_KEY` vale `kling-cred-…`.
4. **Agente**: `kling exec -e ANTHROPIC_BASE_URL=http://api.anthropic.com -w /work <id> -- claude -p "…" --permission-mode acceptEdits`. Es `http://` a propósito: el proxy del host sale por HTTPS verificando el certificado.
5. **Resultado**: `kling cp <id>:/work/fichero -` lo saca a stdout. Para varios ficheros o para que sobrevivan al sandbox, crea un volumen (`kling volume create`) y pásalo con `-volume NOMBRE:/work` en el `sandbox create`.
6. **Cierre**: `kling machine audit <id>` y `kling sandbox rm -f <id>`.

## Por qué es más seguro

- Si el prompt es hostil y el agente intenta `echo $ANTHROPIC_API_KEY | curl …`, saca un marcador. Además, fuera de `api.anthropic.com` no hay red.
- `-allow-request` limita la clave a `/v1/messages`: aunque el agente use el proxy, no puede llamar a otras rutas de la API.
- `--permission-mode acceptEdits` en lugar de saltarse permisos: Claude Code se niega a `--dangerously-skip-permissions` como root. El aislamiento es la VM, que se destruye sola con el `-ttl`.

Sin verificar contra el agente real: que la versión instalada acepte un marcador como `ANTHROPIC_API_KEY` y `ANTHROPIC_BASE_URL` con `http://`.
