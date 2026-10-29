#!/bin/sh
# release.sh — crea un tag anotado y lo pushea. El workflow de GitHub Actions
# detecta el tag, compila los binarios y publica la release.
#
# Una etiqueta publica todo el repo: el núcleo, kling-vz y kling-hv, las extensiones de
# ext/ (kling-mcp, kling-bridge, kling-sandbox, kling-db, kling-phone), el operador
# (binarios e imagen GHCR) y los tar del host, con un único SHA256SUMS.
#
# USO
#   ./scripts/release.sh                        # tag v0.1.0 (lee VERSION del env o del último tag)
#   VERSION=v0.2.0 ./scripts/release.sh        # tag explícito
#   ./scripts/release.sh --dry-run             # muestra qué haría
#
# Pre-requisitos:
#   - Working tree limpio (sin cambios sin commitear)
#   - HEAD = main = origin/main: se etiqueta el commit que se comprobó
#   - CI verde en ese commit (gh api .../check-runs); gh autenticado
#   - Permisos para pushear tags al repo
#   - CHANGELOG.md con la sección "## [X.Y.Z] - AAAA-MM-DD" (inglés, una
#     línea por cambio): es lo único que llevan las notas de la release
#
# Lo que hace:
#   1. Verifica que el working tree esté limpio
#   2. Verifica que ext/*/go.mod requieran kindling en esa misma versión
#   3. Verifica que CHANGELOG.md tenga el bloque de la versión
#      (scripts/release-notes.sh, el mismo que usa el workflow)
#   4. Verifica que todos los checks de CI de ese commit acabaron en verde
#   5. Crea un tag anotado vX.Y.Z con mensaje corto SOBRE ESE COMMIT
#   6. Push del tag → activa .github/workflows/release.yml
#   7. (Opcional) espera a ESE run del workflow y abre la release en el browser

# -e y -u siempre; pipefail donde el sh lo tenga (bash, zsh, dash reciente).
# Ningún paso depende de que un error se ignore: los que pueden fallar sin que
# sea fatal van con `|| ...` explícito o dentro de un if.
set -eu
# shellcheck disable=SC3040 # se comprueba antes en una subshell
if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi

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
    # Sin tubería: con pipefail, `git tag | head -1` puede morir de SIGPIPE.
    LAST=$(git for-each-ref --count=1 --sort=-v:refname --format='%(refname:short)' 'refs/tags/v*' 2>/dev/null || true)
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

if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null 2>&1; then
    echo "tag $VERSION already exists locally" >&2
    exit 1
fi

# El tag puede existir ya en origin sin estar en local (lo empujó otra máquina):
# el push fallaría después de haber creado el tag local.
# ls-remote --exit-code: 0 = existe, 2 = no existe, otro = no se pudo preguntar.
RC=0
git ls-remote --exit-code --tags origin "refs/tags/$VERSION" >/dev/null 2>&1 || RC=$?
case "$RC" in
    0) echo "tag $VERSION already exists on origin" >&2; exit 1 ;;
    2) ;;
    *) echo "cannot query tags on origin (git ls-remote exit $RC)" >&2
       [ "$DRY_RUN" = "1" ] || exit 1 ;;
esac

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

# main al día. Sin fetch, origin/main es el de la última vez que se trajo y la
# comparación no dice nada.
if ! git fetch --quiet origin main; then
    echo "git fetch origin main failed: cannot check that main is up to date" >&2
    [ "$DRY_RUN" = "1" ] || exit 1
fi
LOCAL=$(git rev-parse --verify main 2>/dev/null || git rev-parse --verify HEAD 2>/dev/null || true)
REMOTE=$(git rev-parse --verify origin/main 2>/dev/null || echo "")
if [ -n "$REMOTE" ] && [ "$LOCAL" != "$REMOTE" ]; then
    echo "local main ($LOCAL) != origin/main ($REMOTE): fetch/merge first" >&2
    exit 1
fi
# Lo que se comprueba aquí (go.mod de ext/, CHANGELOG, CI) es el árbol de
# trabajo, que es HEAD: si HEAD no es main, se comprobaría una cosa y se
# etiquetaría otra. Se etiqueta COMMIT, explícito, y no "lo que haya en HEAD".
COMMIT="${REMOTE:-$LOCAL}"
HEAD_SHA=$(git rev-parse --verify HEAD 2>/dev/null || echo "")
if [ "$HEAD_SHA" != "$COMMIT" ]; then
    echo "HEAD ($HEAD_SHA) is not main ($COMMIT): check out main before tagging" >&2
    [ "$DRY_RUN" = "1" ] || exit 1
