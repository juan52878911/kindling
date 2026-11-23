#!/usr/bin/env bash
# code-server tras un endpoint. `--dry-run` imprime las órdenes y no ejecuta nada.
# Compatible con bash 3.2 (macOS).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

seco=0
[ "${1:-}" = "--dry-run" ] && seco=1
paso() {
  echo "+ $*"
  [ "$seco" -eq 1 ] || "$@"
}

paso kling sbx template apply -f code-server.json
paso pip install ../../../sdks/python
paso python3 serve.py
