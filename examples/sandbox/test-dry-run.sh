#!/usr/bin/env bash
# Test de los ejemplos de sandbox: cada --dry-run sale con código 0, imprime
# órdenes («+ …») y no llama a ningún LLM ni necesita daemon, gateway ni claves.
# Compatible con bash 3.2 (macOS). Como scripts/recetas/test-dry-run.sh.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

FALSA="sk-ant-FALSA-0123456789abcdef"
fallos=0
for d in langchain-tool data-analysis code-server fork-variants osb-code-interpreter; do
  if [ ! -f "$d/README.md" ] || [ ! -f "$d/run.sh" ]; then
    echo "FALLO $d: faltan README.md o run.sh"; fallos=$((fallos + 1)); continue
  fi
  salida=$(ANTHROPIC_API_KEY=$FALSA OPENAI_API_KEY=$FALSA bash "$d/run.sh" --dry-run 2>&1) \
    || { echo "FALLO $d: --dry-run salió con error"; fallos=$((fallos + 1)); continue; }
  n=$(grep -c '^+ ' <<<"$salida" || true)
  if [ "$n" -lt 3 ]; then echo "FALLO $d: solo $n órdenes"; fallos=$((fallos + 1)); fi
  if grep -qF -- "$FALSA" <<<"$salida"; then echo "FALLO $d: la clave falsa sale en la salida"; fallos=$((fallos + 1)); fi
  echo "ok $d: $n órdenes"
done
[ "$fallos" -eq 0 ]
