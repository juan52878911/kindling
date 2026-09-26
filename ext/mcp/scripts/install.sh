#!/bin/sh
# install.sh — instala kindling-mcp (kling-mcp y el puente local) desde GitHub Releases.
#
# USO
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling-mcp/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --tag v0.1.0 --prefix ~/.local/bin
#   curl -fsSL .../install.sh | sh -s -- --dry-run
#
# kling-mcp es una extensión de kling: necesita kindling instalado antes
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
# El instalador comprueba que el kling que encuentra tiene la versión mínima
# que declara la extensión (min_kling de su manifiesto).
#
# Esto instala la parte de TU máquina: kling-mcp, que añade a kling los comandos
# mcp, add, connect, gateway..., y kling-bridge, para exponer un MCP de stdio
# local. La parte del host del daemon (gateway, heal, empaquetador y el puente
# que va dentro de las microVMs) se despliega con `make deploy` desde el repo.
#
# Variables de entorno respetadas:
#   KLING_MCP_VERSION  versión a instalar (ej. v0.1.0). Por defecto: última estable.
#   KLING_PREFIX       directorio de instalación. Por defecto: ~/.local/bin
#   KLING_MCP_REPO     repo de donde descargar. Por defecto: juan52878911/kindling-mcp

set -u
# NOTA: set -u pero NO set -e; los errores se manejan a mano con `||`/`if`.

REPO="${KLING_MCP_REPO:-juan52878911/kindling-mcp}"
PREFIX="${KLING_PREFIX:-${HOME}/.local/bin}"
TAG="${KLING_MCP_VERSION:-}"
DRY_RUN=0
NO_BRIDGE=0

usage() {
    cat <<USAGE
Uso: install.sh [opciones]

  --tag VER          instala una versión concreta (ej. v0.1.0). Por defecto: última.
  --prefix DIR       directorio destino (por defecto: ~/.local/bin)
  --no-bridge        no instala kling-bridge
  --repo OWNER/NAME  repo de GitHub (por defecto: juan52878911/kindling-mcp)
  --dry-run          muestra lo que haría sin descargar ni instalar nada
  -h, --help         muestra esta ayuda
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tag)       TAG="$2"; shift 2 ;;
        --prefix)    PREFIX="$2"; shift 2 ;;
        --no-bridge) NO_BRIDGE=1; shift ;;
        --repo)      REPO="$2"; shift 2 ;;
        --dry-run)   DRY_RUN=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)           echo "opción desconocida: $1" >&2; usage >&2; exit 2 ;;
    esac
done

info() { printf '  • %s\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }
fail() { printf '  ✗ %s\n' "$*" >&2; exit 1; }

case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
    linux)  PLAT_OS="linux" ;;
    darwin) PLAT_OS="darwin" ;;
    *)      fail "sistema no soportado: kindling-mcp corre en Linux y macOS" ;;
esac
case "$(uname -m)" in
    x86_64|amd64)  PLAT_ARCH="amd64" ;;
    aarch64|arm64) PLAT_ARCH="arm64" ;;
    *)             fail "arquitectura no soportada: $(uname -m)" ;;
esac
PLAT="${PLAT_OS}-${PLAT_ARCH}"

if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" 2>/dev/null | sed 's|.*/||')
    if [ -z "$TAG" ] || [ "$TAG" = "latest" ]; then
        fail "no pude resolver la última release. Pasa --tag vX.Y.Z."
    fi
fi

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --tries=3 -O "$2" "$1"
    else
        fail "ni curl ni wget disponibles"
    fi
}

