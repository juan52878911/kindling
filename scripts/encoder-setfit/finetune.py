"""Ajuste fino contrastivo (el de SetFit) del cuerpo del codificador.

SetFit entrena en dos pasos: primero ajusta el codificador con pares de frases
(misma intención = parecidas, distinta = lejanas, pérdida de coseno) y luego
entrena una cabeza sobre sus vectores. Aquí solo va el primer paso; la cabeza es
la de kindling (Go, .jenc), que se entrena después sobre los vectores del
codificador ajustado igual que sobre los del congelado (docs/codificador.md).

Necesita GPU para ser razonable (ver la receta en docs/codificador.md). NO se ha
ejecutado todavía: las versiones de requirements.txt salen de los metadatos de
PyPI y conviene comprobarlas en la primera pasada.

    python finetune.py --train train.jsonl --indirect indirect.jsonl --out body/
"""
import argparse
import json
import random
from collections import defaultdict

import torch
from datasets import Dataset
from sentence_transformers import (SentenceTransformer, SentenceTransformerTrainer,
                                   SentenceTransformerTrainingArguments, losses)

# Pesos de partida fijados (los mismos que convierte scripts/encoder-gguf.sh).
MODELS = {
    "multilingual-e5-small": ("intfloat/multilingual-e5-small", "614241f622f53c4eeff9890bdc4f31cfecc418b3", "query: "),
    "paraphrase-multilingual-minilm-l12-v2": ("sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2",
                                              "e8f8c211226b894fcb81acc59f3b34ba3efd5f42", ""),
}


def read(path, split=None):
    rows = [json.loads(l) for l in open(path, encoding="utf-8") if l.strip()]
    return [r for r in rows if split is None or r.get("split") == split]


def pairs(rows, iters, rng):
    """Pares al estilo SetFit: por cada frase, `iters` pares positivos (misma
    intención) y `iters` negativos (otra), con etiqueta 1/0."""
    by = defaultdict(list)
    for r in rows:
        by[r["intent"]].append(r["text"])
    labels = sorted(by)
    a, b, y = [], [], []
    for r in rows:
        for _ in range(iters):
            a.append(r["text"]); b.append(rng.choice(by[r["intent"]])); y.append(1.0)
            other = rng.choice([l for l in labels if l != r["intent"]])
            a.append(r["text"]); b.append(rng.choice(by[other])); y.append(0.0)
    return a, b, y


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="multilingual-e5-small", choices=sorted(MODELS))
    ap.add_argument("--train", required=True, help="unified JSONL (tools/domotica-data build)")
    ap.add_argument("--indirect", help="pkg/domotica/indirect.jsonl: its train split is always used, whole")
    ap.add_argument("--per-class", type=int, default=64, help="sentences sampled per intent (the few-shot of SetFit)")
    ap.add_argument("--iters", type=int, default=20, help="positive and negative pairs per sentence")
    ap.add_argument("--epochs", type=int, default=1)
    ap.add_argument("--batch", type=int, default=64)
    ap.add_argument("--lr", type=float, default=2e-5)
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--out", required=True, help="output directory (sentence-transformers format, for encoder-gguf.sh SRC_DIR=)")
    a = ap.parse_args()

    repo, rev, prefix = MODELS[a.model]
    rng = random.Random(a.seed)
    torch.manual_seed(a.seed)
    by = defaultdict(list)
    for r in read(a.train):
        by[r["intent"]].append(r)
    rows = []
    for intent in sorted(by):
        rs = by[intent]
        rng.shuffle(rs)
        rows += rs[: a.per_class]
    if a.indirect:
        rows += read(a.indirect, "train")
    for r in rows:
        r["text"] = prefix + r["text"]
    s1, s2, y = pairs(rows, a.iters, rng)
    print(f"{len(rows)} sentences, {len(y)} pairs, {len(by)} intents")

    model = SentenceTransformer(repo, revision=rev)
    args = SentenceTransformerTrainingArguments(
        output_dir=a.out + ".work", num_train_epochs=a.epochs, per_device_train_batch_size=a.batch,
        learning_rate=a.lr, warmup_ratio=0.1, seed=a.seed, fp16=torch.cuda.is_available(),
        logging_steps=100, save_strategy="no", report_to=[])
    trainer = SentenceTransformerTrainer(
        model=model, args=args,
        train_dataset=Dataset.from_dict({"sentence1": s1, "sentence2": s2, "score": y}),
        loss=losses.CosineSimilarityLoss(model))
    trainer.train()
    model.save(a.out)
    print("saved", a.out)


if __name__ == "__main__":
    main()
