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
#   ./scripts/release.sh --dry-run             # muestra qué haría (o -n); sale
#                                              # con 1 si la release se pararía
#
# Pre-requisitos:
#   - Working tree limpio (sin cambios sin commitear)
#   - HEAD = main = origin/main: se etiqueta el commit que se comprobó
#   - CI verde en ese commit (gh api .../check-runs); gh autenticado
#   - Permisos para pushear tags al repo
#   - CHANGELOG.md con la sección "## [X.Y.Z] - AAAA-MM-DD" (inglés, una
#     línea por cambio): es lo único que llevan las notas de la release; y
#     [Unreleased] vacío, porque la etiqueta lleva todo lo de main
#
# Lo que hace:
#   1. Verifica que el working tree esté limpio
#   2. Verifica que ext/*/go.mod requieran kindling en esa misma versión, y
#      desde v1.0.0 que haya claves públicas de release compiladas
#      (`firmar check`, docs/firmas.md)
#   3. Verifica que CHANGELOG.md tenga el bloque de la versión
#      (scripts/release-notes.sh, el mismo que usa el workflow) y nada en
#      [Unreleased]
#   4. Verifica que todos los checks de CI de ese commit acabaron en verde
#      (los de ci.yml, CI_OBLIGATORIOS, en success: saltados no valen) y,
#      desde v1.0.0, que el e2e de kling-hv pasó en un Apple Silicon (el job
#      de e2e-mac.yml o el commit status que publica scripts/e2e-mac-release.sh;
#      docs/releases.md)
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
# En --dry-run lo que pararía la release no sale en el acto: se cuenta, se
# sigue para enseñarlo todo (y las notas) y se sale con 1 al final. Un -n que
# acaba en 0 es una release que pasaría.
BLOQUEOS=0
bloquea() {
    [ "$DRY_RUN" = "1" ] || exit 1
    BLOQUEOS=$((BLOQUEOS + 1))
}

usage() {
    cat <<EOF
Usage: scripts/release.sh [--dry-run] [--wait] [VERSION]

  --dry-run, -n  show what would happen, and the release notes, without changing
                 anything; exits 1 if any check would stop the release
  --wait      wait for the release workflow to finish
  VERSION     tag to create (e.g. v0.2.0); CHANGELOG.md needs a "## [0.2.0]" section

Environment: VERSION.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run|-n) DRY_RUN=1; shift ;;
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
       bloquea ;;
esac

# Working tree limpio (--dry-run lo permite)
if [ "$DRY_RUN" = "0" ]; then
    if ! git diff --quiet --ignore-submodules HEAD 2>/dev/null; then
        echo "uncommitted changes: commit them or use --dry-run" >&2
        git status --short >&2
        exit 1
    fi
fi

