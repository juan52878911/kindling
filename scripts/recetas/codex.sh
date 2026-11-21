#!/usr/bin/env bash
# Receta: Codex CLI (`codex exec`) en un sandbox, con la clave fuera de la VM.
# Doc: docs/recetas/codex.md
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
# shellcheck source=lib.sh
. ./lib.sh

uso() {
  cat <<'U'
uso: codex.sh [--dry-run] ["prompt"]
  Variable: OPENAI_API_KEY (la clave real; solo se lee en el host).
  --dry-run  imprime los comandos sin ejecutarlos ni pedir la clave.
U
}

analizar_args "$@"
PROMPT=${ARGS[0]:-"Escribe un hola mundo en Go en /work/hola.go"}
PLANTILLA=${PLANTILLA:-codex}
NOMBRE=${NOMBRE:-receta-codex}
exigir_clave OPENAI_API_KEY

# 1. Plantilla: node + Codex CLI. Esta fase sí necesita red (npm), pero aún
#    no hay ninguna clave en juego.
ejecutar "$KLING" image import node:22-slim -warm -template "$PLANTILLA" -disk 2G \
  -egress internet -step 'npm install -g @openai/codex' \
  -step 'mkdir -p /work'

# 2. Sandbox con egress mínimo y la credencial en el proxy del host.
ID=$(crear_sandbox "$NOMBRE" -from "$PLANTILLA" api.openai.com)
entregar_credencial OPENAI_API_KEY "$ID" api.openai.com OPENAI_API_KEY \
  -allow-request 'POST /v1/responses' -allow-request 'POST /v1/chat/completions'

# 3. El agente, sin modo interactivo; habla http:// y el proxy pone el TLS.
ejecutar "$KLING" exec -e OPENAI_BASE_URL=http://api.openai.com/v1 \
  -w /work -timeout 10m "$ID" -- \
  codex exec --skip-git-repo-check --sandbox danger-full-access "$PROMPT"

# 4. Resultado: ficheros que salen con cp.
ejecutar "$KLING" cp "$ID:/work/hola.go" -

# 5. Auditoría y limpieza.
ejecutar "$KLING" machine audit "$ID"
ejecutar "$KLING" sandbox rm -f "$ID"
