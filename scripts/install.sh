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
#     mover nada a su sitio. Si la release trae SHA256SUMS.minisig y hay
#     `minisign`, comprueba antes la firma de SHA256SUMS con las claves de
#     KLING_RELEASE_PUBKEYS (docs/firmas.md). Sin minisign la comprueba el propio
#     kling recién bajado (`kling up -verify-sums`, Go puro): no hace falta
#     instalar nada. Una 0.x sin firma se instala con aviso (solo sha256);
#     desde v1.0.0 (un -rc incluido) la firma es obligatoria, salvo
#     KLING_INSECURE_NO_MINISIGN=1.
#   - Si se pasa --prefix DIR, instala en DIR; por defecto ~/.local/bin
#     (crea el directorio si no existe, sin pedir sudo si es del usuario).
#   - --with mcp,sandbox baja además esas extensiones de LA MISMA
#     release (todo lo de una release es compatible entre sí), las verifica con
#     el mismo SHA256SUMS y las deja en el directorio de extensiones, igual que
#     `kling plugin install <n>`: primer directorio de $KLING_PLUGIN_PATH; si
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
# Cada binario se escribe en un fichero temporal DEL MISMO DIRECTORIO y se
# renombra encima (rename es atómico): sobrescribir un kling en marcha en su
# sitio lo mataba en macOS (la firma de código deja de cuadrar con el fichero
# abierto) y dejaba a medias a quien actualizaba desde una shell con kling
# corriendo.
#
# Sobre una instalación que ya existe (hay un kling en --prefix) no reinstala
# kling sin --force: actualizar es `kling upgrade`, que además para y arranca el
# daemon, verifica que el nuevo responde y vuelve atrás solo si no
# (docs/actualizar.md). --force reinstala con este script, como antes. Con
# --with y sin --tag sí sigue: añade esas extensiones y deja kling como está
# (lo mismo que --skip-kling).
#
# Al final deja `kling doctor` en verde: instala el completado de tu shell en
# ~/.config/kling y añade a tu rc la línea que lo carga y el PATH de --prefix
# si faltaba (una sola vez, bajo un comentario "# kling"). Con --no-rc no toca
# nada fuera de --prefix (ni el rc ni ~/.config/kling): imprime las líneas y ya.
#
# Variables de entorno respetadas:
#   KLING_VERSION   versión a instalar (ej. v0.1.0). Por defecto: última estable.
#   KLING_PREFIX    directorio de instalación. Por defecto: ~/.local/bin
#   KLING_REPO      repo de donde descargar. Por defecto: juan52878911/kindling
#   KLING_WITH      extensiones a instalar, como --with (ej. mcp,sandbox)
#   KLING_PLUGIN_PATH  su primer directorio es donde van las extensiones
#   KLING_NO_RC     =1 no toca tu rc (como --no-rc)
#   KLING_INSECURE_NO_MINISIGN  =1 salta la comprobación de la firma y deja solo
#                   el sha256 (sin la firma no se sabe de dónde viene el
#                   SHA256SUMS: solo si no hay forma de comprobarla)

set -u
# NOTA: usamos `set -u` pero NO `set -e`. Los tests `[ ... ]` que comparan
# variables que aún no hemos inicializado devuelven false (1), y con `set -e`
# eso abortaría el script. Manejamos los errores a mano con `||`/`if`.

