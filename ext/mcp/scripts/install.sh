#!/bin/sh
# install.sh — el instalador de kindling-mcp de antes de la unificación.
#
# Desde kindling v0.13.0 la extensión vive en el repo de kindling (ext/mcp) y
# sale en su misma release, con un único SHA256SUMS. Este script ya no descarga
# nada por su cuenta: traduce sus opciones de siempre y pasa el testigo al
# instalador del núcleo con --skip-kling --with mcp, para que las recetas que lo
# citan (curl ... | sh) sigan funcionando y haya una sola copia de la lógica de
# descarga y verificación.
#
# USO (igual que antes)
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/ext/mcp/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --tag v0.13.0 --prefix ~/.local/bin
#   curl -fsSL .../install.sh | sh -s -- --no-bridge --dry-run
#
# Equivale a:
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh -s -- --skip-kling --with mcp

set -u

# KLING_MCP_VERSION era la variable de este instalador; la release ahora es la
# de kindling, así que se traduce a la del núcleo.
if [ -n "${KLING_MCP_VERSION:-}" ] && [ -z "${KLING_VERSION:-}" ]; then
    KLING_VERSION="$KLING_MCP_VERSION"
    export KLING_VERSION
fi

# Reescribe "$@" in situ: POSIX sh no tiene arrays, así que se rota la lista
# (se saca cada argumento por delante y se añade traducido por detrás).
n=$#
while [ "$n" -gt 0 ]; do
    a="$1"; shift; n=$((n - 1))
    case "$a" in
        --no-bridge)
            set -- "$@" --no-companions ;;
        --repo)
            # El repo kindling-mcp ya no publica releases nuevas: la opción se
            # ignora en vez de mandar a descargar de un sitio que no las tiene.
            if [ "$n" -gt 0 ]; then shift; n=$((n - 1)); fi
            printf '  ! --repo ignored: kindling-mcp now ships in the kindling release\n' >&2 ;;
        --tag|--prefix)
            if [ "$n" -gt 0 ]; then
                set -- "$@" "$a" "$1"; shift; n=$((n - 1))
            else
                set -- "$@" "$a"
            fi ;;
        *)
            set -- "$@" "$a" ;;
    esac
done

# Desde un clon, el instalador del núcleo está tres directorios más arriba; por
# curl | sh no hay fichero ($0 es la shell) y se baja de main.
here=""
case "$0" in
    */install.sh) here="$(cd "$(dirname "$0")" 2>/dev/null && pwd)" ;;
esac
if [ -n "$here" ] && [ -f "$here/../../../scripts/install.sh" ]; then
    exec sh "$here/../../../scripts/install.sh" --skip-kling --with mcp "$@"
fi

# A un fichero y no por tubería: con `curl | sh` un fallo de descarga le da a sh
# una entrada vacía y el conjunto sale con 0 sin haber instalado nada.
URL="https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh"
TMP="$(mktemp)" || exit 1
trap 'rm -f "$TMP"' EXIT INT TERM
if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 -o "$TMP" "$URL"
elif command -v wget >/dev/null 2>&1; then
    wget -q --tries=3 -O "$TMP" "$URL"
else
    printf '  ✗ neither curl nor wget is available\n' >&2
    exit 1
fi || { printf '  ✗ could not download %s\n' "$URL" >&2; exit 1; }
sh "$TMP" --skip-kling --with mcp "$@"
