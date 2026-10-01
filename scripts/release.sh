#!/bin/sh
# release.sh — crea un tag anotado y lo pushea. El workflow de GitHub Actions
# detecta el tag, compila los binarios y publica la release.
#
# Una etiqueta publica todo el repo: el núcleo, kling-vz, las extensiones de
# ext/ (kling-mcp, kling-bridge, kling-sandbox), el operador
# (binarios e imagen GHCR) y los tar del host, con un único SHA256SUMS.
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
#   - CHANGELOG.md con la sección "## [X.Y.Z] - AAAA-MM-DD" (inglés, una
#     línea por cambio): es lo único que llevan las notas de la release
#
# Lo que hace:
#   1. Verifica que el working tree esté limpio
#   2. Verifica que ext/*/go.mod requieran kindling en esa misma versión
#   3. Verifica que CHANGELOG.md tenga el bloque de la versión
#      (scripts/release-notes.sh, el mismo que usa el workflow)
#   4. Crea un tag anotado vX.Y.Z con mensaje corto
#   5. Push del tag → activa .github/workflows/release.yml
#   6. (Opcional) espera a que el workflow termine y abre la release en el browser

set -u

VERSION="${VERSION:-}"
DRY_RUN=0
WAIT=0

usage() {
    cat <<EOF
Usage: scripts/release.sh [--dry-run] [--wait] [VERSION]

  --dry-run   show what would happen, and the release notes, without changing anything
  --wait      wait for the release workflow to finish
  VERSION     tag to create (e.g. v0.2.0); CHANGELOG.md needs a "## [0.2.0]" section

Environment: VERSION.
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
    echo "git not found" >&2; exit 1
fi
# En modo dry-run saltamos las verificaciones de repo/working tree.
if [ "$DRY_RUN" = "0" ]; then
    if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        echo "not inside a git repository" >&2; exit 1
    fi
fi

if [ -z "$VERSION" ]; then
    LAST=$(git tag --list 'v*' --sort=-v:refname | head -1)
    if [ -z "$LAST" ]; then
        echo "no previous tags and no VERSION given: starting at v0.1.0" >&2
        VERSION="v0.1.0"
    else
        echo "last tag: $LAST"
        echo "pass VERSION=vX.Y.Z explicitly (no automatic bump, on purpose)" >&2
        exit 1
    fi
fi

# Normaliza: debe empezar con 'v'
case "$VERSION" in
    v*) ;;
    *)   VERSION="v$VERSION" ;;
esac

if git tag --list "$VERSION" | grep -q "^${VERSION}\$"; then
    echo "tag $VERSION already exists locally" >&2
    exit 1
fi

# Working tree limpio (--dry-run lo permite)
if [ "$DRY_RUN" = "0" ]; then
    if ! git diff --quiet --ignore-submodules HEAD 2>/dev/null; then
        echo "uncommitted changes: commit them or use --dry-run" >&2
        git status --short >&2
        exit 1
    fi
fi

# ext/*/go.mod citan en su require la etiqueta que se va a publicar. Con el
# replace ../.. nunca se descarga, pero quien use una extensión como módulo sin
# el replace (go get .../ext/mcp@vX) tiraría de ese require: si se queda en la
# versión anterior, la extensión de vX compilaría contra el núcleo de vX-1.
for m in ext/*/go.mod; do
    [ -f "$m" ] || continue
    REQ=$(awk '$1=="require" && $2=="github.com/juan52878911/kindling" {print $3}' "$m")
    if [ "$REQ" != "$VERSION" ]; then
        echo "$m requires kindling ${REQ:-(nothing)}, not $VERSION; update it before tagging" >&2
        [ "$DRY_RUN" = "1" ] || exit 1
    fi
done

# main al día
LOCAL=$(git rev-parse --verify main 2>/dev/null || git rev-parse --verify HEAD)
REMOTE=$(git rev-parse --verify origin/main 2>/dev/null || echo "")
if [ -n "$REMOTE" ] && [ "$LOCAL" != "$REMOTE" ]; then
    echo "local main ($LOCAL) != origin/main ($REMOTE): fetch/merge first" >&2
    exit 1
fi

# ── notas ──────────────────────────────────────────────────────────────────
# Las notas son el bloque de esta versión en CHANGELOG.md y una línea de
# instalación. Sin bloque el workflow fallaría tras compilar: se para aquí.
if ! NOTES=$(sh "$(dirname "$0")/release-notes.sh" "$VERSION"); then
    echo "add a \"## [${VERSION#v}] - YYYY-MM-DD\" section to CHANGELOG.md before tagging" >&2
    exit 1
fi

# ── crear tag ──────────────────────────────────────────────────────────────
MSG="release $VERSION — see CHANGELOG.md"

if [ "$DRY_RUN" = "1" ]; then
    echo "(dry-run) would run:"
    echo "  git tag -a $VERSION -m \"$MSG\""
    echo "  git push origin $VERSION"
    echo "GitHub Actions then builds and publishes the release with these notes:"
    echo ""
    printf '%s\n' "$NOTES"
    exit 0
fi

echo "creating annotated tag $VERSION..."
git tag -a "$VERSION" -m "$MSG"

echo "pushing $VERSION to origin (this starts the workflow)..."
git push origin "$VERSION"

cat <<EOF

  ✓ tag $VERSION pushed.

  .github/workflows/release.yml is running. Progress:

      https://github.com/$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')/actions

  When it finishes, the release is at:

      https://github.com/$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')/releases/tag/$VERSION
EOF

# ── esperar al workflow (opcional) ─────────────────────────────────────────
if [ "$WAIT" = "1" ]; then
    if ! command -v gh >/dev/null 2>&1; then
        echo "gh CLI not found, cannot wait for the workflow" >&2
        exit 0
    fi
    echo "waiting for the workflow..."
    REPO=$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')
    gh run watch --exit-status --repo "$REPO" || {
        echo "the workflow failed. See:" >&2
        echo "  https://github.com/$REPO/actions" >&2
        exit 1
    }
    echo "✓ release published"
    gh release view "$VERSION" --repo "$REPO" --web
fi
