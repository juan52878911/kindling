#!/usr/bin/env bash
# Receta del ajuste fino en una instancia con GPU (docs/codificador.md, «Ajuste
# fino con GPU»). Pensada para una g5.xlarge (1× A10G) con la Deep Learning
# Base AMI (Ubuntu 22.04, driver NVIDIA ya instalado). SIN EJECUTAR: pedir
# aprobación antes de lanzar la instancia.
#
#   scp train.jsonl pkg/domotica/indirect.jsonl scripts/encoder-setfit/* scripts/encoder-gguf.sh ubuntu@<ip>:
#   ssh ubuntu@<ip> ./run.sh
#   scp ubuntu@<ip>:work/*.gguf .      # 126 MiB por modelo; luego, apagar la instancia
set -euo pipefail
nvidia-smi --query-gpu=name,memory.total --format=csv
python3 -m venv venv
venv/bin/pip install -q --extra-index-url https://download.pytorch.org/whl/cu128 -r requirements.txt
for m in multilingual-e5-small paraphrase-multilingual-minilm-l12-v2; do
  for seed in 1 2 3; do
    venv/bin/python finetune.py --model "$m" --train train.jsonl --indirect indirect.jsonl --seed "$seed" --out "body-$m-$seed"
    # El mismo conversor fijado, sobre el cuerpo ajustado (sin sha256 esperado:
    # se fija después, cuando el modelo gane su evaluación).
    SRC_DIR="$PWD/body-$m-$seed" WORK="$PWD/work" ./encoder-gguf.sh "$m"
    mv "work/$m-q8_0.gguf" "work/$m-setfit-$seed-q8_0.gguf"
  done
done
sha256sum work/*.gguf
