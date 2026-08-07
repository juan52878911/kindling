#!/usr/bin/env bash
# Instala firecracker y jailer desde la última release. Ejecutar DENTRO de la VM del laboratorio.
set -euo pipefail

PREFIX="${PREFIX:-/usr/local/bin}"
ARCH="$(uname -m)"

[ -e /dev/kvm ] || {
  echo "no hay /dev/kvm. Si esto es una VM, el host debe pasar las extensiones de" >&2
  echo "virtualización (en Proxmox: --cpu host) y tener la anidación activada." >&2
  exit 1
}

command -v curl >/dev/null || { echo "falta curl" >&2; exit 1; }
command -v jq   >/dev/null || { echo "falta jq" >&2; exit 1; }

TAG="$(curl -sfL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest \
       | jq -r .tag_name)"
[ -n "$TAG" ] && [ "$TAG" != "null" ] || { echo "no pude resolver la última release" >&2; exit 1; }

echo "instalando Firecracker $TAG ($ARCH)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
curl -sfL "https://github.com/firecracker-microvm/firecracker/releases/download/${TAG}/firecracker-${TAG}-${ARCH}.tgz" \
  | tar -xz -C "$tmp"

install -m755 "$tmp/release-${TAG}-${ARCH}/firecracker-${TAG}-${ARCH}" "$PREFIX/firecracker"
install -m755 "$tmp/release-${TAG}-${ARCH}/jailer-${TAG}-${ARCH}"      "$PREFIX/jailer"

"$PREFIX/firecracker" --version | head -1
"$PREFIX/jailer" --version 2>&1 | head -1
