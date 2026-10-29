#!/bin/sh
# firma-clave.sh — genera en local el par de claves con el que se firma el
# SHA256SUMS de cada release (docs/firmas.md) y dice dónde va cada parte.
#
#   scripts/firma-clave.sh [DIR]     # por defecto ~/.kindling-release-key
#
# No sube nada ni toca el repo: escribe DIR/kling-release.key (secreta, 0600,
# formato minisign sin cifrar) y DIR/kling-release.pub, e imprime los pasos.
# Se corre una vez por clave (la primera y en cada rotación).
set -eu
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${1:-$HOME/.kindling-release-key}"
command -v go >/dev/null 2>&1 || { echo "hace falta Go para generar la clave" >&2; exit 1; }
[ -e "$DIR/kling-release.key" ] && { echo "$DIR/kling-release.key ya existe: no la machaco" >&2; exit 1; }

(cd "$HERE" && go run ./internal/upgrade/firmar genkey -dir "$DIR")
PUB="$(sed -n 2p "$DIR/kling-release.pub")"

cat <<EOF

Siguientes pasos:

1. La clave SECRETA ($DIR/kling-release.key) va al secreto del environment
   protegido "release" del repo (Settings > Environments > release; con
   revisores obligatorios y solo la etiqueta v* como rama/etiqueta permitida):

     gh secret set KLING_RELEASE_SIGNING_KEY --env release < "$DIR/kling-release.key"

2. Guarda una copia de la secreta fuera de este Mac (gestor de contraseñas o
   un USB cifrado) y BORRA la del disco:  rm "$DIR/kling-release.key"
   Sin copia, perderla obliga a rotar (docs/firmas.md §Rotar).

3. La PÚBLICA va en el código, en un commit antes de la primera release firmada:
     - internal/upgrade/firma.go, en clavesRelease:  "$PUB",
     - scripts/install.sh, en KLING_RELEASE_PUBKEYS (separadas por espacios)
   (un test exige que las dos listas sean iguales).

4. Comprueba una release a mano:
     minisign -V -P $PUB -m SHA256SUMS -x SHA256SUMS.minisig
EOF
