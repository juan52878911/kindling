#!/usr/bin/env bash
# El paso 3c de docs/releases.md de un tirón, en un Mac con Apple Silicon y
# desde un checkout limpio: compila kling, kling-guest (linux/arm64), kling-db,
# kling-hv y kling-vz de este árbol, sellados con git describe; prepara vmlinux
# y min.ext4; corre 92-e2e-mac.sh y 95-e2e-upgrade-mac.sh con VMM=hv y, si los
# dos pasan sobre un árbol sin cambios, publica en GitHub un commit status
# (e2e-mac/kling-hv, success) sobre ese commit. Es la prueba que pide
# scripts/release.sh mientras e2e-mac.yml no tenga runner (la línea a mano
# KLING_E2E_HV ya no vale); KLING_E2E_STATUS=0 no lo publica.
#
#   scripts/e2e-mac-release.sh
#   E2E=95 scripts/e2e-mac-release.sh        solo uno (92 o 95)
#   MEM=256 BURST=4 scripts/e2e-mac-release.sh   más suave con la memoria (pasan a 92)
#
# Nada toca el kindling del usuario: los daemons de las pruebas tienen su raíz,
# su socket y su configuración bajo un directorio temporal, que se borra al
# acabar (KEEP_WORK=1 lo deja). Del kindling instalado solo se LEEN vmlinux y
# min.ext4 (KLING_E2E_DATA, por defecto ~/Library/Application Support/kindling/images),
# clonados con cp -c; si no están, se bajan de la última release y se comprueban
# contra su SHA256SUMS. Lo único que queda es la caché de la imagen de 92
# (KLING_E2E_CACHE, por defecto ~/.cache/kling-e2e) y, si una prueba falla, sus
# logs en KLING_E2E_CACHE/logs/<sha12>-<prueba>/ (el script dice dónde).
#
# 95 instala su propio agente de launchd con la etiqueta dev.kindling.daemon
# (la que reinicia `kling upgrade`) y se niega si el usuario ya tiene uno. Este
# script no lo quita nunca: si está, corre 92, explica qué hacer y termina sin
# publicar el status. Cada prueba que pasa deja una marca por commit en
# KLING_E2E_CACHE/passed, así que 95 puede correr después con E2E=95 y el
# status se publica entonces.
#
# Variables: E2E ("92 95"), KEEP_WORK, KLING_E2E_DATA, KLING_E2E_CACHE,
# KLING_E2E_STATUS (1), KLING_E2E_REPO (juan52878911/kindling) y lo que
# entienden 92 (MEM, BURST, KLING_MIN_FREE_DISK_MIB...) y 95 (OLD_TAG, OLD_DIR,
# GITHUB_TOKEN). bash 3.2.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TOP="$(cd "$HERE/.." && pwd)"
E2E="${E2E:-92 95}"
KEEP_WORK="${KEEP_WORK:-0}"
KLING_E2E_DATA="${KLING_E2E_DATA:-$HOME/Library/Application Support/kindling/images}"
KLING_E2E_CACHE="${KLING_E2E_CACHE:-$HOME/.cache/kling-e2e}"
export KLING_E2E_DATA KLING_E2E_CACHE
# Las dos pruebas caben en unos 2 GiB; el Mac pide 16 libres por defecto.
export KLING_MIN_FREE_DISK_MIB="${KLING_MIN_FREE_DISK_MIB:-4096}"
LABEL="dev.kindling.daemon"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
TMPBASE="${TMPDIR:-/tmp}"; TMPBASE="${TMPBASE%/}"

die() { echo "e2e-mac-release: $*" >&2; exit 1; }
say() { printf "\n\033[1m== %s\033[0m\n" "$*"; }
# shellcheck source=scripts/lib-e2e-upgrade.sh
. "$HERE/lib-e2e-upgrade.sh"

[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || die "this is for an Apple Silicon Mac"
[ "$(sysctl -n kern.hv_support 2>/dev/null)" = 1 ] || die "kern.hv_support is not 1: no hypervisor for kling-hv"
command -v go >/dev/null || die "go is missing (brew install go)"
for p in $E2E; do case "$p" in 92|95) ;; *) die "E2E: 92 and/or 95, not \"$p\"";; esac; done

# El agente del usuario: 95 no puede correr con él cargado (ni con su plist).
AGENTE=0
if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || [ -e "$PLIST" ]; then AGENTE=1; fi
explica_agente() {
  cat >&2 <<EOF

95-e2e-upgrade-mac.sh did not run: you have your own $LABEL (loaded, or
$PLIST exists), and the test needs that label for a private daemon of its own.
This script never touches yours. To run 95, stop it for a few minutes (its
machines stop with it) and move its plist aside:

  launchctl bootout gui/\$(id -u)/$LABEL
  mv "$PLIST" "$PLIST.e2e-aside"
  E2E=95 scripts/e2e-mac-release.sh

and then put it back:

  mv "$PLIST.e2e-aside" "$PLIST"
  launchctl bootstrap gui/\$(id -u) "$PLIST"

EOF
}

