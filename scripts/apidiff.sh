#!/usr/bin/env bash
# apidiff.sh — compara la API Go pública (pkg/api, pkg/plugin, pkg/config: lo
# que promete 1.0) con la de la etiqueta anterior, y falla si hay un cambio
# incompatible (quitar o cambiar un símbolo exportado, un campo, un método).
#
#   scripts/apidiff.sh [ETIQUETA]
#
# ETIQUETA por defecto: la última alcanzable desde HEAD^ (la anterior a esta,
# esté HEAD etiquetado o no). Un cambio incompatible hecho a propósito se
# declara con una línea "API-Break: <motivo>" en el mensaje de algún commit
# entre ETIQUETA y HEAD; entonces se informa y no falla. Necesita apidiff
# (golang.org/x/exp/cmd/apidiff) en el PATH, o APIDIFF=ruta.
set -euo pipefail

cd "$(dirname "$0")/.."
APIDIFF="${APIDIFF:-apidiff}"
command -v "$APIDIFF" >/dev/null || { echo "apidiff not found: go install golang.org/x/exp/cmd/apidiff@latest" >&2; exit 2; }
MOD=github.com/juan52878911/kindling
PKGS="${PKGS:-pkg/api pkg/plugin pkg/config}"
BASE="${1:-$(git describe --tags --abbrev=0 HEAD^)}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/old" "$tmp/export"
# El árbol de la etiqueta entero (pkg/ importa otros paquetes del módulo),
# sin tocar el repo: ni worktree ni checkout.
git archive "$BASE" | tar -x -C "$tmp/old"

export GOWORK=off
roto=""
for p in $PKGS; do
  n=$(echo "$p" | tr / _)
  if [ ! -d "$tmp/old/$p" ]; then
    echo "== $p: new since $BASE"
    continue
  fi
  ( cd "$tmp/old" && "$APIDIFF" -w "$tmp/export/$n.old" "$MOD/$p" )
  "$APIDIFF" -w "$tmp/export/$n.new" "$MOD/$p"
  todo=$("$APIDIFF" "$tmp/export/$n.old" "$tmp/export/$n.new")
  inc=$("$APIDIFF" -incompatible "$tmp/export/$n.old" "$tmp/export/$n.new")
  echo "== $p against $BASE"
  if [ -z "$todo" ]; then echo "  no changes"; else printf '%s\n' "$todo" | sed 's/^/  /'; fi
  [ -n "$inc" ] && roto="$roto $p"
done

if [ -n "$roto" ]; then
  if git log --format=%B "$BASE..HEAD" | grep -q '^API-Break:'; then
    echo "incompatible changes in$roto, declared with API-Break:"
    git log --format=%B "$BASE..HEAD" | grep '^API-Break:' | sed 's/^/  /'
    exit 0
  fi
  echo "::error::incompatible API changes in$roto since $BASE; if they are on purpose, say why in a commit with a line 'API-Break: <reason>' (and in CHANGELOG)" >&2
  exit 1
fi
echo "public Go API compatible with $BASE"
