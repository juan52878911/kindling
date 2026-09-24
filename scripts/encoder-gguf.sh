#!/usr/bin/env bash
# Convierte un codificador de frases del catálogo de kindling a GGUF con el
# conversor de llama.cpp, TODO fijado: la revisión de los pesos en Hugging Face
# (y el sha256 de cada fichero), la versión de llama.cpp (la misma b11147 que
# sirve los modelos), la de uv y las de los paquetes de Python. El resultado se
# compara con el sha256 del catálogo (pkg/von/embed.go): si no coincide, no se
# instala.
#
# Por qué convertir y no descargar un GGUF: ni intfloat ni sentence-transformers
# publican GGUF, y los de terceros no se pueden comprobar. Una conversión
# reproducible sí: cualquiera puede repetirla y obtener el mismo fichero.
#
# Uso (en el host Linux del daemon, arm64 o x86_64; necesita python3 y red):
#
#   scripts/encoder-gguf.sh multilingual-e5-small            # convierte y verifica
#   sudo scripts/encoder-gguf.sh multilingual-e5-small -install
#       # además lo deja en $KLING_ROOT/cache/von/models/<sha256>.gguf, donde lo
#       # busca el constructor llm (kling models add enc-e5 -model multilingual-e5-small)
#
# Trabaja en $WORK (por defecto ~/.cache/kindling/encoder-gguf, ~1,5 GB con el
# entorno de Python; bórralo al terminar). Tarda unos minutos, casi todo en
# instalar PyTorch.
set -euo pipefail

MODEL=${1:?usage: $0 <multilingual-e5-small|paraphrase-multilingual-minilm-l12-v2> [-install]}
INSTALL=${2:-}
WORK=${WORK:-$HOME/.cache/kindling/encoder-gguf}
KLING_ROOT=${KLING_ROOT:-/var/lib/kindling}

LLAMA_TAG=b11147
LLAMA_SRC_SHA256=29187719f9e9390e4b20108bb6401574ca95849bba3743f904f09b93a0c28288
UV_VERSION=0.12.18

case "$(uname -m)" in
aarch64 | arm64) UV_ARCH=aarch64-unknown-linux-gnu; UV_SHA256=afb6291f3f0a6b4521fc67b947822506c41dde5b60d2189dd8f3695b2ac8c9e7 ;;
x86_64) UV_ARCH=x86_64-unknown-linux-gnu; UV_SHA256=${UV_SHA256_X86:?set UV_SHA256_X86 to the sha256 of uv-x86_64-unknown-linux-gnu.tar.gz $UV_VERSION from its GitHub release page} ;;
*) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# Ficheros de cada modelo con su sha256 en la revisión fijada. Los dos son
# BertModel con el tokenizador SentencePiece de XLM-RoBERTa (250k piezas).
case "$MODEL" in
multilingual-e5-small)
	REPO=intfloat/multilingual-e5-small
	REV=614241f622f53c4eeff9890bdc4f31cfecc418b3
	WANT=${WANT_SHA256:-}
	FILES="
69137736cab8b8903a07fe8afaafdda25aac55415a12a55d1bffa9f581abf959 config.json
1a55775f53449dac10a2bcbc312469fac40b96d53198c407081a831f81c98477 model.safetensors
cfc8146abe2a0488e9e2a0c56de7952f7c11ab059eca145a0a727afce0db2865 sentencepiece.bpe.model
d05497f1da52c5e09554c0cd874037a083e1dc1b9cfd48034d1c717f1afc07a7 special_tokens_map.json
0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39 tokenizer.json
a1d6bc8734a6f635dc158508bef000f8e2e5a759c7d92f984b2c86e5ff53425b tokenizer_config.json
c6e29747481e8b5dd2b58401966aeac910de39092f90cda9a704b1545f902b04 modules.json
948201d8329907aae938fa62f9ceeed53f5694dacc2b87b9f3b78b37ee986529 sentence_bert_config.json
987f7a67a38fa564c849bb5d277c52ab9088a84368fc0be31a354125aebb12a0 1_Pooling/config.json"
	;;
paraphrase-multilingual-minilm-l12-v2)
	REPO=sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2
	REV=e8f8c211226b894fcb81acc59f3b34ba3efd5f42
	WANT=${WANT_SHA256:-}
	FILES="
6300193cb75e01cf80c96decef7187dfb33094d97cc1490b7ead6ff134476e4e config.json
eaa086f0ffee582aeb45b36e34cdd1fe2d6de2bef61f8a559a1bbc9bd955917b model.safetensors
cfc8146abe2a0488e9e2a0c56de7952f7c11ab059eca145a0a727afce0db2865 sentencepiece.bpe.model
378eb3bf733eb16e65792d7e3fda5b8a4631387ca04d2015199c4d4f22ae554d special_tokens_map.json
2c3387be76557bd40970cec13153b3bbf80407865484b209e655e5e4729076b8 tokenizer.json
5036ea374ffedd706e3bef33e2e0d6953cb868ef8a490e76e32ba0faa37a6b9b tokenizer_config.json
71b44701d7efd054205115acfa6ef126c5d2f84bd3affe0c59e48163674d19a6 unigram.json
8f4b264b80206c830bebbdcae377e137925650a433b689343a63bdc9b3145460 modules.json
70f4448f31320443fe3557cacea5abf2dcc4915dda8c80646bec9f3bb0aa5a1f sentence_bert_config.json
4be450dde3b0273bb9787637cfbd28fe04a7ba6ab9d36ac48e92b11e350ffc23 1_Pooling/config.json"
	;;