# sha256sum en Linux, shasum -a 256 en macOS.
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# verify NOMBRE: las dos guardas de vacío hacen falta. Sin set -e, una
# herramienta que falte deja ACTUAL vacío, y "" = "" daría por bueno un binario
# sin verificar.
verify() {
    EXPECTED="$(grep -E "  $1\$" "$WORK/SHA256SUMS" | awk '{print $1}')"
    [ -n "$EXPECTED" ] || fail "no encuentro $1 en SHA256SUMS"
    ACTUAL="$(sha256 "$WORK/$1")"
    [ -n "$ACTUAL" ] || fail "no puedo calcular el sha256 de $1 (falta sha256sum/shasum)"
    [ "$EXPECTED" = "$ACTUAL" ] || fail "checksum de $1 no coincide (esperaba $EXPECTED, obtuve $ACTUAL)"
}

# Compara "X.Y.Z" por sus tres números: ¿$1 >= $2?
version_ge() {
    [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}

EXT="kling-mcp-${PLAT}"
BRIDGE="kling-bridge-${PLAT}"
BASE="https://github.com/${REPO}/releases/download/${TAG}"

echo
echo "kindling-mcp ${TAG} — instalación"
echo "  plataforma:  ${PLAT}"
echo "  destino:     ${PREFIX}"
echo

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) NO descargo ni instalo nada. Descargaría:"
    info "$BASE/$EXT"
    [ "$NO_BRIDGE" = "1" ] || info "$BASE/$BRIDGE"
    info "$BASE/SHA256SUMS"
    exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

info "descargando $EXT"
fetch "$BASE/$EXT" "$WORK/$EXT" || fail "no pude descargar $EXT"
fetch "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" || fail "no pude descargar SHA256SUMS"
verify "$EXT"
ok "$EXT verificado"
if [ "$NO_BRIDGE" != "1" ]; then
    info "descargando $BRIDGE"
    fetch "$BASE/$BRIDGE" "$WORK/$BRIDGE" || fail "no pude descargar $BRIDGE"
    verify "$BRIDGE"
    ok "$BRIDGE verificado"
fi

# ¿Hay un kling suficiente? La extensión declara su mínimo en el manifiesto.
chmod +x "$WORK/$EXT"
MIN="$("$WORK/$EXT" --kling-manifest 2>/dev/null | tr -d "\n" | grep -o "\"min_kling\": *\"[^\"]*\"" | sed "s/.*\"\([^\"]*\)\"$/\1/")"
if ! command -v kling >/dev/null 2>&1; then
    warn "no encuentro kling en el PATH. kling-mcp es una extensión: instala kindling ${MIN:+>= v$MIN }primero:"
    warn "  curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh"
elif [ -n "$MIN" ]; then
    HAVE="$(kling version 2>/dev/null | awk '{print $2}' | sed 's/^v//; s/[-+].*//')"
    case "$HAVE" in
        [0-9]*.[0-9]*)
            if version_ge "$HAVE" "$MIN"; then
                ok "kling $HAVE (necesita >= $MIN)"
            else
                fail "tu kling es $HAVE y kindling-mcp ${TAG} necesita >= $MIN. Actualiza kindling primero."
            fi ;;
        *) warn "no sé leer la versión de kling (\"$HAVE\"); sigo sin comprobar el mínimo ($MIN)" ;;
    esac
fi

if [ ! -d "$PREFIX" ]; then
    mkdir -p "$PREFIX" 2>/dev/null || fail "no puedo crear $PREFIX — prueba --prefix"
fi
mv "$WORK/$EXT" "$PREFIX/kling-mcp" || fail "no puedo escribir en $PREFIX"
ok "instalado en $PREFIX/kling-mcp"
if [ "$NO_BRIDGE" != "1" ]; then
    chmod +x "$WORK/$BRIDGE"
    mv "$WORK/$BRIDGE" "$PREFIX/kling-bridge" || fail "no puedo escribir en $PREFIX"
    ok "instalado en $PREFIX/kling-bridge"
fi

cat <<DONE

  kling descubre la extensión por estar en el PATH. Comprueba:

      kling plugins
      kling mcp search github

  Recarga el completado de la shell para ver los comandos nuevos:

      source <(kling completion zsh)

  En el host del daemon (gateway, heal, empaquetador), desde un clon del repo:

      make deploy HOST=ssh://usuario@host

DONE
ok "listo"
