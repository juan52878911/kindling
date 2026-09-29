#!/bin/bash
# Compila el servidor uidump a un .dex para app_process (Android 13, arm64 o
# cualquier otra: el dex es independiente de la arquitectura).
#
#   prototypes/android/uidump/build.sh [SALIDA.dex]
#
# Necesita un JDK 17 (javac) y curl. android.jar (plataforma 33) y r8 (d8) se
# bajan una vez a $UIDUMP_SDK (por defecto ~/.cache/kindling-uidump) y se
# verifican por sha256 antes de usarlos.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${1:-$HERE/kindling-uidump.dex}"
SDK="${UIDUMP_SDK:-$HOME/.cache/kindling-uidump}"

# ── lo fijado ────────────────────────────────────────────────────────────────
PLATFORM_ZIP=platform-33-ext5_r01.zip
PLATFORM_URL="https://dl.google.com/android/repository/$PLATFORM_ZIP"
PLATFORM_SHA256=3d6189a18d0cebd4e518b8728de0e6b940d95c96b57316769dd56ee2d1708d10
R8_VERSION=8.13.24
R8_URL="https://dl.google.com/android/maven2/com/android/tools/r8/$R8_VERSION/r8-$R8_VERSION.jar"
R8_SHA256=9323b4b8f27bd855f299cf6b870eef4ec879b6b8999718a20aba22937efd5a70

die() { echo "uidump/build.sh: $*" >&2; exit 1; }
command -v javac >/dev/null || die "missing javac (apt-get install openjdk-17-jdk-headless)"
command -v curl >/dev/null || die "missing curl"
command -v unzip >/dev/null || die "missing unzip"

# baja URL FICHERO SHA256: descarga si falta y comprueba siempre.
baja() {
  if [ ! -f "$2" ]; then
    curl -fsSL --retry 3 -o "$2.part" "$1" || die "download failed: $1"
    mv "$2.part" "$2"
  fi
  echo "$3  $2" | sha256sum -c --quiet - || { rm -f "$2"; die "sha256 mismatch: $2"; }
}

mkdir -p "$SDK"
baja "$PLATFORM_URL" "$SDK/$PLATFORM_ZIP" "$PLATFORM_SHA256"
baja "$R8_URL" "$SDK/r8-$R8_VERSION.jar" "$R8_SHA256"
if [ ! -f "$SDK/android-33.jar" ]; then
  unzip -p "$SDK/$PLATFORM_ZIP" android-33-ext5/android.jar >"$SDK/android-33.jar.part" \
    || unzip -p "$SDK/$PLATFORM_ZIP" '*/android.jar' >"$SDK/android-33.jar.part"
  mv "$SDK/android-33.jar.part" "$SDK/android-33.jar"
fi

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
mapfile -t SRC < <(find "$HERE/src" -name '*.java')
javac --release 11 -Xlint:-options -XDsuppressNotes -encoding UTF-8 -cp "$SDK/android-33.jar" -d "$W/classes" "${SRC[@]}" \
  || die "javac failed"
mapfile -t CLS < <(find "$W/classes" -name '*.class')
mkdir -p "$W/dex"
java -cp "$SDK/r8-$R8_VERSION.jar" com.android.tools.r8.D8 --release --min-api 33 \
  --lib "$SDK/android-33.jar" --output "$W/dex" "${CLS[@]}"
install -m644 "$W/dex/classes.dex" "$OUT"
echo "uidump: $OUT ($(wc -c <"$OUT") bytes, sha256 $(sha256sum "$OUT" | cut -d' ' -f1))"