# Todo el script va dentro de main y la única orden de nivel superior es la
# última línea: si la descarga del script se corta a medias, la shell no llega
# a ver el `main "$@"` y no ejecuta nada.
main() {

REPO="${KLING_REPO:-juan52878911/kindling}"
PREFIX="${KLING_PREFIX:-${HOME}/.local/bin}"
TAG=""
# TAG_LATEST=1 cuando la etiqueta sale de /releases/latest y no de --tag o
# KLING_VERSION: entonces la firma se exige aunque sea una 0.x.
TAG_LATEST=0
DRY_RUN=0
WITH="${KLING_WITH:-}"
PLUGIN_DIR=""
SKIP_KLING=0
NO_COMPANIONS=0
NO_RC="${KLING_NO_RC:-0}"
FORCE=0

# Claves públicas con las que se firman las releases (minisign, separadas por
# espacios): las mismas que clavesRelease de internal/upgrade/firma.go, lo
# comprueba un test (docs/firmas.md).
KLING_RELEASE_PUBKEYS="RWQ9r2LB7j2GE1A0GedCeER+ZFHmX4E1NPubT39GBQDho/awRnY6WpVN"
# Desde esta versión mayor una release sin firma se rechaza:
# upgrade.FirmaObligatoriaDesde (v1.0.0). Una 0.x sin firma solo se instala
# pidiéndola con --tag (o KLING_VERSION): nunca por llegar como «latest».
SIGNED_FROM_MAJOR=1

usage() {
    cat <<EOF
Usage: install.sh [options]

  --tag VER          install a specific version (e.g. v0.1.0). Default: the latest.
  --prefix DIR       destination directory (default: ~/.local/bin)
  --repo OWNER/NAME  GitHub repo (default: juan52878911/kindling)
  --with LIST        also install these extensions from the same release,
                     comma separated: mcp, sandbox
  --plugin-dir DIR   where to put the extensions (default: kling's extension
                     directory: ~/.local/share/kling/plugins)
  --skip-kling       don't install kling: only the extensions of --with
  --no-companions    don't install the extensions' companions (kling-bridge)
  --no-rc            touch nothing outside --prefix: neither your rc file nor the
                     completion in ~/.config/kling; print what you would do
  --force            reinstall even if there is a kling in --prefix (to
                     upgrade, prefer: kling upgrade)
  --dry-run          show what it would do without downloading or installing
  -h, --help         show this help

Equivalent environment variables: KLING_VERSION, KLING_PREFIX, KLING_REPO,
KLING_WITH. The signature of SHA256SUMS is checked with minisign if you have it
and, if not, with the kling it just downloaded: you don't need to install
anything. KLING_INSECURE_NO_MINISIGN=1 skips the check (sha256 only).
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
        --no-rc)     NO_RC=1; shift ;;
        --dry-run)   DRY_RUN=1; shift ;;
        --force)     FORCE=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)           echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
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
            echo "Unsupported operating system: $OS" >&2
            echo "kling runs on Linux and macOS. Windows is not supported." >&2
            exit 1
            ;;
    esac

    case "$ARCH" in
        x86_64|amd64)  PLAT_ARCH="amd64" ;;
        aarch64|arm64) PLAT_ARCH="arm64" ;;
        *)
            echo "Unsupported architecture: $ARCH" >&2
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
# `kling plugin install <n> -from URL`.
EXTS=""
for e in $(printf '%s' "$WITH" | tr ',' ' '); do
    case "$e" in
        mcp|sandbox|db|phone) ;;
        *) echo "unknown extension in --with: $e (valid: mcp, sandbox, db, phone)" >&2; exit 2 ;;
    esac
    # Sin repetidas: --bridge y --with mcp a la vez no la instalan dos veces.
    case " $EXTS " in *" $e "*) ;; *) EXTS="${EXTS:+$EXTS }$e" ;; esac
done
if [ "$SKIP_KLING" = "1" ] && [ -z "$EXTS" ]; then
    echo "--skip-kling without --with installs nothing" >&2
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

# El directorio de extensiones, en el mismo orden que `kling plugin install`.
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
        TAG=$(curl -fsSL --proto '=https' --proto-redir '=https' -o /dev/null -w '%{url_effective}' "$LATEST_URL" | sed 's|.*/||')
    fi
    TAG_LATEST=1
    if [ -z "$TAG" ] || [ "$TAG" = "latest" ]; then
        echo "Could not resolve the latest release. Pass --tag vX.Y.Z." >&2
        exit 1
    fi
}

# ── instalación existente: eso es kling upgrade ─────────────────────────────
# Va antes de resolver la etiqueta: no hace falta red para decir que no.
# La versión se pregunta sin daemon: KLING_HOST a ninguna parte, para que
# `kling version` no hable con uno ni abra un ssh del contexto activo.
if [ "$SKIP_KLING" != "1" ] && [ "$FORCE" != "1" ] && [ -x "$PREFIX/kling" ] \
   && [ -n "$EXTS" ] && [ -z "$TAG" ]; then
    echo "There is already a kling in $PREFIX/kling: leaving it as is and installing only $EXTS (to upgrade it: kling upgrade)."
    SKIP_KLING=1
