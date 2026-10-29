#!/bin/sh
# Rellena la plantilla kling.rb con la versión y los sha256 del SHA256SUMS de
# la release. Uso: formula.sh 0.20.0 ruta/SHA256SUMS > kling.rb
# Falla si a la plantilla le queda un @...@ sin rellenar (un binario que el
# SHA256SUMS no lista): una fórmula con un hash vacío no se publica.
set -eu
[ $# -eq 2 ] || { echo "uso: $0 VERSIÓN SHA256SUMS" >&2; exit 2; }
VERSION="${1#v}"
SUMS="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"
echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || {
  echo "versión con forma inesperada: $VERSION" >&2; exit 1; }
out="$(sed "s/@VERSION@/$VERSION/g" "$HERE/kling.rb")"
for asset in $(printf '%s\n' "$out" | sed -n 's/.*@SHA256_\([A-Za-z0-9.-]*\)@.*/\1/p'); do
  # sha256sum escribe "<hash>  <nombre>" (o "*<nombre>" en modo binario).
  sum="$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$SUMS")"
  echo "$sum" | grep -Eq '^[0-9a-f]{64}$' || { echo "$asset no está en $SUMS" >&2; exit 1; }
  out="$(printf '%s\n' "$out" | sed "s/@SHA256_$asset@/$sum/g")"
done
if printf '%s\n' "$out" | grep -q '@[A-Za-z0-9_.-]*@'; then
  echo "quedan marcadores sin rellenar" >&2; exit 1
fi
# La cabecera de la plantilla sobra en la fórmula publicada.
printf '%s\n' "$out" | sed '/^# Fórmula de Homebrew/,/^# van firmados/d'
