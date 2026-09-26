#!/bin/sh
# install.sh — instala kling (CLI) y, si se piden, sus extensiones desde GitHub Releases.
#
# USO
#   curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --tag v0.13.0 --prefix ~/.local/bin
#   curl -fsSL .../install.sh | sh -s -- --with mcp,sandbox
#   curl -fsSL .../install.sh | sh -s -- --dry-run
#
# Comportamiento:
#   - Detecta OS (linux/darwin) y arch (amd64/arm64).
#   - Descarga el binario CLI correspondiente de la release.
#   - Verifica SHA256 contra SHA256SUMS publicado en la misma release, ANTES de
#     mover nada a su sitio.
#   - Si se pasa --prefix DIR, instala en DIR; por defecto ~/.local/bin
#     (crea el directorio si no existe, sin pedir sudo si es del usuario).
#   - --with mcp,sandbox baja además esas extensiones de LA MISMA
#     release (todo lo de una release es compatible entre sí), las verifica con
#     el mismo SHA256SUMS y las deja en el directorio de extensiones, igual que
#     `kling plugins install <n>`: primer directorio de $KLING_PLUGIN_PATH; si
#     no, $XDG_DATA_HOME/kling/plugins; si no, ~/.local/share/kling/plugins.
#     Junto a cada una escribe kling-<n>.json con de dónde salió.
#     Los ejecutables compañeros (kling-bridge para mcp) van junto a kling en
#     --prefix, que es donde los busca quien los usa (el PATH).
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
#   KLING_WITH      extensiones a instalar, como --with (ej. mcp,sandbox)
#   KLING_PLUGIN_PATH  su primer directorio es donde van las extensiones

set -u
# NOTA: usamos `set -u` pero NO `set -e`. Los tests `[ ... ]` que comparan
# variables que aún no hemos inicializado devuelven false (1), y con `set -e`
# eso abortaría el script. Manejamos los errores a mano con `||`/`if`.

REPO="${KLING_REPO:-juan52878911/kindling}"
PREFIX="${KLING_PREFIX:-${HOME}/.local/bin}"
TAG=""
DRY_RUN=0
WITH="${KLING_WITH:-}"
PLUGIN_DIR=""
SKIP_KLING=0
NO_COMPANIONS=0

usage() {
    cat <<EOF
Uso: install.sh [opciones]

  --tag VER          instala una versión concreta (ej. v0.1.0). Por defecto: última.
  --prefix DIR       directorio destino (por defecto: ~/.local/bin)
  --repo OWNER/NAME  repo de GitHub (por defecto: juan52878911/kindling)
  --with LISTA       instala también estas extensiones de la misma release,
                     separadas por comas: mcp, sandbox
  --plugin-dir DIR   dónde dejar las extensiones (por defecto el directorio de
                     extensiones de kling: ~/.local/share/kling/plugins)
  --skip-kling       no instala kling: solo las extensiones de --with
  --no-companions    no instala los compañeros de las extensiones (kling-bridge)
  --dry-run          muestra lo que haría sin descargar ni instalar nada
  -h, --help         muestra esta ayuda

Variables de entorno equivalentes: KLING_VERSION, KLING_PREFIX, KLING_REPO,
KLING_WITH.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tag)       TAG="$2"; shift 2 ;;
        --prefix)    PREFIX="$2"; shift 2 ;;
        # --bridge era la opción de antes de v0.6; el puente vuelve a estar en
        # esta release como compañero de la extensión mcp.
        --bridge)    WITH="${WITH:+$WITH,}mcp"; shift ;;
        --repo)      REPO="$2"; shift 2 ;;
        --with)      WITH="${WITH:+$WITH,}$2"; shift 2 ;;
        --with=*)    WITH="${WITH:+$WITH,}${1#--with=}"; shift ;;
        --plugin-dir) PLUGIN_DIR="$2"; shift 2 ;;
        --skip-kling) SKIP_KLING=1; shift ;;
        --no-companions) NO_COMPANIONS=1; shift ;;
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

# ── extensiones pedidas con --with ──────────────────────────────────────────
# El catálogo es corto y fijo a propósito: son las extensiones que publica esta
# misma release (kling-<n>-<os>-<arch>). Una de fuera se instala con
# `kling plugins install <n> -from URL`.
EXTS=""
for e in $(printf '%s' "$WITH" | tr ',' ' '); do
    case "$e" in
        mcp|sandbox) ;;
        *) echo "extensión desconocida en --with: $e (válidas: mcp, sandbox)" >&2; exit 2 ;;
    esac
    # Sin repetidas: --bridge y --with mcp a la vez no la instalan dos veces.
    case " $EXTS " in *" $e "*) ;; *) EXTS="${EXTS:+$EXTS }$e" ;; esac
