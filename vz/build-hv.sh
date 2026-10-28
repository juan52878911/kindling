#!/bin/sh
# Compila kling-hv y lo firma ad-hoc con el entitlement de Hypervisor.framework
# (com.apple.security.hypervisor). Sin él, hv_vm_create devuelve HV_DENIED.
#
#   ./build-hv.sh                 -> ./bin/kling-hv
#   OUT=/ruta/kling-hv VERSION=0.1.0 ./build-hv.sh
set -eu
cd "$(dirname "$0")"
OUT=${OUT:-bin/kling-hv}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)}
mkdir -p "$(dirname "$OUT")"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION#v}" -o "$OUT" ./cmd/kling-hv
codesign --force --entitlements kling-hv.entitlements -s - "$OUT"
echo "$OUT ($VERSION)"
