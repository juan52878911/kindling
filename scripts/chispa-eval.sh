#!/usr/bin/env bash
# Reproduce docs/CHISPA-EVAL.md: construye el conjunto de commits a partir de los
# repos locales, calcula las líneas base y entrena/evalúa las variantes de Chispa.
#
#   scripts/chispa-eval.sh OUT_DIR REPO...
#   scripts/chispa-eval.sh /tmp/chispa ~/Documents/GitHub/* ~/Github/*
#
# Los datos se quedan en OUT_DIR (no en el repo). Variables: HOLDOUT (repo que se
# guarda entero para la prueba entre repos, OpenWA por defecto), TRAIN_PCT y
# VALID_PCT (reparto temporal por repo, 60/20 por defecto: con 10 % de
# validación no hay bastantes ejemplos para fijar τ por clase; ver el informe).
set -euo pipefail

out=${1:?usage: scripts/chispa-eval.sh OUT_DIR REPO...}
shift
[ $# -gt 0 ] || { echo "usage: scripts/chispa-eval.sh OUT_DIR REPO..." >&2; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
holdout=${HOLDOUT:-OpenWA}
mkdir -p "$out"
export SOURCE_DATE_EPOCH=0 # modelos reproducibles byte a byte

(cd "$root" && go build -o "$out/kling" ./cmd/kling && go build -o "$out/chispa-commits" ./tools/chispa-commits)
k=$out/kling

echo "### dataset"
"$out/chispa-commits" build -out "$out" -holdout "$holdout" \
	-train-pct "${TRAIN_PCT:-60}" -valid-pct "${VALID_PCT:-20}" "$@"

section() { echo; echo "### $*"; }

section "baselines (temporal split)"
"$out/chispa-commits" baseline -train "$out/train.jsonl" -test "$out/test.jsonl"

section "baselines (cross-repo: $holdout held out)"
"$out/chispa-commits" baseline -train "$out/xrepo-train.jsonl" -test "$out/xrepo-test.jsonl"

train() { # nombre, split (vacío = temporal, xrepo- = entre repos), opciones...
	local name=$1 split=$2
	shift 2
	section "chispa $name (${split:-temporal})"
	"$k" chispa train -data "$out/${split}train.jsonl" -valid "$out/${split}valid.jsonl" \
		-test "$out/${split}test.jsonl" -o "$out/$name.chispa" "$@"
}

train words-fields ""
train words-only "" -fields=false
train words-char-fields "" -char 3-5
train words-fields-p90 "" -precision 0.90
train words-fields-p99 "" -precision 0.99
train xrepo-words-fields xrepo-
train xrepo-words-only xrepo- -fields=false
train fix-vs-rest "" -one-vs-rest fix

section "inspect"
"$k" chispa inspect "$out/words-fields.chispa"