fi
if [ "$SKIP_KLING" != "1" ] && [ "$FORCE" != "1" ] && [ -x "$PREFIX/kling" ]; then
    HAVE_VER="$(KLING_HOST=unix:///nonexistent/kling-install.sock "$PREFIX/kling" version 2>/dev/null | awk 'NR==1{print $2}')"
    echo "There is already a kling ${HAVE_VER:-} in $PREFIX/kling." >&2
    echo "To upgrade it:  kling upgrade${TAG:+ -tag $TAG}   (with the daemon: sudo kling upgrade on Linux)" >&2
    echo "  it downloads and verifies the release, stops the daemon, swaps, starts, checks and" >&2
    echo "  rolls back by itself if something fails. A kling older than v0.18 lacks it:" >&2
    echo "  run this script again with --force." >&2
    exit 1
fi

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
        curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 -o "$DEST" "$URL"
    elif command -v wget >/dev/null 2>&1; then
        # --https-only es el --proto-redir de wget: una redirección no puede
        # llevar la descarga a http. Un wget sin la opción (busybox) no vale.
        wget --help 2>&1 | grep -q -- '--https-only' \
            || fail "this wget has no --https-only: install curl"
        wget -q --https-only --tries=3 -O "$DEST" "$URL"
    else
        fail "neither curl nor wget is available"
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
echo "kling ${TAG} — install"
echo "  platform:    ${PLAT}"
echo "  target:      ${PREFIX}"
[ -n "$EXTS" ] && echo "  extensions:  ${EXTS} (in ${PLUGIN_DIR})"
echo

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) NOT downloading or installing anything."
    echo "Would download:"
    info "$BASE/SHA256SUMS"
    info "$BASE/SHA256SUMS.minisig (if the release has it)"
    if [ "$SKIP_KLING" != "1" ]; then
        info "$BASE/$BIN_NAME"
        [ "$PLAT" = "darwin-arm64" ] && info "$BASE/kling-guest-linux-arm64  ->  \$KLING_ROOT/lib/kling-guest (if the release has it)"
        [ "$PLAT" = "darwin-arm64" ] && info "$BASE/kling-vz-darwin-arm64" && info "$BASE/kling-hv-darwin-arm64 (if the release has it)" && info "$BASE/kling-credproxy-darwin-arm64 (if the release has it)"
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
    [ -n "$EXPECTED" ] || fail "cannot find $1 in SHA256SUMS"
    ACTUAL="$(sha256_of "$WORK/$1")"
    [ -n "$ACTUAL" ] || fail "cannot compute the sha256 of $1 (sha256sum/shasum missing)"
    [ "$EXPECTED" = "$ACTUAL" ] || fail "checksum of $1 does not match (expected $EXPECTED, got $ACTUAL)"
}

# place ORIGEN DESTINO: deja ORIGEN en DESTINO sin tocar el fichero que ya
# hubiera hasta el último instante. Copia a un temporal del mismo directorio y
# lo renombra encima: rename(2) es atómico, y el kling que estuviera corriendo
# sigue con su inodo viejo en vez de ver cómo le cambian el contenido debajo
# (en macOS eso lo mata: "Killed: 9" por la firma de código).
place() {
    DEST_DIR="$(dirname "$2")"
    TMP="$DEST_DIR/.$(basename "$2").tmp.$$"
    cp "$1" "$TMP" 2>/dev/null || return 1
    chmod +x "$TMP"
    mv -f "$TMP" "$2" || { rm -f "$TMP"; return 1; }
}

# get NOMBRE: descarga y verifica. Nada sale de $WORK sin pasar por aquí.
get() {
    [ -f "$WORK/$1" ] && return 0   # already downloaded and verified (signature check)
    info "downloading $1"
    fetch "$BASE/$1" "$WORK/$1" || fail "could not download $BASE/$1"
    verify "$1"
    ok "$1 verified"
}

