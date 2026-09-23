#!/bin/sh
# install.sh — instala kling (CLI) desde GitHub Releases.
#
# USO
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --tag v0.1.0 --prefix ~/.local
#   curl -fsSL .../install.sh | sh -s -- --dry-run
#
# Comportamiento:
#   - Detecta OS (linux/darwin) y arch (amd64/arm64).
#   - Descarga el binario CLI correspondiente de la release.
#   - Verifica SHA256 contra SHA256SUMS publicado en la misma release.
#   - kling-bridge es de kindling-mcp desde v0.6: su install.sh lo trae.
#   - Si se pasa --prefix DIR, instala en DIR; por defecto ~/.local/bin
#     (crea el directorio si no existe, sin pedir sudo si es del usuario).
#
# Plataformas soportadas:
#   - linux/amd64, linux/arm64 (CLI + daemon)
#   - darwin/amd64 (solo CLI) y darwin/arm64 (CLI y daemon con kling-vz)
#   - Windows: NO soportado (el código usa syscall.Kill, Setsid, Stat_t que son POSIX).
#
# Variables de entorno respetadas:
#   KLING_VERSION   versión a instalar (ej. v0.1.0). Por defecto: última estable.
#   KLING_PREFIX    directorio de instalación. Por defecto: ~/.local/bin
#   KLING_REPO      repo de donde descargar. Por defecto: juan52878911/kindling

set -u
# NOTA: usamos `set -u` pero NO `set -e`. Los tests `[ ... ]` que comparan
# variables que aún no hemos inicializado devuelven false (1), y con `set -e`
# eso abortaría el script. Manejamos los errores a mano con `||`/`if`.

REPO="${KLING_REPO:-juan52878911/kindling}"
PREFIX="${KLING_PREFIX:-${HOME}/.local/bin}"
TAG=""
DRY_RUN=0

usage() {
    cat <<EOF
Uso: install.sh [opciones]

  --tag VER          instala una versión concreta (ej. v0.1.0). Por defecto: última.
  --prefix DIR       directorio destino (por defecto: ~/.local/bin)
  --repo OWNER/NAME  repo de GitHub (por defecto: juan52878911/kindling)
  --dry-run          muestra lo que haría sin descargar ni instalar nada
  -h, --help         muestra esta ayuda

Variables de entorno equivalentes: KLING_VERSION, KLING_PREFIX, KLING_REPO.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tag)       TAG="$2"; shift 2 ;;
        --prefix)    PREFIX="$2"; shift 2 ;;
        --bridge)    printf "  ✗ kling-bridge es de kindling-mcp desde v0.6: https://github.com/juan52878911/kindling-mcp\n" >&2; exit 1 ;;
        --repo)      REPO="$2"; shift 2 ;;
        --dry-run)   DRY_RUN=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)           echo "opción desconocida: $1" >&2; usage >&2; exit 2 ;;
    esac
done

# ── detección de plataforma ─────────────────────────────────────────────────
detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m)"

    case "$OS" in
        linux)  PLAT_OS="linux" ;;
        darwin) PLAT_OS="darwin" ;;
        *)
            echo "Sistema operativo no soportado: $OS" >&2
            echo "kling corre en Linux y macOS. Windows no está soportado." >&2
            exit 1
            ;;
    esac

    case "$ARCH" in
        x86_64|amd64)  PLAT_ARCH="amd64" ;;
        aarch64|arm64) PLAT_ARCH="arm64" ;;
        *)
            echo "Arquitectura no soportada: $ARCH" >&2
            exit 1
            ;;
    esac

    PLAT="${PLAT_OS}-${PLAT_ARCH}"
    BIN_NAME="kling-${PLAT}"
}

detect_platform
[ -n "${KLING_VERSION:-}" ] && TAG="$KLING_VERSION"

# ── resolver tag (latest si no se pasó uno) ────────────────────────────────
resolve_tag() {
    if [ -n "$TAG" ]; then
        return
    fi
    # GitHub redirige /latest a la release estable más reciente
    LATEST_URL="https://github.com/${REPO}/releases/latest"
    if command -v curl >/dev/null 2>&1; then
        TAG=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$LATEST_URL" | sed 's|.*/||')
    fi
    if [ -z "$TAG" ] || [ "$TAG" = "latest" ]; then
        echo "No pude resolver la última release. Pasa --tag vX.Y.Z." >&2
        exit 1
    fi
}

resolve_tag

