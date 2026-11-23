#!/usr/bin/env bash
# Análisis de datos con /code. `--dry-run` imprime las órdenes y no ejecuta nada.
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
paso pip install ../../../sdks/python
paso python3 analyze.py