done
if [ "$SKIP_KLING" = "1" ] && [ -z "$EXTS" ]; then
    echo "--skip-kling sin --with no instala nada" >&2
    exit 2
fi

# Ejecutables que acompañan a cada extensión (el campo companions de su
# manifiesto). kling-bridge expone por HTTP un MCP de stdio local.
companions_of() {
    case "$1" in
        mcp) echo "kling-bridge" ;;
        *)   echo "" ;;
    esac
}

# El directorio de extensiones, en el mismo orden que `kling plugins install`.
if [ -z "$PLUGIN_DIR" ]; then
    if [ -n "${KLING_PLUGIN_PATH:-}" ]; then
        PLUGIN_DIR="${KLING_PLUGIN_PATH%%:*}"
    fi
    if [ -n "$PLUGIN_DIR" ]; then
        :
    elif [ -n "${XDG_DATA_HOME:-}" ]; then
        PLUGIN_DIR="${XDG_DATA_HOME}/kling/plugins"
    else
        PLUGIN_DIR="${HOME}/.local/share/kling/plugins"
    fi
fi

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
warn() { printf '  ! %s\n' "$*" >&2; }
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
[ -n "$EXTS" ] && echo "  extensiones: ${EXTS} (en ${PLUGIN_DIR})"
echo

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) NO descargo ni instalo nada."
    echo "Descargaría:"
    info "$BASE/SHA256SUMS"
    if [ "$SKIP_KLING" != "1" ]; then
        info "$BASE/$BIN_NAME"
        [ "$PLAT" = "darwin-arm64" ] && info "$BASE/kling-vz-darwin-arm64"
    fi
    for e in $EXTS; do
        info "$BASE/kling-${e}-${PLAT}  ->  $PLUGIN_DIR/kling-$e"
        if [ "$NO_COMPANIONS" != "1" ]; then
            for c in $(companions_of "$e"); do
                info "$BASE/${c}-${PLAT}  ->  $PREFIX/$c"
            done
        fi
    done
    exit 0
fi

# verify NOMBRE: comprueba $WORK/NOMBRE contra SHA256SUMS. Las dos guardas de
# vacío hacen falta: sin set -e, una herramienta que falte deja ACTUAL vacío, y
# "" = "" daría por bueno un binario sin verificar.
verify() {
    EXPECTED="$(grep -E "  ${1}\$" "$WORK/SHA256SUMS" | awk '{print $1}')"
    [ -n "$EXPECTED" ] || fail "no encuentro $1 en SHA256SUMS"
    ACTUAL="$(sha256_of "$WORK/$1")"
    [ -n "$ACTUAL" ] || fail "no puedo calcular el sha256 de $1 (falta sha256sum/shasum)"
    [ "$EXPECTED" = "$ACTUAL" ] || fail "checksum de $1 no coincide (esperaba $EXPECTED, obtuve $ACTUAL)"
}

# get NOMBRE: descarga y verifica. Nada sale de $WORK sin pasar por aquí.
get() {
    info "descargando $1"
    fetch "$BASE/$1" "$WORK/$1" || fail "no pude descargar $BASE/$1"
    verify "$1"
    ok "$1 verificado"
}

info "descargando SHA256SUMS"
fetch "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" || fail "no pude descargar SHA256SUMS"
ok "SHA256SUMS"

# Todo se baja y se verifica ANTES de instalar nada: un fallo a medias no deja
# un kling nuevo con extensiones viejas.
if [ "$SKIP_KLING" != "1" ]; then
    get "$BIN_NAME"
    [ "$PLAT" = "darwin-arm64" ] && get "kling-vz-darwin-arm64"
fi
for e in $EXTS; do
    get "kling-${e}-${PLAT}"
    if [ "$NO_COMPANIONS" != "1" ]; then
        for c in $(companions_of "$e"); do
            get "${c}-${PLAT}"
        done
    fi
done