# verify_sums_signature: comprueba SHA256SUMS.minisig contra
# KLING_RELEASE_PUBKEYS con minisign, y que se firmó para esta etiqueta (el
# tag: del comentario de confianza; si no, el SHA256SUMS firmado de otra
# release valdría para colar sus binarios). Sin firma: error desde
# SIGNED_FROM_MAJOR; antes, aviso solo si la 0.x se pidió con --tag (una que
# llega como «latest» o una etiqueta que no se entiende exigen la firma).
# Sin minisign, igual: error desde SIGNED_FROM_MAJOR (como `kling upgrade`,
# que no instala sin firma comprobada), salvo KLING_INSECURE_NO_MINISIGN=1; antes, aviso y solo sha256.
verify_sums_signature() {
    TAG_MAJOR="$(printf '%s' "${TAG#v}" | cut -d. -f1)"
    REQUIRED=0
    case "$TAG_MAJOR" in
        ''|*[!0-9]*) REQUIRED=1 ;;
        *) [ "$TAG_MAJOR" -ge "$SIGNED_FROM_MAJOR" ] && REQUIRED=1 ;;
    esac
    # Una 0.x como «latest» sin firma se instala con aviso: las 0.x nunca se
    # firmaron, y es lo que resuelve el one-liner del README mientras la 1.x no
    # sea la «latest». Exigirla lo rompía.
    if ! fetch "$BASE/SHA256SUMS.minisig" "$WORK/SHA256SUMS.minisig" 2>/dev/null; then
        rm -f "$WORK/SHA256SUMS.minisig"
        [ "$REQUIRED" = "1" ] && fail "release $TAG has no SHA256SUMS.minisig: releases are signed from v${SIGNED_FROM_MAJOR}.0.0 on (docs/firmas.md)"
        warn "release $TAG has no SHA256SUMS.minisig: checking with sha256 only (docs/firmas.md)"
        return 0
    fi
    if [ -z "$KLING_RELEASE_PUBKEYS" ]; then
        [ "$REQUIRED" = "1" ] && fail "this install.sh has no release public keys: use the one from that release"
        warn "this install.sh has no release public key: checking with sha256 only"
        return 0
    fi
    if command -v minisign >/dev/null 2>&1; then
        for K in $KLING_RELEASE_PUBKEYS; do
            if OUT="$(minisign -V -P "$K" -m "$WORK/SHA256SUMS" -x "$WORK/SHA256SUMS.minisig" 2>&1)"; then
                printf '%s\n' "$OUT" | tr '\t' '\n' | grep -qx "tag:${TAG}" \
                    || fail "SHA256SUMS.minisig is not signed for $TAG"
                ok "SHA256SUMS signature (minisign)"
                return 0
            fi
        done
        fail "the SHA256SUMS signature is not valid with any release key: installing nothing"
    fi
    if [ "${KLING_INSECURE_NO_MINISIGN:-0}" = "1" ]; then
        warn "skipping the signature check (KLING_INSECURE_NO_MINISIGN=1): sha256 only"
        return 0
    fi
    # Sin minisign: el propio kling que se acaba de bajar (ya comprobado contra
    # SHA256SUMS) lleva el verificador en Go y las mismas claves que este script
    # le pasa. No es independiente del binario que se instala, como sí lo es
    # minisign; es lo que hay sin pedirle nada más a quien instala.
    info "checking the SHA256SUMS signature with the kling just downloaded (no minisign needed)"
    get "$BIN_NAME"
    chmod +x "$WORK/$BIN_NAME"
    OUT="$("$WORK/$BIN_NAME" up -verify-sums "$WORK/SHA256SUMS" -sig "$WORK/SHA256SUMS.minisig" -tag "$TAG" -key "$KLING_RELEASE_PUBKEYS" 2>&1)"
    RC=$?
    if [ "$RC" = "0" ]; then
        ok "SHA256SUMS signature (checked by kling)"
        return 0
    fi
    if [ "$RC" != "2" ]; then
        fail "the SHA256SUMS signature check failed: $OUT"
    fi
    # 2: este kling no sabe verificar (una release anterior).
    [ "$REQUIRED" = "1" ] && fail "cannot check the SHA256SUMS signature: this kling cannot do it and minisign is not installed (brew install minisign / apt install minisign)"
    warn "cannot check the SHA256SUMS signature (no minisign): sha256 only"
    return 0
}

