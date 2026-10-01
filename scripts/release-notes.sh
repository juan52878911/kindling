#!/bin/sh
# release-notes.sh — imprime las notas de una release: el bloque de esa versión
# en CHANGELOG.md (sin su título, que ya es el de la release) y una línea de
# instalación. Lo usan .github/workflows/release.yml y scripts/release.sh.
#
# USO
#   scripts/release-notes.sh v0.18.0 [CHANGELOG.md]
#
# Sale con 1 si CHANGELOG.md no tiene la sección "## [0.18.0] - AAAA-MM-DD"
# (o "## 0.18.0 - ..."): una release sin su bloque no se publica.
# POSIX sh + awk: corre igual en Linux y en el bash 3.2 de macOS.

set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
    echo "usage: scripts/release-notes.sh VERSION [CHANGELOG]" >&2
    exit 2
fi
VER="${1#v}"
FILE="${2:-$(dirname "$0")/../CHANGELOG.md}"

if [ ! -f "$FILE" ]; then
    echo "release-notes: $FILE not found" >&2
    exit 1
fi

# Comparación exacta del número de versión (0.1.0 no casa con 0.10.0), sin
# tratarlo como regex. Se quitan las líneas en blanco del principio y del final.
BODY=$(awk -v ver="$VER" '
    /^## / {
        if (seen) exit
        h = $2
        gsub(/^\[|\]$/, "", h)
        sub(/^v/, "", h)
        if (h == ver) { seen = 1; next }
    }
    /^\[[^]]+\]: / { next }
    seen { lines[++n] = $0 }
    END {
        first = 1; while (first <= n && lines[first] ~ /^[ \t]*$/) first++
        last = n;  while (last >= first && lines[last] ~ /^[ \t]*$/) last--
        for (i = first; i <= last; i++) print lines[i]
    }
' "$FILE")

if [ -z "$BODY" ]; then
    echo "release-notes: no \"## [$VER]\" section in $FILE" >&2
    exit 1
fi

printf '%s\n\n' "$BODY"
printf '%s\n' "**Install or upgrade:** \`curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | KLING_VERSION=v$VER sh\`"