# ── comprobaciones antes de tocar nada ──────────────────────────────────────
# Compara "X.Y.Z" por sus tres números: ¿$1 >= $2?
version_ge() {
    [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}

# Con --skip-kling el kling que ya hay puede ser más viejo que la extensión: se
# compara con el min_kling de su manifiesto, como hacía el instalador de
# kindling-mcp. Si kling viene de esta misma release no hace falta.
check_min_kling() {
    [ "$SKIP_KLING" = "1" ] || return 0
    MIN="$("$1" --kling-manifest 2>/dev/null | tr -d '\n' | grep -o '"min_kling": *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/')"
    if ! command -v kling >/dev/null 2>&1; then
        warn "no encuentro kling en el PATH. Las extensiones son de kling: instálalo ${MIN:+(>= v$MIN) }quitando --skip-kling."
        return 0
    fi
    [ -n "$MIN" ] || return 0
    HAVE="$(kling version 2>/dev/null | awk 'NR==1{print $2}' | sed 's/^v//; s/[-+].*//')"
    case "$HAVE" in
        [0-9]*.[0-9]*)
            version_ge "$HAVE" "$MIN" || fail "tu kling es $HAVE y $(basename "$1") necesita >= $MIN. Quita --skip-kling para actualizarlo." ;;
        *) warn "no sé leer la versión de kling (\"$HAVE\"); sigo sin comprobar el mínimo ($MIN)" ;;
    esac
}

for e in $EXTS; do
    chmod +x "$WORK/kling-${e}-${PLAT}"
    check_min_kling "$WORK/kling-${e}-${PLAT}"
done

# Preparar directorio destino
if [ ! -d "$PREFIX" ]; then
    if mkdir -p "$PREFIX" 2>/dev/null; then
        ok "creado $PREFIX"
    else
        fail "no puedo crear $PREFIX — prueba --prefix o ejecuta como root"
    fi
fi

if [ "$SKIP_KLING" != "1" ]; then
    chmod +x "$WORK/$BIN_NAME"
    mv "$WORK/$BIN_NAME" "$PREFIX/kling" || fail "no puedo escribir en $PREFIX"
    ok "instalado en $PREFIX/kling"

    # En un Mac Apple Silicon el daemon arranca las microVMs con kling-vz, que
    # tiene que vivir junto a kling. Va firmado ad-hoc y sin notarizar: la
    # cuarentena que pone la descarga haría que macOS se negara a ejecutarlo,
    # así que se quita.
    if [ "$PLAT" = "darwin-arm64" ]; then
        VZ_NAME="kling-vz-darwin-arm64"
        chmod +x "$WORK/$VZ_NAME"
        xattr -d com.apple.quarantine "$WORK/$VZ_NAME" 2>/dev/null || true
        mv "$WORK/$VZ_NAME" "$PREFIX/kling-vz" || fail "no puedo escribir en $PREFIX"
        ok "instalado en $PREFIX/kling-vz (backend nativo de macOS)"
    fi
fi

# ── extensiones ────────────────────────────────────────────────────────────
if [ -n "$EXTS" ]; then
    mkdir -p "$PLUGIN_DIR" 2>/dev/null || fail "no puedo crear $PLUGIN_DIR — prueba --plugin-dir"
    NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi
for e in $EXTS; do
    ASSET="kling-${e}-${PLAT}"
    SUM="$(sha256_of "$WORK/$ASSET")"
    mv "$WORK/$ASSET" "$PLUGIN_DIR/kling-$e" || fail "no puedo escribir en $PLUGIN_DIR"
    # Lo mismo que deja `kling plugins install`: de dónde salió y con qué suma.
    # Empieza por kling- pero acaba en .json, así que el descubrimiento de
    # extensiones no lo confunde con un ejecutable.
    printf '{"name":"%s","version":"%s","url":"%s","sha256":"%s","installed":"%s"}\n' \
        "$e" "$TAG" "$BASE/$ASSET" "$SUM" "$NOW" > "$PLUGIN_DIR/kling-$e.json" \
        || fail "no puedo escribir $PLUGIN_DIR/kling-$e.json"
    ok "instalado en $PLUGIN_DIR/kling-$e"
    if [ "$NO_COMPANIONS" != "1" ]; then
        for c in $(companions_of "$e"); do
            chmod +x "$WORK/${c}-${PLAT}"
            mv "$WORK/${c}-${PLAT}" "$PREFIX/$c" || fail "no puedo escribir en $PREFIX"
            ok "instalado en $PREFIX/$c"
        done
    fi
done

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

if [ -n "$EXTS" ]; then
cat <<EOF
  Extensiones instaladas en $PLUGIN_DIR:

      kling plugins ls

  Recarga el completado de la shell para ver los comandos nuevos:

      source <(kling completion zsh)

EOF
fi

ok "listo"