info "downloading SHA256SUMS"
fetch "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" || fail "could not download SHA256SUMS"
verify_sums_signature
ok "SHA256SUMS"

# Todo se baja y se verifica ANTES de instalar nada: un fallo a medias no deja
# un kling nuevo con extensiones viejas.
if [ "$SKIP_KLING" != "1" ]; then
    get "$BIN_NAME"
    if [ "$PLAT" = "darwin-arm64" ]; then
        get "kling-vz-darwin-arm64"
        # kling-hv llega en las releases posteriores a v0.18: una anterior no
        # lo trae y se instala igual (el daemon usará kling-vz).
        if grep -Eq '[ *]kling-hv-darwin-arm64$' "$WORK/SHA256SUMS"; then
            get "kling-hv-darwin-arm64"
            HV_NAME="kling-hv-darwin-arm64"
        fi
        # kling-credproxy (el proxy de credenciales aparte de cada máquina)
        # llega después de v1.0.0-rc.2: sin él, el daemon no da credenciales
        # a un kling-vz que sabe reenviarlas y lo dice.
        if grep -Eq '[ *]kling-credproxy-darwin-arm64$' "$WORK/SHA256SUMS"; then
            get "kling-credproxy-darwin-arm64"
            CP_NAME="kling-credproxy-darwin-arm64"
        fi
        # El agente de invitado (kling-guest, de Linux) lo necesita el daemon
        # para construir imágenes (toolchain) o importar las de Docker: sin él
        # `kling try -image alpine` no arranca. Una release anterior no lo trae.
        if grep -Eq '[ *]kling-guest-linux-arm64$' "$WORK/SHA256SUMS"; then
            get "kling-guest-linux-arm64"
            GUEST_NAME="kling-guest-linux-arm64"
        fi
    fi
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

# ¿Versión $1 >= $2? Como version_ge, pero con prerelease: una 1.0.0-rc.2 es
# menor que la 1.0.0 final (quitarle el sufijo la daba por buena ante un
# min_kling 1.0.0). Entre dos prereleases de la misma base compara el sufijo
# por campos (rc.9 < rc.10). El +build no cuenta.
semver_ge() {
    SA="${1#v}"; SA="${SA%%+*}"
    SB="${2#v}"; SB="${SB%%+*}"
    SA_PRE=""; case "$SA" in *-*) SA_PRE="${SA#*-}" ;; esac
    SB_PRE=""; case "$SB" in *-*) SB_PRE="${SB#*-}" ;; esac
    SA="${SA%%-*}"; SB="${SB%%-*}"
    if [ "$SA" != "$SB" ]; then
        version_ge "$SA" "$SB"
        return
    fi
    [ -n "$SA_PRE" ] || return 0
    [ -n "$SB_PRE" ] || return 1
    [ "$(printf '%s\n%s\n' "$SB_PRE" "$SA_PRE" | sort -t. -k1,1 -k2,2n -k3,3n | head -n1)" = "$SB_PRE" ]
}

# Con --skip-kling el kling que ya hay puede ser más viejo que la extensión: se
# compara con el min_kling de su manifiesto, como hacía el instalador de
# kindling-mcp. Si kling viene de esta misma release no hace falta.
check_min_kling() {
    [ "$SKIP_KLING" = "1" ] || return 0
    MIN="$("$1" --kling-manifest 2>/dev/null | tr -d '\n' | grep -o '"min_kling": *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/')"
    if ! command -v kling >/dev/null 2>&1; then
        warn "kling is not in the PATH. The extensions are kling's: install it ${MIN:+(>= v$MIN) }by dropping --skip-kling."
        return 0
    fi
    [ -n "$MIN" ] || return 0
    HAVE="$(KLING_HOST=unix:///nonexistent/kling-install.sock kling version 2>/dev/null | awk 'NR==1{print $2}' | sed 's/^v//')"
    case "$HAVE" in
        [0-9]*.[0-9]*)
            semver_ge "$HAVE" "$MIN" || fail "your kling is $HAVE and $(basename "$1") needs >= $MIN. Drop --skip-kling to upgrade it." ;;
        *) warn "cannot read the kling version (\"$HAVE\"); continuing without checking the minimum ($MIN)" ;;
    esac
}

