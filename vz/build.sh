#!/bin/sh
# Compila kling-vz y lo firma ad-hoc con el entitlement de virtualización.
#
# La firma ad-hoc (-s -) basta: Virtualization.framework no exige Developer ID,
# solo que el proceso declare com.apple.security.virtualization. Sin la firma,
# crear la VM falla con un error de permisos poco claro.
#
#   ./build.sh                 -> ./bin/kling-vz
#   OUT=/ruta/kling-vz VERSION=0.1.0 ./build.sh
set -eu
cd "$(dirname "$0")"
OUT=${OUT:-bin/kling-vz}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)}
mkdir -p "$(dirname "$OUT")"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION#v}" -o "$OUT" ./cmd/kling-vz
codesign --force --entitlements kling-vz.entitlements -s - "$OUT"
echo "$OUT ($VERSION)"