*) echo "unknown encoder $MODEL" >&2; exit 1 ;;
esac
# El sha256 esperado del GGUF sale del catálogo si no se da a mano.
if [ -z "$WANT" ] && command -v kling >/dev/null; then
	WANT=$(kling models ls -json | python3 -c "import json,sys;print(next(m['SHA256'] for m in json.load(sys.stdin)['catalog'] if m['ID']=='$MODEL'))" 2>/dev/null || true)
fi

fetch() { # url sha256 destino
	if [ -f "$3" ] && echo "$2  $3" | sha256sum -c --status; then return; fi
	mkdir -p "$(dirname "$3")"
	curl -fsSL --retry 3 -o "$3.part" "$1"
	echo "$2  $3.part" | sha256sum -c --quiet
	mv "$3.part" "$3"
}

mkdir -p "$WORK" && cd "$WORK"
fetch "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/uv-$UV_ARCH.tar.gz" "$UV_SHA256" uv.tgz
[ -x "uv-$UV_ARCH/uv" ] || tar xzf uv.tgz
UV="$PWD/uv-$UV_ARCH/uv"
fetch "https://codeload.github.com/ggml-org/llama.cpp/tar.gz/refs/tags/$LLAMA_TAG" "$LLAMA_SRC_SHA256" "llama-$LLAMA_TAG.tgz"
if [ ! -f "llama-$LLAMA_TAG/convert_hf_to_gguf.py" ]; then
	mkdir -p "llama-$LLAMA_TAG" && tar xzf "llama-$LLAMA_TAG.tgz" -C "llama-$LLAMA_TAG" --strip-components=1
fi
# Las versiones de requirements/requirements-convert_hf_to_gguf.txt de b11147,
# fijadas exactas (el fichero deja rangos). PyTorch solo CPU.
if [ ! -x venv/bin/python ]; then
	UV_CACHE_DIR="$WORK/uvcache" "$UV" venv -q -p python3 venv
	UV_CACHE_DIR="$WORK/uvcache" "$UV" pip install -q -p venv/bin/python --index-strategy unsafe-best-match \
		--extra-index-url https://download.pytorch.org/whl/cpu \
		torch==2.11.0 numpy==2.2.6 sentencepiece==0.2.1 transformers==4.57.6 protobuf==4.25.8 "./llama-$LLAMA_TAG/gguf-py"
fi

SRC="src/$MODEL"
echo "$FILES" | while read -r sum f; do
	[ -n "$f" ] && fetch "https://huggingface.co/$REPO/resolve/$REV/$f" "$sum" "$SRC/$f"
done

# El conversor lee config.json y ve "BertModel": con eso buscaría un
# vocabulario WordPiece, y estos modelos usan el SentencePiece de XLM-RoBERTa.
# Se le presenta como XLMRobertaModel (misma red, otro tokenizador), pero con
# pad_token_id a null: con un pad_token_id, el conversor recorta las dos
# primeras filas de las posiciones, como pide RoBERTa (sus posiciones empiezan
# en pad+1), y estos modelos son BERT, con posiciones desde 0. Comprobado
# contra transformers: coseno 1,00000 en F16 y >= 0,999 en Q8_0
# (docs/codificador.md).
CONV="conv/$MODEL"
rm -rf "$CONV" && mkdir -p "$CONV"
for f in "$SRC"/*; do [ -f "$f" ] && ln -s "$PWD/$f" "$CONV/"; done
ln -s "$PWD/$SRC/1_Pooling" "$CONV/1_Pooling"
rm "$CONV/config.json"
venv/bin/python - "$SRC/config.json" "$CONV/config.json" <<'EOF'
import json, sys
c = json.load(open(sys.argv[1]))
c["architectures"] = ["XLMRobertaModel"]
c["pad_token_id"] = None
json.dump(c, open(sys.argv[2], "w"), indent=2)
EOF
OUT="$WORK/$MODEL-q8_0.gguf"
venv/bin/python "llama-$LLAMA_TAG/convert_hf_to_gguf.py" "$CONV" --outtype q8_0 --outfile "$OUT" >"$WORK/$MODEL.log" 2>&1 ||
	{ tail -20 "$WORK/$MODEL.log" >&2; exit 1; }
GOT=$(sha256sum "$OUT" | cut -d' ' -f1)
echo "$OUT"
echo "sha256 $GOT  size $(stat -c %s "$OUT")"
if [ -n "$WANT" ] && [ "$GOT" != "$WANT" ]; then
	echo "MISMATCH: the catalog pins $WANT; not installing" >&2
	exit 1
fi
if [ "$INSTALL" = "-install" ]; then
	[ -n "$WANT" ] || { echo "no pinned sha256 to check against (set WANT_SHA256); not installing" >&2; exit 1; }
	install -D -m 0644 "$OUT" "$KLING_ROOT/cache/von/models/$GOT.gguf"
	echo "installed $KLING_ROOT/cache/von/models/$GOT.gguf"
fi