for e in $EXTS; do
    chmod +x "$WORK/kling-${e}-${PLAT}"
    check_min_kling "$WORK/kling-${e}-${PLAT}"
done

# Preparar directorio destino
if [ ! -d "$PREFIX" ]; then
    if mkdir -p "$PREFIX" 2>/dev/null; then
        ok "created $PREFIX"
    else
        fail "cannot create $PREFIX — try --prefix or run as root"
    fi
fi

if [ "$SKIP_KLING" != "1" ]; then
    place "$WORK/$BIN_NAME" "$PREFIX/kling" || fail "cannot write to $PREFIX"
    ok "installed $PREFIX/kling"

    # En un Mac Apple Silicon el daemon arranca las microVMs con kling-vz, que
    # tiene que vivir junto a kling. Va firmado ad-hoc y sin notarizar: la
    # cuarentena que pone la descarga haría que macOS se negara a ejecutarlo,
    # así que se quita.
    if [ "$PLAT" = "darwin-arm64" ]; then
        VZ_NAME="kling-vz-darwin-arm64"
        xattr -d com.apple.quarantine "$WORK/$VZ_NAME" 2>/dev/null || true
        place "$WORK/$VZ_NAME" "$PREFIX/kling-vz" || fail "cannot write to $PREFIX"
        ok "installed $PREFIX/kling-vz (native macOS backend)"
        # kling-hv (Hypervisor.framework), igual: junto a kling y sin
        # cuarentena. Con daemon.vmm auto el daemon lo elige si este macOS lo
        # admite (docs/mac.md).
        if [ -n "${HV_NAME:-}" ]; then
            xattr -d com.apple.quarantine "$WORK/$HV_NAME" 2>/dev/null || true
            place "$WORK/$HV_NAME" "$PREFIX/kling-hv" || fail "cannot write to $PREFIX"
            ok "installed $PREFIX/kling-hv (macOS hv backend)"
        fi
        # kling-credproxy, igual. Firmado con hardened runtime: place no lo
        # toca más que para copiarlo.
        if [ -n "${CP_NAME:-}" ]; then
            xattr -d com.apple.quarantine "$WORK/$CP_NAME" 2>/dev/null || true
            place "$WORK/$CP_NAME" "$PREFIX/kling-credproxy" || fail "cannot write to $PREFIX"
            ok "installed $PREFIX/kling-credproxy (separate credentials proxy)"
        fi
        # lib/ de la raíz de datos: donde el daemon de Mac busca kling-guest.
        if [ -n "${GUEST_NAME:-}" ]; then
            LIB_DIR="${KLING_ROOT:-$HOME/Library/Application Support/kindling}/lib"
            mkdir -p "$LIB_DIR" 2>/dev/null || fail "cannot create $LIB_DIR"
            place "$WORK/$GUEST_NAME" "$LIB_DIR/kling-guest" || fail "cannot write to $LIB_DIR"
            ok "installed $LIB_DIR/kling-guest (guest agent)"
        fi
    fi
fi

# ── extensiones ────────────────────────────────────────────────────────────
if [ -n "$EXTS" ]; then
    mkdir -p "$PLUGIN_DIR" 2>/dev/null || fail "cannot create $PLUGIN_DIR — try --plugin-dir"
    NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi
for e in $EXTS; do
    ASSET="kling-${e}-${PLAT}"
    SUM="$(sha256_of "$WORK/$ASSET")"
    place "$WORK/$ASSET" "$PLUGIN_DIR/kling-$e" || fail "cannot write to $PLUGIN_DIR"
    # Lo mismo que deja `kling plugin install`: de dónde salió y con qué suma.
    # Empieza por kling- pero acaba en .json, así que el descubrimiento de
    # extensiones no lo confunde con un ejecutable.
    printf '{"name":"%s","version":"%s","url":"%s","sha256":"%s","installed":"%s"}\n' \
        "$e" "$TAG" "$BASE/$ASSET" "$SUM" "$NOW" > "$PLUGIN_DIR/kling-$e.json" \
        || fail "cannot write $PLUGIN_DIR/kling-$e.json"
    ok "installed $PLUGIN_DIR/kling-$e"
    if [ "$NO_COMPANIONS" != "1" ]; then
        for c in $(companions_of "$e"); do
            place "$WORK/${c}-${PLAT}" "$PREFIX/$c" || fail "cannot write to $PREFIX"
            ok "installed $PREFIX/$c"
        done
    fi
