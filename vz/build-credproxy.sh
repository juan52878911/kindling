#!/bin/sh
# Compila kling-credproxy, el proxy de credenciales aparte de cada máquina
# (docs/proxy-macos-separado.md del núcleo), y lo firma ad-hoc con hardened
# runtime y SIN entitlements: sin com.apple.security.get-task-allow, otro
# proceso del mismo usuario (el VMM de la máquina incluido) no consigue su
# task_for_pid para leerle la memoria, que es donde están las claves. Es lo
# que hace que la separación valga algo. No se vuelve a tocar el binario
# después (strip, otra firma): se perdería el hardened runtime.
#
#   ./build-credproxy.sh            -> ./bin/kling-credproxy
#   OUT=/ruta/kling-credproxy VERSION=0.1.0 ./build-credproxy.sh
set -eu
cd "$(dirname "$0")"
OUT=${OUT:-bin/kling-credproxy}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)}
mkdir -p "$(dirname "$OUT")"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION#v}" -o "$OUT" ./cmd/kling-credproxy
codesign --force --options runtime -s - "$OUT"
# Que la firma diga lo que se pide: hardened runtime y ningún entitlement.
codesign -d --verbose=2 "$OUT" 2>&1 | grep -q 'flags=0x10002(adhoc,runtime)' \
	|| { echo "$OUT: not signed with the hardened runtime" >&2; exit 1; }
if codesign -d --entitlements - "$OUT" 2>/dev/null | grep -q get-task-allow; then
	echo "$OUT: has get-task-allow" >&2
	exit 1
fi
echo "$OUT ($VERSION)"
