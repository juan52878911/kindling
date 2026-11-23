#!/usr/bin/env bash
# El ejemplo code-interpreter de OpenSandbox, sin cambios, apuntado al gateway.
# `--dry-run` imprime las órdenes y no ejecuta nada. Los nombres de las variables
# y la ruta del ejemplo de OpenSandbox NO están verificados aquí (sin red): ver el README.
# Compatible con bash 3.2 (macOS).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

seco=0
[ "${1:-}" = "--dry-run" ] && seco=1
paso() {
  echo "+ $*"
  [ "$seco" -eq 1 ] || "$@"
}

OSB_DIR=${OSB_DIR:-OpenSandbox}
paso git clone --depth 1 https://github.com/alibaba/OpenSandbox "$OSB_DIR"
paso pip install opensandbox opensandbox-code-interpreter
# El SDK de OpenSandbox lee dominio y clave de su entorno: aquí, el gateway y el token del inquilino.
echo "+ export SANDBOX_DOMAIN=127.0.0.1:8090 SANDBOX_API_KEY=\$KLING_SANDBOX_TOKEN"
if [ "$seco" -eq 0 ]; then
  export SANDBOX_DOMAIN=127.0.0.1:8090 SANDBOX_API_KEY=${KLING_SANDBOX_TOKEN:?set KLING_SANDBOX_TOKEN}
fi
paso python3 "$OSB_DIR/examples/code-interpreter/main.py"
