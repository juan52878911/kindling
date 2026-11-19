#!/usr/bin/env bash
# Receta: Claude Code (`claude -p`) en un sandbox, con la clave fuera de la VM.
# Doc: docs/recetas/claude-code.md
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
# shellcheck source=lib.sh
. ./lib.sh

uso() {
  cat <<'U'
uso: claude-code.sh [--dry-run] ["prompt"]
  Variable: ANTHROPIC_API_KEY (la clave real; solo se lee en el host).
  --dry-run  imprime los comandos sin ejecutarlos ni pedir la clave.
U
}

analizar_args "$@"
PROMPT=${ARGS[0]:-"Escribe un hola mundo en Go en /work/hola.go"}
PLANTILLA=${PLANTILLA:-claude-code}
NOMBRE=${NOMBRE:-receta-claude}
exigir_clave ANTHROPIC_API_KEY

# 1. Plantilla: node + Claude Code. Esta fase sí necesita red (npm), pero aún
#    no hay ninguna clave en juego.
ejecutar "$KLING" image import node:22-slim -warm -template "$PLANTILLA" \
  -egress internet -step 'npm install -g @anthropic-ai/claude-code' \
  -step 'mkdir -p /work'

# 2. Sandbox con egress mínimo y la credencial en el proxy del host.
ID=$(crear_sandbox "$NOMBRE" -from "$PLANTILLA" api.anthropic.com)
entregar_credencial ANTHROPIC_API_KEY "$ID" api.anthropic.com ANTHROPIC_API_KEY \
  -allow-request 'POST /v1/messages' -allow-request 'POST /v1/messages/count_tokens'

# 3. El agente, sin modo interactivo; habla http:// y el proxy pone el TLS.
ejecutar "$KLING" exec -e ANTHROPIC_BASE_URL=http://api.anthropic.com \
  -w /work -timeout 10m "$ID" -- \
  claude -p "$PROMPT" --permission-mode acceptEdits

# 4. Resultado: ficheros que salen con cp.
ejecutar "$KLING" cp "$ID:/work/hola.go" -

# 5. Auditoría y limpieza.
ejecutar "$KLING" machine audit "$ID"
ejecutar "$KLING" sandbox rm -f "$ID"