fi
REPO=$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')

# ── CI verde ───────────────────────────────────────────────────────────────
# Todos los check runs del commit, acabados y en verde (skipped y neutral
# cuentan como verde: un job que se salta a propósito no es un fallo). Sin
# ninguno, tampoco: un commit que CI no ha visto no se publica.
ci_verde() {
    command -v gh >/dev/null 2>&1 || { echo "gh CLI not found: cannot check CI for $COMMIT" >&2; return 1; }
    # shellcheck disable=SC2016 # el $ es de jq, no del shell
    RESUMEN=$(gh api "repos/$REPO/commits/$COMMIT/check-runs?per_page=100" --jq '
        [.check_runs[] | {n: .name, s: .status, c: (.conclusion // "")}] as $r
        | ($r | length) as $total
        | ($r | map(select(.s != "completed")) | length) as $pend
        | ($r | map(select(.s == "completed" and (.c | IN("success","skipped","neutral") | not))) | map(.n | gsub(" "; "_"))) as $mal
        | "\($total) \($pend) \($mal | join(","))"') || { echo "gh api check-runs failed for $COMMIT" >&2; return 1; }
    # shellcheck disable=SC2086 # se parte a propósito en sus tres campos
    set -- $RESUMEN
    TOTAL=${1:-0}; PEND=${2:-0}; MAL=${3:-}
    if [ "$TOTAL" = 0 ]; then echo "no CI check runs for $COMMIT yet" >&2; return 1; fi
    if [ "$PEND" != 0 ]; then echo "CI still running on $COMMIT ($PEND of $TOTAL checks pending)" >&2; return 1; fi
    if [ -n "$MAL" ]; then echo "CI failed on $COMMIT: $MAL" >&2; return 1; fi
    echo "CI green on $COMMIT ($TOTAL checks)"
}
if ! ci_verde; then
    echo "see https://github.com/$REPO/commit/$COMMIT/checks" >&2
    [ "$DRY_RUN" = "1" ] || exit 1
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
    echo "  git tag -a $VERSION -m \"$MSG\" $COMMIT"
    echo "  git push origin $VERSION"
    echo "GitHub Actions then builds and publishes the release with these notes:"
    echo ""
    printf '%s\n' "$NOTES"
    exit 0
fi

echo "creating annotated tag $VERSION on $COMMIT..."
git tag -a "$VERSION" -m "$MSG" "$COMMIT"

echo "pushing $VERSION to origin (this starts the workflow)..."
git push origin "$VERSION"

cat <<EOF

  ✓ tag $VERSION pushed.

  .github/workflows/release.yml is running. Progress:

      https://github.com/$REPO/actions

  When it finishes, the release is at:

      https://github.com/$REPO/releases/tag/$VERSION
EOF

# ── esperar al workflow (opcional) ─────────────────────────────────────────
if [ "$WAIT" = "1" ]; then
    if ! command -v gh >/dev/null 2>&1; then
        echo "gh CLI not found, cannot wait for the workflow" >&2
        exit 0
    fi
    # El run de ESTE tag: sin id, `gh run watch` pregunta (o coge otro run
    # que esté en marcha). Tarda unos segundos en aparecer tras el push.
    RUN_ID=""
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
        RUN_ID=$(gh run list --repo "$REPO" --workflow release.yml --commit "$COMMIT" \
            --event push --limit 1 --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null || true)
        [ -n "$RUN_ID" ] && break
        sleep 5
    done
    if [ -z "$RUN_ID" ]; then
        echo "no release.yml run for $COMMIT after 60 s. See https://github.com/$REPO/actions" >&2
        exit 1
    fi
    echo "waiting for run $RUN_ID..."
    gh run watch "$RUN_ID" --exit-status --repo "$REPO" || {
        echo "the workflow failed. See:" >&2
        echo "  https://github.com/$REPO/actions" >&2
        exit 1
    }
    echo "✓ release published"
    gh release view "$VERSION" --repo "$REPO" --web
fi
