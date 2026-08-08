#!/bin/sh
# release.sh — crea un tag anotado y lo pushea. El workflow de GitHub Actions
# detecta el tag, compila los binarios y publica la release.
#
# USO
#   ./scripts/release.sh                        # tag v0.1.0 (lee VERSION del env o del último tag)
#   VERSION=v0.2.0 ./scripts/release.sh        # tag explícito
#   ./scripts/release.sh --dry-run             # muestra qué haría
#
# Pre-requisitos:
#   - Working tree limpio (sin cambios sin commitear)
#   - main al día con origin/main
#   - Permisos para pushear tags al repo
#
# Lo que hace:
#   1. Verifica que el working tree esté limpio
#   2. Crea un tag anotado vX.Y.Z con mensaje corto
#   3. Push del tag → activa .github/workflows/release.yml
#   4. (Opcional) espera a que el workflow termine y abre la release en el browser

set -u

VERSION="${VERSION:-}"
DRY_RUN=0
WAIT=0

usage() {
    cat <<EOF
Uso: scripts/release.sh [--dry-run] [--wait] [VERSION]

  --dry-run   muestra qué haría sin tocar nada
  --wait      espera a que el workflow de release termine
  VERSION     tag a crear (ej. v0.2.0). Si se omite, sugiere el siguiente
              SemVer a partir del último tag usando VERSION del entorno.

Variables de entorno: VERSION.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        --wait)    WAIT=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *)         VERSION="$1"; shift ;;
    esac
done

# ── preflight ──────────────────────────────────────────────────────────────
if ! command -v git >/dev/null 2>&1; then
    echo "git no encontrado" >&2; exit 1
fi
# En modo dry-run saltamos las verificaciones de repo/working tree.
if [ "$DRY_RUN" = "0" ]; then
    if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        echo "no estamos en un repo git" >&2; exit 1
    fi
fi

if [ -z "$VERSION" ]; then
    LAST=$(git tag --list 'v*' --sort=-v:refname | head -1)
    if [ -z "$LAST" ]; then
        echo "no hay tags previos y no se pasó VERSION. Empezamos en v0.1.0" >&2
        VERSION="v0.1.0"
    else
        echo "último tag: $LAST"
        echo "pasa VERSION=vX.Y.Z explícito (no se incrementa automáticamente a propósito)" >&2
        exit 1
    fi
fi

# Normaliza: debe empezar con 'v'
case "$VERSION" in
    v*) ;;
    *)   VERSION="v$VERSION" ;;
esac

if git tag --list "$VERSION" | grep -q "^${VERSION}\$"; then
    echo "el tag $VERSION ya existe localmente" >&2
    exit 1
fi

# Working tree limpio (--dry-run lo permite)
if [ "$DRY_RUN" = "0" ]; then
    if ! git diff --quiet --ignore-submodules HEAD 2>/dev/null; then
        echo "hay cambios sin commitear. Haz commit o usa --dry-run." >&2
        git status --short >&2
        exit 1
    fi
fi

# main al día
LOCAL=$(git rev-parse --verify main 2>/dev/null || git rev-parse --verify HEAD)
REMOTE=$(git rev-parse --verify origin/main 2>/dev/null || echo "")
if [ -n "$REMOTE" ] && [ "$LOCAL" != "$REMOTE" ]; then
    echo "main local ($LOCAL) != origin/main ($REMOTE). Haz fetch/merge primero." >&2
    exit 1
fi

# ── crear tag ──────────────────────────────────────────────────────────────
MSG="release $VERSION — ver CHANGELOG.md"

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) haría:"
    echo "  git tag -a $VERSION -m \"$MSG\""
    echo "  git push origin $VERSION"
    echo "  GitHub Actions compila y publica la release con los binarios."
    exit 0
fi

echo "creando tag anotado $VERSION..."
git tag -a "$VERSION" -m "$MSG"

echo "pusheando $VERSION a origin (esto activa el workflow)..."
git push origin "$VERSION"

cat <<EOF

  ✓ tag $VERSION pusheado.

  El workflow .github/workflows/release.yml ya está corriendo.
  Mira el progreso en:

      https://github.com/$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')/actions

  Cuando termine, la release queda en:

      https://github.com/$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')/releases/tag/$VERSION
EOF

# ── esperar al workflow (opcional) ─────────────────────────────────────────
if [ "$WAIT" = "1" ]; then
    if ! command -v gh >/dev/null 2>&1; then
        echo "gh CLI no encontrado, no puedo esperar al workflow" >&2
        exit 0
    fi
    echo "esperando a que termine el workflow..."
    REPO=$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')
    gh run watch --exit-status --repo "$REPO" || {
        echo "el workflow terminó con error. Revisa:" >&2
        echo "  https://github.com/$REPO/actions" >&2
        exit 1
    }
    echo "✓ release publicada"
    gh release view "$VERSION" --repo "$REPO" --web
fi