#!/usr/bin/env bash
# apidiff.sh — compara la API Go pública (pkg/api, pkg/plugin, pkg/config: lo
# que promete 1.0) con la de la etiqueta anterior, y falla si hay un cambio
# incompatible (quitar o cambiar un símbolo exportado, un campo, un método).
#
#   scripts/apidiff.sh [ETIQUETA]
#
# ETIQUETA por defecto: la última alcanzable desde HEAD^ (la anterior a esta,
# esté HEAD etiquetado o no). Un cambio incompatible hecho a propósito se
# declara en el mensaje de algún commit entre ETIQUETA y HEAD con una línea
#
#   API-Break: pkg/api[, pkg/config]: <motivo>
#
# que nombra los paquetes que rompe, antes de los dos puntos: solo esos se
# informan sin fallar; un cambio incompatible en otro paquete sigue fallando.
# Un "API-Break: <motivo>" sin paquetes no excusa ninguno. Necesita apidiff
# (golang.org/x/exp/cmd/apidiff) en el PATH, o APIDIFF=ruta.
#
#   scripts/apidiff.sh --declared < mensajes
#
# imprime, uno por línea, los paquetes que declaran las líneas API-Break: de
# esos mensajes de commit (lo que usa la comprobación; para probarla).
set -euo pipefail

# declarados lee mensajes de commit por la entrada y escribe los paquetes que
# nombran sus líneas "API-Break: <paquetes>: <motivo>": lo de antes del primer
# ":" tras la etiqueta, partido por espacios y comas. Solo cuentan los que
# empiezan por pkg/, para que un motivo sin lista ("API-Break: quita X") no
# cuele una palabra suelta como paquete.
declarados() {
  { grep '^API-Break:' || true; } | while IFS= read -r l; do
    resto=${l#API-Break:}
    case "$resto" in
      *:*) lista=${resto%%:*} ;;
      *)   continue ;;
    esac
    printf '%s\n' "$lista" | tr ',' ' ' | tr -s ' \t' '\n' | { grep '^pkg/' || true; }
  done | sort -u
}

if [ "${1:-}" = "--declared" ]; then
  declarados
  exit 0
fi

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
  permitidos=$(git log --format=%B "$BASE..HEAD" | declarados)
  sin=""
  for p in $roto; do
    if printf '%s\n' "$permitidos" | grep -qxF "$p"; then
      echo "incompatible changes in $p, declared with API-Break:"
    else
      sin="$sin $p"
    fi
  done
  if [ -n "$sin" ]; then
    echo "::error::incompatible API changes in$sin since $BASE; if they are on purpose, say why in a commit with a line 'API-Break: <package>[, <package>]: <reason>' naming each one (and in CHANGELOG)" >&2
    exit 1
  fi
  git log --format=%B "$BASE..HEAD" | grep '^API-Break:' | sed 's/^/  /'
  exit 0
fi
echo "public Go API compatible with $BASE"