# ext/*/go.mod y vz/go.mod citan en su require la etiqueta que se va a publicar. Con el
# replace ../.. nunca se descarga, pero quien use una extensión como módulo sin
# el replace (go get .../ext/mcp@vX) tiraría de ese require: si se queda en la
# versión anterior, la extensión de vX compilaría contra el núcleo de vX-1.
for m in ext/*/go.mod vz/go.mod; do
    [ -f "$m" ] || continue
    # Una línea (require M v) o dentro de un bloque require ( … ), como vz/go.mod.
    REQ=$(awk '$1=="require" && $2=="github.com/juan52878911/kindling" {print $3}
               $1=="github.com/juan52878911/kindling" && $2 ~ /^v/ {print $2}' "$m" | head -n 1)
    if [ "$REQ" != "$VERSION" ]; then
        echo "$m requires kindling ${REQ:-(nothing)}, not $VERSION; update it before tagging" >&2
        bloquea
    fi
done

# Claves de las releases: desde v1.0.0 (un -rc incluido) la release va firmada
# y sus binarios tienen que traer la pública (clavesRelease de
# internal/upgrade/firma.go, KLING_RELEASE_PUBKEYS de install.sh); si no, el
# workflow fallaría tras compilarlo todo, o peor, un kling 1.x sin claves no
# aceptaría ninguna release. Lo comprueba `firmar check` (con
# KLING_RELEASE_SIGNING_KEY en el entorno, también que es su clave). Sin Go,
# lo mismo a mano sobre install.sh, que un test obliga a coincidir.
if command -v go >/dev/null 2>&1; then
    if ! go run ./internal/upgrade/firmar check -tag "$VERSION"; then
        bloquea
    fi
else
    MAJOR=$(printf '%s' "${VERSION#v}" | cut -d. -f1)
    case "$MAJOR" in
        ''|*[!0-9]*) ;;
        *) if [ "$MAJOR" -ge 1 ] && grep -q '^KLING_RELEASE_PUBKEYS=""' scripts/install.sh; then
               echo "$VERSION must be signed and there are no release public keys (KLING_RELEASE_PUBKEYS, clavesRelease): see docs/firmas.md" >&2
               bloquea
           fi ;;
    esac
fi

# main al día. Sin fetch, origin/main es el de la última vez que se trajo y la
# comparación no dice nada.
if ! git fetch --quiet origin main; then
    echo "git fetch origin main failed: cannot check that main is up to date" >&2
    bloquea
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
    bloquea
fi
REPO=$(git config --get remote.origin.url | sed 's|.*github.com[:/]||;s|\.git$||')

# ── CI verde ───────────────────────────────────────────────────────────────
# Todos los check runs del commit, acabados y en verde. skipped y neutral
# valen para lo opcional (un job que se salta a propósito no es un fallo),
# pero no para los jobs de ci.yml: uno de esos saltado o ausente es un commit
# que CI no comprobó entero. CI_OBLIGATORIOS son sus nombres exactos tal como
# los da GitHub, uno por línea y con la matriz expandida;
# scripts/release_test.go falla si esta lista y ci.yml se separan. Sin ningún
# run, tampoco: un commit que CI no ha visto no se publica.
CI_OBLIGATORIOS='make test cross (.)
make test cross (ext/db)
make test cross (ext/mcp)
make test cross (ext/phone)
make test cross (ext/sandbox)
núcleo sin dependencias ni cgo
kling-unpack (Rust)
go.work
clon real (xfs)
clon real (btrfs)
kling-vz and kling-hv (macOS)
clon real (apfs)
lint (govulncheck, staticcheck, shellcheck, apidiff)'
ci_verde() {
    command -v gh >/dev/null 2>&1 || { echo "gh CLI not found: cannot check CI for $COMMIT" >&2; return 1; }
    # La lista va dentro del filtro como un array de jq: gh --jq no tiene --arg.
    # Los nombres no llevan comillas ni barras.
    OBL=$(printf '%s\n' "$CI_OBLIGATORIOS" | awk 'NF { printf "%s\"%s\"", (n++ ? "," : ""), $0 } END { print "" }')
    # Cada campo sin espacios ("-" si está vacío) para partirlo con set --.
    # shellcheck disable=SC2016 # el $ es de jq, no del shell
    RESUMEN=$(gh api "repos/$REPO/commits/$COMMIT/check-runs?per_page=100" --jq '
        ['"$OBL"'] as $obl
        | [.check_runs[] | {n: .name, s: .status, c: (.conclusion // "")}] as $r
        | ($r | length) as $total
        | ($r | map(select(.s != "completed")) | length) as $pend
        | ($r | map(select(.s == "completed" and (
                (.c | IN("success","skipped","neutral") | not)
                or (.c != "success" and (.n | IN($obl[]))))))
              | map("\(.n)=\(.c)" | gsub(" "; "_"))) as $mal
        | ($obl - ($r | map(.n)) | map(gsub(" "; "_"))) as $faltan
        | "\($total) \($pend) \(if ($mal | length) > 0 then $mal | join(",") else "-" end) \(if ($faltan | length) > 0 then $faltan | join(",") else "-" end)"') || { echo "gh api check-runs failed for $COMMIT" >&2; return 1; }
    # shellcheck disable=SC2086 # se parte a propósito en sus cuatro campos
    set -- $RESUMEN
    TOTAL=${1:-0}; PEND=${2:-0}; MAL=${3:--}; FALTAN=${4:--}
    if [ "$TOTAL" = 0 ]; then echo "no CI check runs for $COMMIT yet" >&2; return 1; fi
    if [ "$PEND" != 0 ]; then echo "CI still running on $COMMIT ($PEND of $TOTAL checks pending)" >&2; return 1; fi
    if [ "$MAL" != "-" ]; then echo "CI not green on $COMMIT: $MAL" >&2; return 1; fi
    if [ "$FALTAN" != "-" ]; then echo "required CI jobs missing on $COMMIT: $FALTAN" >&2; return 1; fi
    echo "CI green on $COMMIT ($TOTAL checks)"
}
if ! ci_verde; then
    echo "see https://github.com/$REPO/commit/$COMMIT/checks" >&2
    bloquea
fi

# ── e2e de kling-hv ────────────────────────────────────────────────────────
# kling-hv es estable desde 1.0 con una condición (compatibilidad.md): cada
# release pasa 92-e2e-mac.sh y 95-e2e-upgrade-mac.sh con VMM=hv en un Apple
# Silicon. Vale el job de e2e-mac.yml en verde sobre este commit (skipped no
# vale: es lo que sale sin runner) o el commit status e2e-mac/kling-hv en
# success que publica scripts/e2e-mac-release.sh cuando pasan los dos. Ya no
# vale KLING_E2E_HV=<sha>: era una línea a mano que no dejaba rastro y que
# nada comprobaba; el status queda en el commit, a la vista.
# El contexto es el de e2e-mac-release.sh: si cambia, en los dos.
E2E_HV_CONTEXTO="e2e-mac/kling-hv"
e2e_hv_hecho() {
    if [ -n "${KLING_E2E_HV:-}" ]; then
        echo "KLING_E2E_HV is no longer accepted: the proof is the $E2E_HV_CONTEXTO commit status" >&2
    fi
    if command -v gh >/dev/null 2>&1; then
        C=$(gh api "repos/$REPO/commits/$COMMIT/check-runs?per_page=100" \
            --jq '[.check_runs[] | select(.name == "microVMs (kling-hv, Hypervisor.framework)" and .conclusion == "success")] | length' 2>/dev/null || echo 0)
        if [ "${C:-0}" -gt 0 ]; then echo "kling-hv e2e green on the Apple Silicon runner"; return 0; fi
        # El combinado da el último status de cada contexto: uno en failure
        # posterior a un success no cuenta.
        S=$(gh api "repos/$REPO/commits/$COMMIT/status" \
            --jq "[.statuses[] | select(.context == \"$E2E_HV_CONTEXTO\" and .state == \"success\")] | length" 2>/dev/null || echo 0)
        if [ "${S:-0}" -gt 0 ]; then echo "kling-hv e2e passed on an Apple Silicon Mac (commit status $E2E_HV_CONTEXTO)"; return 0; fi
    fi
    echo "kling-hv e2e not run on $COMMIT: run scripts/e2e-mac-release.sh on an Apple Silicon Mac with gh authenticated; it publishes the $E2E_HV_CONTEXTO commit status (docs/releases.md)" >&2
    return 1
}
case "${VERSION#v}" in
    0.*) ;;
    *) e2e_hv_hecho || bloquea ;;
esac

# ── notas ──────────────────────────────────────────────────────────────────
# Las notas son el bloque de esta versión en CHANGELOG.md y una línea de
# instalación. Sin bloque el workflow fallaría tras compilar: se para aquí.
if ! NOTES=$(sh "$(dirname "$0")/release-notes.sh" "$VERSION"); then
    echo "add a \"## [${VERSION#v}] - YYYY-MM-DD\" section to CHANGELOG.md before tagging" >&2
    exit 1
fi
# La etiqueta publica todo lo que hay en main: lo que siga bajo [Unreleased]
# iría en los binarios y no en sus notas. Se cuentan las líneas con texto del
# bloque, sin sus ### ni los enlaces del pie.
UNREL=$(awk '
    /^## / { if (dentro) exit; if ($2 == "[Unreleased]") { dentro = 1; next } }
    /^\[[^]]+\]: / { next }
    dentro && /^[^#[:space:]]/ { n++ }
    END { print n + 0 }
' CHANGELOG.md)
if [ "$UNREL" != 0 ]; then
    echo "CHANGELOG.md has $UNREL line(s) under [Unreleased]: move them into \"## [${VERSION#v}] - YYYY-MM-DD\" before tagging" >&2
    bloquea
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
    if [ "$BLOQUEOS" != 0 ]; then
        echo "" >&2
        echo "(dry-run) $BLOQUEOS check(s) above would stop this release" >&2
        exit 1
    fi
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
