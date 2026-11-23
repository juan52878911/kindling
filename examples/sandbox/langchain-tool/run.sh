#!/usr/bin/env bash
# Herramienta de agente con LangChain. `--dry-run` imprime las órdenes y no
# ejecuta nada (ni llama a ningún LLM); sin él las ejecuta contra tu gateway.
# Compatible con bash 3.2 (macOS).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

seco=0
[ "${1:-}" = "--dry-run" ] && seco=1
paso() {
  echo "+ $*"
  [ "$seco" -eq 1 ] || "$@"
}

paso kling sbx template apply -f ../templates/python.json
paso pip install ../../../sdks/python langchain-core langchain-anthropic
paso python3 tool.py
if [ "$seco" -eq 0 ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "ANTHROPIC_API_KEY is not set: the tool works, the agent needs a key." >&2
  exit 2
fi
paso python3 agent.py "What is the 30th Fibonacci number? Compute it, don't guess."
