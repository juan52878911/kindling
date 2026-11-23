#!/usr/bin/env bash
# E2E del primer uso, de `curl … | sh` al primer sandbox, en un HOME vacío y con
# un PATH sin minisign ni brew: lo que vive un usuario nuevo (E2 de la ronda 6).
#
#   scripts/96-e2e-primer-uso.sh                 la última release estable (red)
#   TAG=v1.0.0 scripts/96-e2e-primer-uso.sh      una concreta
#   INSTALL_SH=scripts/install.sh ...            el install.sh a probar (por defecto el de este árbol)
#   KLING_E2E_BIN=/ruta/a/kling ...              el kling del árbol: tras install.sh sustituye al bajado
#                                                para el up y el try (sin él se prueba el de la release
#                                                publicada, no el cambio que se está juzgando)
#
# Mide, con cronómetro, cada tramo: install.sh y `kling try -- uname -a` (que
# arranca el daemon y baja el kernel y min). Cifra esperada en un Mac limpio con
# fibra: por debajo de 30 s en total. Al acabar imprime una línea JSON con los
# segundos y borra todo lo que creó: el HOME temporal, y el agente de launchd
# (KLING_LAUNCH_AGENT_PLIST propio, con etiqueta propia: no toca el del usuario).
#
# En Linux solo corre hasta install.sh (el daemon necesita root y KVM): con
# SKIP_TRY=0 y root intenta también el try. bash 3.2.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${INSTALL_SH:-$HERE/install.sh}"
TAG="${TAG:-}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/kling-primer-uso.XXXXXX")"
HOME_T="$WORK/home"
mkdir -p "$HOME_T"

# PATH mínimo: lo del sistema, sin /opt/homebrew ni /usr/local (minisign, brew).
BASE_PATH="/usr/bin:/bin:/usr/sbin:/sbin"
if PATH="$BASE_PATH" command -v minisign >/dev/null 2>&1; then
    echo "aviso: minisign está en $BASE_PATH; no se prueba la ausencia" >&2
fi

LABEL="dev.kindling.e2e$$"
PLIST="$HOME_T/Library/LaunchAgents/$LABEL.plist"
export KLING_LAUNCH_AGENT_LABEL="$LABEL" KLING_LAUNCH_AGENT_PLIST="$PLIST"
export KLING_ROOT="$HOME_T/kindling-root"
export KLING_SOCKET="$KLING_ROOT/kling.sock"

limpiar() {
    if [ "$(uname -s)" = Darwin ] && [ -f "$PLIST" ]; then
        launchctl bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
}
trap limpiar EXIT INT TERM

ahora() { date +%s; }

t0="$(ahora)"
if ! env -i HOME="$HOME_T" PATH="$BASE_PATH" SHELL=/bin/sh \
        /bin/sh "$INSTALL_SH" ${TAG:+--tag "$TAG"} --no-rc --prefix "$HOME_T/bin" 2>&1 | sed 's/^/  install| /'; then
    echo "FALLO: install.sh" >&2
    exit 1
fi
[ -x "$HOME_T/bin/kling" ] || { echo "FALLO: install.sh no dejó kling" >&2; exit 1; }
if [ -n "${KLING_E2E_BIN:-}" ]; then
    [ -x "$KLING_E2E_BIN" ] || { echo "FALLO: KLING_E2E_BIN=$KLING_E2E_BIN no es ejecutable" >&2; exit 1; }
    cp "$KLING_E2E_BIN" "$HOME_T/bin/kling" || { echo "FALLO: no se pudo copiar KLING_E2E_BIN" >&2; exit 1; }
    echo "kling bajo prueba: $KLING_E2E_BIN (el de install.sh se sustituye)"
fi
t1="$(ahora)"
echo "install.sh: $((t1 - t0)) s"

# En Linux con KVM, `up -install -dry-run` dice los pasos (firecracker, jailer,
# usuarios, unidad) y no toca nada: se comprueba sin root.
if [ "$(uname -s)" = Linux ] && [ -e /dev/kvm ]; then
    if ! DRY="$(KLING_HOST=unix:///nonexistent/kling.sock "$HOME_T/bin/kling" up -install -dry-run 2>&1)"; then
        echo "FALLO: kling up -install -dry-run: $DRY" >&2
        exit 1
    fi
    printf '%s\n' "$DRY" | sed 's/^/  dry-run| /'
fi

if [ "$(uname -s)" != Darwin ] && { [ "${SKIP_TRY:-1}" = 1 ] || [ "$(id -u)" != 0 ]; }; then
    printf '{"install_s":%s,"try_s":null}\n' "$((t1 - t0))"
    exit 0
fi

# El agente se instala sin preguntar: no hay terminal (kling up -install lo
# haría igual). Después try arranca lo que falte y contesta.
env -i HOME="$HOME_T" PATH="$HOME_T/bin:$BASE_PATH" KLING_ROOT="$KLING_ROOT" KLING_SOCKET="$KLING_SOCKET" \
    KLING_LAUNCH_AGENT_LABEL="$LABEL" KLING_LAUNCH_AGENT_PLIST="$PLIST" \
    "$HOME_T/bin/kling" up -install 2>&1 | sed 's/^/  up| /'
# El código de salida es el de kling, no el de sed (PIPESTATUS[0]).
RC_UP="${PIPESTATUS[0]}"
t2="$(ahora)"
if [ "$RC_UP" != 0 ]; then
    echo "FALLO: kling up -install salió con $RC_UP" >&2
    exit 1
fi
SALIDA="$(env -i HOME="$HOME_T" PATH="$HOME_T/bin:$BASE_PATH" KLING_ROOT="$KLING_ROOT" KLING_SOCKET="$KLING_SOCKET" \
    "$HOME_T/bin/kling" try -- uname -a 2>"$WORK/try.err")"
RC=$?
t3="$(ahora)"
echo "kling try -- uname -a (rc=$RC): $SALIDA"
sed 's/^/  try| /' "$WORK/try.err"
echo "kling up -install: $((t2 - t1)) s · kling try: $((t3 - t2)) s · total: $((t3 - t0)) s"
printf '{"install_s":%s,"up_s":%s,"try_s":%s,"total_s":%s}\n' "$((t1 - t0))" "$((t2 - t1))" "$((t3 - t2))" "$((t3 - t0))"
[ "$RC" = 0 ] && case "$SALIDA" in Linux*) exit 0 ;; esac
echo "FALLO: kling try no contestó como un invitado Linux" >&2
exit 1
