# Receta: Codex CLI en un sandbox

Codex CLI no interactivo (`codex exec`) en una microVM que solo sale a `api.openai.com`, con la clave de OpenAI en el host.

```sh
export OPENAI_API_KEY=...           # solo en el host
scripts/recetas/codex.sh --dry-run "tu prompt"
scripts/recetas/codex.sh "tu prompt"                   # necesita el daemon
```

## Pasos

1. **Plantilla** (una vez): `kling image import node:22-slim -warm -template codex -disk 2G -egress internet -step 'npm install -g @openai/codex'`.
2. **Sandbox**: `kling sandbox create -from codex -egress allowlist -allow api.openai.com -ttl 30m -wait-ready -q`.
3. **Credencial**: por stdin, `kling machine credential <id> -domain api.openai.com -env OPENAI_API_KEY -allow-request 'POST /v1/responses' -allow-request 'POST /v1/chat/completions'`. La VM solo ve `kling-cred-…`.
4. **Agente**: `kling exec -e OPENAI_BASE_URL=http://api.openai.com/v1 -w /work <id> -- codex exec --skip-git-repo-check --sandbox danger-full-access "…"`. `http://` porque el proxy pone el TLS.
5. **Resultado**: `kling cp <id>:/work/fichero -`, o un volumen con `-volume` en el `sandbox create`.
6. **Cierre**: `kling machine audit <id>`; `kling sandbox rm -f <id>`.

## Por qué es más seguro

- La clave nunca entra en la VM ni en el argv de ningún comando; el agente puede leer todo el entorno y solo encuentra un marcador.
- `-allow-request` deja solo las rutas de inferencia (no ficheros, ni ajustes de la cuenta).
- `--sandbox danger-full-access` desactiva el sandbox propio de Codex porque dentro de la microVM no aporta y suele fallar; el límite real es la VM, la red allowlist y el TTL.

Sin verificar contra el agente real: las rutas que usa tu versión de Codex (`/v1/responses` o `/v1/chat/completions`) y que respete `OPENAI_BASE_URL`; mira `kling machine audit <id> -denied` si algo da 403.
