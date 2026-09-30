#!/usr/bin/env bash
# Instala firecracker y jailer de una release fijada, comprobando su sha256.
# Ejecutar DENTRO de la VM del laboratorio.
#
#   sudo ./20-install-firecracker.sh
#   sudo FC_VERSION=v1.18.0 FC_SHA256=<sha256 del tgz> ./20-install-firecracker.sh
set -euo pipefail

PREFIX="${PREFIX:-/usr/local/bin}"
ARCH="$(uname -m)"

# La versión y el sha256 de su tgz, fijados aquí y no resueltos al vuelo: antes
# se instalaba como root lo que dijera la API de GitHub en ese momento, sin
# comprobar nada. Los hashes son los .sha256.txt de la release. Otra versión
# pide su hash (FC_SHA256): sin él no se instala.
FC_VERSION="${FC_VERSION:-v1.17.0}"
if [ -z "${FC_SHA256:-}" ]; then
  case "$FC_VERSION-$ARCH" in
    v1.17.0-x86_64)  FC_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558 ;;
    v1.17.0-aarch64) FC_SHA256=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256 ;;
    *) echo "no hay sha256 fijado para Firecracker $FC_VERSION ($ARCH): pásalo en FC_SHA256" >&2; exit 1 ;;
  esac
fi

sha256_de() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

[ -e /dev/kvm ] || {
  echo "no hay /dev/kvm. Si esto es una VM, el host debe pasar las extensiones de" >&2
  echo "virtualización (en Proxmox: --cpu host) y tener la anidación activada." >&2
  exit 1
}

command -v curl >/dev/null || { echo "falta curl" >&2; exit 1; }

# Sin el agente, el hipervisor cuenta la caché de disco del invitado como memoria
# usada. Con snapshots de cientos de MB eso llena el panel de falsos positivos.
if [ -d /sys/class/dmi ] && ! systemctl is-active --quiet qemu-guest-agent 2>/dev/null; then
  echo "instalando qemu-guest-agent (para que el hipervisor reporte memoria real)"
  apt-get install -y -qq qemu-guest-agent >/dev/null 2>&1 && \
    systemctl enable --now qemu-guest-agent >/dev/null 2>&1 || \
    echo "  aviso: no pude instalarlo; el panel contará la caché como memoria usada"
fi

TAG="$FC_VERSION"
echo "instalando Firecracker $TAG ($ARCH)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
curl -sfL "https://github.com/firecracker-microvm/firecracker/releases/download/${TAG}/firecracker-${TAG}-${ARCH}.tgz" \
  -o "$tmp/fc.tgz"
got="$(sha256_de "$tmp/fc.tgz")"
[ "$got" = "$FC_SHA256" ] || {
  echo "sha256 de firecracker-${TAG}-${ARCH}.tgz no coincide: $got, se esperaba $FC_SHA256" >&2; exit 1; }
tar -xzf "$tmp/fc.tgz" -C "$tmp"

install -m755 "$tmp/release-${TAG}-${ARCH}/firecracker-${TAG}-${ARCH}" "$PREFIX/firecracker"
install -m755 "$tmp/release-${TAG}-${ARCH}/jailer-${TAG}-${ARCH}"      "$PREFIX/jailer"

"$PREFIX/firecracker" --version | head -1
"$PREFIX/jailer" --version 2>&1 | head -1