SHA=$(git -C "$TOP" rev-parse --short=12 HEAD) || die "not a git checkout"
SUCIO=""; [ -n "$(git -C "$TOP" status --porcelain --untracked-files=no)" ] && SUCIO=1
V=$(git -C "$TOP" describe --tags --always)
WORK=$(mktemp -d "$TMPBASE/kling-e2e-release.XXXXXX") || die "mktemp"
fin() {
  if [ "$KEEP_WORK" = 1 ]; then echo "KEEP_WORK=1: $WORK left as it is"; else rm -rf "$WORK"; fi
}
trap fin EXIT
BIN="$WORK/bin"; IMGS="$WORK/images"; mkdir -p "$BIN" "$IMGS" "$WORK/plugins"
echo "tree $SHA ($V)${SUCIO:+, with uncommitted changes}; work dir $WORK"
df -h "$TMPBASE" | awk 'NR==2 {print "disk free under TMPDIR: " $4}'

# ── binarios ─────────────────────────────────────────────────────────────────
# Como e2e-mac.yml: la versión sin la v, como release.yml.
say "build ($V)"
(
  set -e
  cd "$TOP"
  export GOWORK=off
  go build -trimpath -ldflags "-X main.Version=${V#v}" -o "$BIN/kling" ./cmd/kling
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-X main.Version=${V#v}" -o "$BIN/kling-guest" ./cmd/kling-guest
  (cd ext/db && go build -trimpath -ldflags "-X main.Version=${V#v}" -o "$WORK/plugins/kling-db" ./cmd/kling-db)
  (cd vz && OUT="$BIN/kling-vz" VERSION="$V" ./build.sh && OUT="$BIN/kling-hv" VERSION="$V" ./build-hv.sh) >/dev/null
  "$BIN/kling-hv" --probe >/dev/null
) || die "build failed"
echo "kling, kling-guest, kling-db, kling-vz and kling-hv in $BIN"

# ── vmlinux y min.ext4 ───────────────────────────────────────────────────────
say "kernel and min image"
if [ -f "$KLING_E2E_DATA/vmlinux" ] && [ -f "$KLING_E2E_DATA/min.ext4" ] \
   && cp -c "$KLING_E2E_DATA/vmlinux" "$IMGS/vmlinux" && cp -c "$KLING_E2E_DATA/min.ext4" "$IMGS/min.ext4"; then
  echo "cloned (cp -c, read only) from $KLING_E2E_DATA"
else
  rm -f "$IMGS/vmlinux" "$IMGS/min.ext4"
  command -v zstd >/dev/null || die "no vmlinux/min.ext4 in $KLING_E2E_DATA and no zstd to unpack the release ones (brew install zstd)"
  TAG=$(release_anterior "$V")
  bajar_release "$TAG" "$WORK/dl" vmlinux-arm64.zst:vmlinux.zst min-arm64.ext4.zst:min.ext4.zst \
    || die "could not download vmlinux and min of $TAG"
  zstd -q -d -o "$IMGS/vmlinux" "$WORK/dl/vmlinux.zst" && zstd -q -d -o "$IMGS/min.ext4" "$WORK/dl/min.ext4.zst" \
    || die "could not unpack vmlinux and min of $TAG"
  rm -rf "$WORK/dl"
  echo "downloaded from release $TAG, checked against its SHA256SUMS"
fi
echo "vmlinux sha256 $(sha256_de "$IMGS/vmlinux" | cut -c1-12)"

# ── las pruebas ──────────────────────────────────────────────────────────────
# Los logs de una prueba que falla sobreviven al borrado de $WORK: el log de su
# daemon y la consola de cada máquina (los copia la propia prueba, antes de su
# limpieza) y los informes de fallo de kling-hv/kling-vz que macOS dejó
# mientras corría, en KLING_E2E_CACHE/logs/<sha12>-<prueba>/. Las imágenes no.
LOGS="$KLING_E2E_CACHE/logs"
MARCA="$WORK/empezado"; : > "$MARCA"
logs_de() { # logs_de <prueba> <código>
  local d="$LOGS/$SHA-$1" f
  if [ "$2" = 0 ]; then rm -rf "$d"; return; fi
  mkdir -p "$d/crashes" || return
  for f in "$HOME"/Library/Logs/DiagnosticReports/kling-hv* "$HOME"/Library/Logs/DiagnosticReports/kling-vz*; do
    [ -f "$f" ] && [ "$f" -nt "$MARCA" ] && cp "$f" "$d/crashes/"
  done
  rmdir "$d/crashes" 2>/dev/null
  echo "logs of $1 kept in $d"
}
R92=-; R95=-
for p in $E2E; do
  case "$p" in
    92)
      say "92-e2e-mac.sh (VMM=hv)"
      rm -rf "$LOGS/$SHA-92"
      VMM=hv KLING="$BIN/kling" KLING_DB="$WORK/plugins/kling-db" IMAGES_DIR="$IMGS" \
        ROOT="$WORK/92/Library/Application Support/kindling-e2e-a-deliberately-long-root" \
        LOG="$WORK/92/daemon.log" KLING_E2E_LOGS="$LOGS/$SHA-92" bash "$HERE/92-e2e-mac.sh"
      R92=$?
      logs_de 92 "$R92"
      ;;
    95)
      if [ "$AGENTE" = 1 ]; then R95=agent; continue; fi
      say "95-e2e-upgrade-mac.sh (VMM=hv)"
      rm -rf "$LOGS/$SHA-95"
      VMM=hv NEW_DIR="$BIN" IMAGES_DIR="$IMGS" BASE="$WORK/95" KLING_E2E_CACHE="$KLING_E2E_CACHE" \
        KLING_E2E_LOGS="$LOGS/$SHA-95" bash "$HERE/95-e2e-upgrade-mac.sh"
      R95=$?
      logs_de 95 "$R95"
      ;;
  esac