done

# ── shell: completado y PATH, para que `kling doctor` salga en verde ─────────
# La shell de quien instala, por $SHELL (el instalador suele correr bajo sh).
USER_SHELL="$(basename "${SHELL:-}")"
case "$USER_SHELL" in
    bash) RC="$HOME/.bashrc"; RELOAD="source <(kling completion bash)" ;;
    zsh)  RC="$HOME/.zshrc";  RELOAD="source <(kling completion zsh)" ;;
    fish) RC="$HOME/.config/fish/config.fish"; RELOAD="kling completion fish | source" ;;
    *)    RC=""; RELOAD="source <(kling completion bash|zsh)   # or: kling completion fish | source" ;;
esac

# add_rc LINEA: la añade al rc una sola vez, bajo "# kling".
add_rc() {
    [ -n "$RC" ] || return 1
    if [ -f "$RC" ] && grep -Fq -- "$1" "$RC"; then
        return 0
    fi
    mkdir -p "$(dirname "$RC")" 2>/dev/null
    if [ -f "$RC" ] && grep -q "^# kling$" "$RC"; then
        printf "%s\n" "$1" >> "$RC" || return 1
    else
        printf "\n# kling\n%s\n" "$1" >> "$RC" || return 1
    fi
    ok "added to $RC: $1"
}

KLING_BIN="$PREFIX/kling"
[ -x "$KLING_BIN" ] || KLING_BIN="$(command -v kling 2>/dev/null || true)"

PATH_LINE=""
case ":$PATH:" in
    *":$PREFIX:"*) ;;
    *)
        if [ "$USER_SHELL" = "fish" ]; then
            PATH_LINE="fish_add_path $PREFIX"
        else
            PATH_LINE="export PATH=\"$PREFIX:\$PATH\""
        fi
        ;;
esac

COMPLETION_LINE=""
if [ "$NO_RC" != "1" ] && [ -n "$KLING_BIN" ] && [ -n "$RC" ]; then
    # `kling completion install` escribe ~/.config/kling/completion.<shell> y
    # dice qué línea lo carga; nos la quedamos de su salida.
    COMPLETION_LINE="$("$KLING_BIN" completion install "$USER_SHELL" 2>/dev/null | sed -n '/^Add this line/{n;s/^  //;p;}')"
    [ -n "$PATH_LINE" ] && add_rc "$PATH_LINE"
    [ -n "$COMPLETION_LINE" ] && add_rc "$COMPLETION_LINE"
fi

echo
if [ -n "$PATH_LINE" ] && { [ "$NO_RC" = "1" ] || [ -z "$RC" ]; }; then
cat <<EOF
  To use kling in an open shell, add this to your rc file:

      $PATH_LINE

EOF
fi
if [ "$NO_RC" = "1" ] || [ -z "$RC" ]; then
cat <<EOF
  Shell completion: write the script and add the line it prints to your rc file:

      kling completion install

EOF
fi
cat <<EOF
  Try it (in a new shell, or after 'source ${RC:-your rc}'):

      kling try -- uname -a

  The first run starts the daemon and downloads the guest kernel and image
  (about 10 MB); after that it runs in well under a second.

  To use a remote daemon instead:

      kling context add lab ssh://user@<host-with-kvm>

EOF

if [ -n "$EXTS" ]; then
cat <<EOF
  Extensions installed in $PLUGIN_DIR:

      kling plugin ls

  In an open shell, reload the completion to see their commands:

      $RELOAD

EOF
fi

ok "done"
}

# Entre llaves: la shell lee el grupo entero antes de ejecutarlo, así que un
# corte a mitad de esta línea (tras `main`, sin los argumentos) no corre nada.
{ main "$@"; }