# ── utilidades ─────────────────────────────────────────────────────────────
info() { printf '  • %s\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }
fail() { printf '  ✗ %s\n' "$*" >&2; exit 1; }

# Detecta una herramienta de descarga y la usa. Prefiere curl por ser
# ubicuo; cae a wget si curl falta.
fetch() {
    URL="$1"
    DEST="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$DEST" "$URL"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --tries=3 -O "$DEST" "$URL"
    else
        fail "ni curl ni wget disponibles"
    fi
}

# macOS no trae sha256sum, sí shasum: sin esto la verificación fallaba en el Mac.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# ── trabajo ────────────────────────────────────────────────────────────────
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

BASE="https://github.com/${REPO}/releases/download/${TAG}"

echo
echo "kling ${TAG} — instalación"
echo "  plataforma:  ${PLAT}"
echo "  destino:     ${PREFIX}"
echo

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) NO descargo ni instalo nada."
    echo "Descargaría:"
    info "$BASE/$BIN_NAME"
    [ "$PLAT" = "darwin-arm64" ] && info "$BASE/kling-vz-darwin-arm64"
    info "$BASE/SHA256SUMS"
    exit 0
fi

# Descargar binario CLI + suma de verificación
info "descargando $BIN_NAME"
fetch "$BASE/$BIN_NAME" "$WORK/$BIN_NAME"
ok "$BIN_NAME (${PLAT})"

info "descargando SHA256SUMS"
fetch "$BASE/SHA256SUMS" "$WORK/SHA256SUMS"
ok "SHA256SUMS"

# Verificar integridad
EXPECTED="$(grep -E "  ${BIN_NAME}\$" "$WORK/SHA256SUMS" | awk '{print $1}')"
if [ -z "$EXPECTED" ]; then
    fail "no encuentro $BIN_NAME en SHA256SUMS"
fi
ACTUAL="$(sha256_of "$WORK/$BIN_NAME")"
if [ -z "$ACTUAL" ]; then
    fail "could not compute sha256 of $BIN_NAME (is sha256sum missing?)"
fi
if [ "$EXPECTED" != "$ACTUAL" ]; then
    fail "checksum no coincide (esperaba $EXPECTED, obtuve $ACTUAL)"
fi
ok "checksum verificado"

# Preparar directorio destino
if [ ! -d "$PREFIX" ]; then
    if mkdir -p "$PREFIX" 2>/dev/null; then
        ok "creado $PREFIX"
    else
        fail "no puedo crear $PREFIX — prueba --prefix o ejecuta como root"
    fi
fi

# Mover a destino
chmod +x "$WORK/$BIN_NAME"
mv "$WORK/$BIN_NAME" "$PREFIX/kling"
ok "instalado en $PREFIX/kling"

# En un Mac Apple Silicon el daemon arranca las microVMs con kling-vz, que tiene
# que vivir junto a kling. Va firmado ad-hoc y sin notarizar: la cuarentena que
# pone la descarga haría que macOS se negara a ejecutarlo, así que se quita.
if [ "$PLAT" = "darwin-arm64" ]; then
    VZ_NAME="kling-vz-darwin-arm64"
    info "descargando $VZ_NAME"
    fetch "$BASE/$VZ_NAME" "$WORK/$VZ_NAME"
    EXPECTED="$(grep -E "  ${VZ_NAME}\$" "$WORK/SHA256SUMS" | awk '{print $1}')"
    [ -n "$EXPECTED" ] || fail "no encuentro $VZ_NAME en SHA256SUMS"
    [ "$EXPECTED" = "$(sha256_of "$WORK/$VZ_NAME")" ] || fail "checksum de $VZ_NAME no coincide"
    chmod +x "$WORK/$VZ_NAME"
    xattr -d com.apple.quarantine "$WORK/$VZ_NAME" 2>/dev/null || true
    mv "$WORK/$VZ_NAME" "$PREFIX/kling-vz"
    ok "instalado en $PREFIX/kling-vz (backend nativo de macOS)"
fi

cat <<EOF

  Para usar kling desde una shell abierta, asegúrate de que $PREFIX
  está en tu PATH. Si no lo está, añade:

      export PATH="$PREFIX:\$PATH"

  a tu ~/.zshrc o ~/.bashrc y abre un terminal nuevo.

  Comprobar:

      kling version
      kling --help

  Para conectar con un daemon remoto:

      kling context add lab ssh://juan@<host-con-kvm>

EOF

ok "listo"