done

say "result"
res() { case "$2" in -) echo "$1: not run";; 0) echo "$1: passed";; agent) echo "$1: not run (your $LABEL is in the way)";; *) echo "$1: FAILED (exit $2)";; esac; }
res 92-e2e-mac.sh "$R92"; res 95-e2e-upgrade-mac.sh "$R95"
[ "$R95" = agent ] && explica_agente
# Cada prueba que pasa sobre un árbol sin cambios deja su marca por commit: 92
# y 95 pueden correr en dos veces (95 pide parar el agente del usuario) y la
# línea sale cuando el commit tiene las dos.
HECHO="$KLING_E2E_CACHE/passed"
if [ -z "$SUCIO" ]; then
  mkdir -p "$HECHO"
  [ "$R92" = 0 ] && : > "$HECHO/$SHA.92"
  [ "$R95" = 0 ] && : > "$HECHO/$SHA.95"
fi
# El rastro en GitHub: un commit status sobre el commit entero, con el mismo
# contexto que busca scripts/release.sh (si cambia, en los dos). Necesita gh
# autenticado con escritura en el repo y el commit ya empujado; si no se
# puede, lo dice y sale con error: sin el status, release.sh no etiqueta.
CONTEXTO="e2e-mac/kling-hv"
publicar_estado() {
  if [ "${KLING_E2E_STATUS:-1}" = 0 ]; then echo "KLING_E2E_STATUS=0: no commit status on GitHub"; return 0; fi
  command -v gh >/dev/null || { echo "gh is missing: no commit status on GitHub" >&2; return 1; }
  local full repo mac
  full=$(git -C "$TOP" rev-parse HEAD) || return 1
  repo="${KLING_E2E_REPO:-juan52878911/kindling}"
  mac="macOS $(sw_vers -productVersion 2>/dev/null), $(sysctl -n machdep.cpu.brand_string 2>/dev/null)"
  if gh api -X POST "repos/$repo/statuses/$full" -f state=success -f context="$CONTEXTO" \
       -f description="92 and 95 passed with VMM=hv ($mac)" >/dev/null; then
    echo "commit status $CONTEXTO=success on $repo@$SHA"
  else
    echo "could not publish the commit status on $repo@$SHA (pushed? gh auth with write access?)" >&2
    return 1
  fi
}
if [ -f "$HECHO/$SHA.92" ] && [ -f "$HECHO/$SHA.95" ] && [ -z "$SUCIO" ]; then
  echo
  echo "92 and 95 passed with VMM=hv on $SHA"
  publicar_estado || { echo "without the commit status release.sh will not tag $SHA: push it, check gh auth and run E2E=95 scripts/e2e-mac-release.sh again" >&2; exit 1; }
  exit 0
fi
[ -n "$SUCIO" ] && echo "the tree has uncommitted changes: nothing counts for the release; commit and run it again" >&2
for p in 92 95; do [ -f "$HECHO/$SHA.$p" ] || echo "the release needs $p passed on $SHA (E2E=$p scripts/e2e-mac-release.sh)" >&2; done
exit 1
