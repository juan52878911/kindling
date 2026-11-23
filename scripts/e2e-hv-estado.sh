#!/bin/sh
# e2e-hv-estado.sh REPO COMMIT: sale con 0 si el commit status e2e-mac/kling-hv
# de COMMIT está en success, y con 1 si no.
#
# Es la puerta del e2e de kling-hv (92-e2e-mac.sh y 95-e2e-upgrade-mac.sh con
# VMM=hv en un Apple Silicon) desde 1.0. La usan scripts/release.sh antes de
# crear la etiqueta y el job release de release.yml antes de firmar: una
# etiqueta empujada a mano tampoco publica sin ella.
#
# Solo cuenta el commit status, no el check run del job de e2e-mac.yml: ese
# job sale verde aunque se lance sin una de las dos pruebas (parts=upgrade).
# El status lo publican el job (solo si corrieron y pasaron las dos) y
# scripts/e2e-mac-release.sh. El combinado de GitHub da el último status de
# cada contexto: un failure posterior a un success cierra la puerta.
#
# Necesita gh autenticado (GH_TOKEN en Actions, con statuses: read).
set -u

# El contexto es el de e2e-mac-release.sh y el de e2e-mac.yml: si cambia, en
# los tres.
CONTEXTO="e2e-mac/kling-hv"

[ $# -eq 2 ] || { echo "usage: $0 OWNER/REPO COMMIT" >&2; exit 2; }
REPO=$1; COMMIT=$2

if ! command -v gh >/dev/null 2>&1; then
    echo "kling-hv e2e not run on $COMMIT: gh is missing, so the $CONTEXTO commit status cannot be read" >&2
    exit 1
fi
# Una línea por status de ese contexto en success, con su descripción (qué
# kernel, qué imagen, de qué release partió 95).
OK=$(gh api "repos/$REPO/commits/$COMMIT/status" \
    --jq ".statuses[] | select(.context == \"$CONTEXTO\" and .state == \"success\") | \"ok \" + (.description // \"\")" 2>/dev/null) || OK=""
case "$OK" in
    ok*)
        echo "kling-hv e2e passed on an Apple Silicon Mac (commit status $CONTEXTO on $COMMIT): ${OK#ok }"
        exit 0
        ;;
esac
echo "kling-hv e2e not run on $COMMIT: run scripts/e2e-mac-release.sh on an Apple Silicon Mac with gh authenticated; it publishes the $CONTEXTO commit status (docs/releases.md)" >&2
exit 1
