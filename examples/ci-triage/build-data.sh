#!/usr/bin/env bash
# Reconstruye los conjuntos y los dos modelos Chispa del ejemplo a partir de
# LogChunks (Brandt, Panichella y Beller, MSR 2020; CC BY 4.0,
# https://doi.org/10.5281/zenodo.3632351). Nada de esto se guarda en el repo.
#
#   examples/ci-triage/build-data.sh OUT_DIR
#
# Deja en OUT_DIR: LogChunks/ (descomprimido), los JSONL de train/valid/test
# (lines-*, category-*, logs-*) y lines.chispa + category.chispa. Con
# SOURCE_DATE_EPOCH fijo, los .chispa salen idénticos byte a byte.
set -euo pipefail

out=${1:?usage: examples/ci-triage/build-data.sh OUT_DIR}
root=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$out"
export SOURCE_DATE_EPOCH=0

# La descarga va fijada por sha256: si Zenodo sirviera otro fichero, se para.
url=https://zenodo.org/api/records/3632351/files/LogChunks.zip/content
sum=fa2e3d10fc700cfe06b92b286741666a6389b46548356da9f0679c0d89be7fa8
zip=$out/LogChunks.zip
if [ ! -f "$zip" ]; then
	echo "downloading LogChunks (24 MB, CC BY 4.0) ..."
	curl -fsSL --max-time 600 -o "$zip.part" "$url"
	mv "$zip.part" "$zip"
fi
got=$(shasum -a 256 "$zip" 2>/dev/null | cut -d' ' -f1 || sha256sum "$zip" | cut -d' ' -f1)
if [ "$got" != "$sum" ]; then
	echo "LogChunks.zip: sha256 $got, want $sum" >&2
	exit 1
fi
[ -d "$out/LogChunks" ] || unzip -q "$zip" -x '__MACOSX/*' -d "$out"

(cd "$root" && go build -o "$out/kling" ./cmd/kling && go build -o "$out/ci-triage" ./examples/ci-triage)

echo "### data"
"$out/ci-triage" data -logchunks "$out/LogChunks" -out "$out" \
	-labels "$root/examples/ci-triage/data/test-categories.tsv"

# Opciones elegidas mirando solo la validación (docs/CI-TRIAGE-EVAL.md).
echo "### line locator (explains / noise)"
"$out/kling" chispa train -data "$out/lines-train.jsonl" -valid "$out/lines-valid.jsonl" -o "$out/lines.chispa"
echo "### category"
"$out/kling" chispa train -data "$out/category-train.jsonl" -valid "$out/category-valid.jsonl" \
	-o "$out/category.chispa" -precision 0.9 -min-support 5

cat <<EOT

Next:
  mkdir -p ~/.config/kling/ci-triage && cp $out/lines.chispa $out/category.chispa ~/.config/kling/ci-triage/
  cp examples/ci-triage/ai.json ~/.config/kling/ai.json   # or merge its models and tasks into yours
  kling ai serve &
  go run ./examples/ci-triage eval -data $out -set test
EOT
