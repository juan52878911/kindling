#!/usr/bin/env bash
# P3 de la ronda 8 (analisis/ronda8-ia/plan.md §4): la Chispa nueva (chispa-server, modo producto
# en CPU: kNN con el codificador e5 en ONNX Runtime, sin L3) alojada en kindling de punta a punta.
#
#   chispa-p3.sh preparar   (root, en la instancia del banco, con el daemon arriba) clona chispa a un
#                           commit fijo, arma gomod-proxy/, construye la imagen imagen-ort, exporta e5 a
#                           ONNX, genera un banco kNN pequeño, hornea la config, importa las imágenes en
#                           kindling y hace los dorados con `kling ai model deploy`
#   chispa-p3.sh medir      96-von-bench.sh sobre cada dorado (frío, restaurar, primera, memoria) y la
#                           carga por `kling ai serve` frente al mismo binario directo en el host;
#                           $RES/ia-p3-<dorado>.json (esquema von-bench/2, lo lee ia.py) y
#                           $RES/p3-resumen.{json,txt}
#
# Lo llama remoto/ia.sh (prueba p3 sin $KB/sube/chispa/imagen.tar); desde el Mac:
#   IA_PRUEBAS=p3 EC2_DISCO_GB=60 TOPE_MIN=110 BINS_DIR=… scripts/bench/ec2.sh ia
#
# Variantes (P3_VARIANTES, por defecto "claves abierto"):
#   claves   chispa-server con api_keys. En la imagen (y en el dorado) va SOLO el SHA-256 de una clave
#            aleatoria generada en la instancia; la clave vive en $P3/chispa.key (0600) y la pone el
#            gateway en cada petición (`auth` del modelo en ai.json). Sin calentamiento: el -warmup de
#            `kling ai model deploy` no manda cabeceras, y calentar con la clave la dejaría en la
#            memoria del dorado.
#   abierto  chispa-server -abierto sin api_keys (la frontera es el gateway) y dorado calentado con
#            tres peticiones reales (-warmup). Sirve para medir cuánto vale el calentamiento.
#
# Entorno: KB (/opt/kbench), P3_DIR ($KB/p3), RES ($KB/res), KLING (kling), IA_BENCH, HIGIENE,
#   CHISPA_REPO (https://github.com/juan52878911/chispa), CHISPA_COMMIT (42f8650f…, sha completo),
#   CHISPA_SRC (checkout ya hecho: no clona; pruebas), P3_CPUS (IA_CPUS o 4), P3_MEM (MiB, 3072),
#   P3_SESIONES (sesiones ORT de e5: min(4, P3_CPUS)), P3_APT (1: instala paquetes), P3_PY (python
#   con torch y transformers ya puestos: no crea el venv), DOCKER (docker), RUNS (10), REPLICAS (4),
#   CLIENTES ("1 4 16"), CARGA_S (20), P3_CARGA ("1:16 4:4": réplicas:max_inflight de cada tanda de
#   carga; la primera se compara con el directo), P3_SOSTENIDA_S (0: segundos de carga sostenida a 16
#   clientes en la primera variante, el «0 errores en 10 min» del plan = 600), VB (96-von-bench.sh).
set -uo pipefail
KB=${KB:-/opt/kbench}
P3=${P3_DIR:-$KB/p3}
RES=${RES:-$KB/res}
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
KLING=${KLING:-kling}
DOCKER=${DOCKER:-docker}
CHISPA_REPO=${CHISPA_REPO:-https://github.com/juan52878911/chispa}
CHISPA_COMMIT=${CHISPA_COMMIT:-42f8650f257a54efe10d4bc2b1666c54596c9171}
# e5 que fija el repo (model/alt/exportar_e5_onnx.py: REV, licencia MIT comprobada en la ficha).
E5_NOMBRE=intfloat/multilingual-e5-small
E5_REV=614241f622f53c4eeff9890bdc4f31cfecc418b3
P3_CPUS=${P3_CPUS:-${IA_CPUS:-4}}
P3_MEM=${P3_MEM:-3072}
P3_SESIONES=${P3_SESIONES:-$(( P3_CPUS < 4 ? P3_CPUS : 4 ))}
P3_VARIANTES=${P3_VARIANTES:-claves abierto}
PUERTO=8090
PUERTO_DIRECTO=18090
SRC=${CHISPA_SRC:-$P3/chispa}
CTX=$P3/ctx
PREG="¿Este mensaje contiene discurso de odio?"
TAREA=p3

paso() { echo "== $(date +%T) p3: $*"; }
ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
falla() { echo "p3: $*" >&2; exit 1; }
modelo_de() { case $1 in claves) echo chispa-p3 ;; abierto) echo chispa-p3a ;; *) return 1 ;; esac; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

# etapa <nombre> <orden…>: corre la orden y apunta cuánto tardó en $P3/tiempos.jsonl (para dimensionar
# TOPE_MIN y PLAZO_P3_PREP_MIN con cifras de la instancia, no estimadas).
etapa() {
  local n=$1 t0 rc
  shift
  paso "$n"
  t0=$(ms)
  "$@"; rc=$?
  jq -nc --arg n "$n" --argjson ms "$(( $(ms) - t0 ))" --argjson rc "$rc" '{etapa:$n, ms:$ms, rc:$rc}' >> "$P3/tiempos.jsonl"
  [ "$rc" = 0 ] || echo "p3: la etapa $n falló (rc=$rc)" >&2
  return "$rc"
}

# ── preparar ──────────────────────────────────────────────────────────────────
paquetes() {
  [ "${P3_APT:-1}" = 1 ] || return 0
  # Docker sin iptables ni puente: no toca las reglas de kindling (el daemon ya está arriba) y los
  # builds y el `go mod download` van con --network host.
  if [ ! -f /etc/docker/daemon.json ]; then
    mkdir -p /etc/docker
    echo '{"iptables": false, "ip6tables": false, "bridge": "none"}' > /etc/docker/daemon.json
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq || return 1
  apt-get install -y -qq docker.io git python3-venv python3-pip jq perl curl ca-certificates util-linux libstdc++6 >/dev/null || return 1
  systemctl start docker
}

fuente() {
  if [ -z "${CHISPA_SRC:-}" ]; then
    rm -rf "$SRC"; mkdir -p "$SRC"
    git -C "$SRC" init -q && git -C "$SRC" remote add origin "$CHISPA_REPO" || return 1
    # GitHub sirve un commit por su sha; si no, la historia entera.
    git -C "$SRC" fetch -q --depth 1 origin "$CHISPA_COMMIT" || git -C "$SRC" fetch -q origin || return 1
    git -C "$SRC" checkout -q --detach "$CHISPA_COMMIT" || return 1
  fi
  local h
  h=$(git -C "$SRC" rev-parse HEAD) || return 1
  case $h in "$CHISPA_COMMIT"*) ;; *) echo "chispa está en $h, no en $CHISPA_COMMIT" >&2; return 1 ;; esac
  # Lo que el Dockerfile tiene que traer fijado: si falta, el build de chispa falla, pero mejor saberlo ya.
  local df=$SRC/deploy/kindling/Dockerfile a
  for a in GO_DIGEST DEBIAN_DIGEST; do
    grep -Eq "^ARG $a=sha256:[0-9a-f]{64}$" "$df" || { echo "$df no fija $a" >&2; return 1; }
  done
  grep -Eq '^ARG ORT_NUPKG_SHA256=[0-9a-f]{64}$' "$df" || { echo "$df no fija ORT_NUPKG_SHA256" >&2; return 1; }
}

# arg_df <nombre>: el valor por defecto de un ARG del Dockerfile de chispa.
arg_df() { sed -n "s/^ARG $1=//p" "$SRC/deploy/kindling/Dockerfile" | head -1; }

# gomod-proxy/: los Dockerfile de chispa compilan con GOPROXY=file:///gomod-proxy y -mod=readonly,
# así que hace falta un árbol de proxy con todos los módulos de server/go.sum (kindling v0.17.0 y
# onnxruntime_go v1.38.0). Se saca de un `go mod download` en la misma imagen de Go del Dockerfile
# (por digest): $GOMODCACHE/cache/download YA tiene la forma de un GOPROXY de ficheros. Va contra
# proxy.golang.org y sum.golang.org, y cada módulo se comprueba con server/go.sum.
gomod() {
  [ -d "$SRC/gomod-proxy" ] && { echo "  gomod-proxy/ ya existe"; return 0; }
  local img
  img="golang:$(arg_df GO_VERSION)-bookworm@$(arg_df GO_DIGEST)"
  rm -rf "$P3/gomodcache"; mkdir -p "$P3/gomodcache"
  "$DOCKER" run --rm --network host -v "$SRC:/src" -v "$P3/gomodcache:/gomodcache" -e GOMODCACHE=/gomodcache \
    -e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly -w /src/server "$img" go mod download -x >"$P3/gomod.log" 2>&1 \
    || { tail -20 "$P3/gomod.log" >&2; return 1; }
  [ -d "$P3/gomodcache/cache/download" ] || { echo "go mod download no dejó cache/download" >&2; return 1; }
  mkdir -p "$SRC/gomod-proxy"
  cp -a "$P3/gomodcache/cache/download/." "$SRC/gomod-proxy/" || return 1
  find "$SRC/gomod-proxy" -name '*.lock' -delete
  rm -rf "$SRC/gomod-proxy/sumdb"
  if [ -n "$(git -C "$SRC" status --porcelain -- server/go.mod server/go.sum)" ]; then
    echo "go mod download cambió server/go.mod o go.sum: el build no sería el del commit" >&2; return 1
  fi
}

# La imagen del modo producto en CPU: destino imagen-ort de deploy/kindling/Dockerfile (cgo + ORT 1.31.1
# de NuGet con su sha256, bases por digest), con el builder clásico como lo construyó el repo.
imagen_base() {
  local a=(build --network host --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64
           -f "$SRC/deploy/kindling/Dockerfile" --target imagen-ort -t chispa-p3-base:ort "$SRC")
  DOCKER_BUILDKIT=0 "$DOCKER" "${a[@]}" >"$P3/build-base.log" 2>&1 && return 0
  # El docker.io de Debian 12 es 20.10: si su builder clásico no traga el Dockerfile, BuildKit (el que trae dentro).
  echo "  el builder clásico falló ($(tail -1 "$P3/build-base.log")); otra vez con BuildKit"
  DOCKER_BUILDKIT=1 "$DOCKER" "${a[@]}" --progress plain >>"$P3/build-base.log" 2>&1 \
    || { tail -30 "$P3/build-base.log" >&2; return 1; }
}

# venv con las versiones que fija el repo para exportar e5 y generar bancos (torch 2.5.1 CPU y
# transformers 4.46.3: otras mueven los vectores) y lo que piden sus módulos (numpy, pyyaml, onnx).
python_venv() {
  if [ -n "${P3_PY:-}" ]; then PY=$P3_PY; return 0; fi
  PY=$P3/venv/bin/python
  [ -x "$PY" ] && "$PY" -c 'import torch, transformers' 2>/dev/null && return 0
  python3 -m venv "$P3/venv" || return 1
  "$PY" -m pip install -q --upgrade pip >/dev/null || return 1
  "$PY" -m pip install -q torch==2.5.1 --index-url https://download.pytorch.org/whl/cpu || return 1
  "$PY" -m pip install -q transformers==4.46.3 numpy==1.26.4 pyyaml==6.0.2 onnx==1.17.0 onnxruntime==1.20.1 \
    huggingface_hub==0.26.5 || return 1
}

# e5: el modelo por revisión fijada desde el hub (público, MIT; exportar_e5_onnx.py comprueba la licencia en
# la ficha), exportado a ONNX con el script del repo (--comprobar: error frente a PyTorch) y el
# tokenizador .chispa-tok. El repo no fija el sha256 de e5.onnx (PRODUCTO.md: los pesos no van en git);
# si se da E5_ONNX_SHA256 se exige, y siempre se apunta en el resumen.
e5() {
  mkdir -p "$CTX/modelos" "$P3/hf"
  HF_HUB_DISABLE_TELEMETRY=1 "$PY" -I -c '
import sys
from huggingface_hub import snapshot_download
print(snapshot_download(sys.argv[1], revision=sys.argv[2], cache_dir=sys.argv[3],
      allow_patterns=["*.json", "model.safetensors", "sentencepiece.bpe.model", "README.md"]))' \
    "$E5_NOMBRE" "$E5_REV" "$P3/hf" > "$P3/e5-snap.txt" || return 1
  SNAPDIR=$(tail -1 "$P3/e5-snap.txt")
  [ -f "$SNAPDIR/model.safetensors" ] || { echo "la revisión $E5_REV no trae model.safetensors" >&2; return 1; }
  (cd "$SRC" && HF_HUB_OFFLINE=1 "$PY" -I model/alt/exportar_e5_onnx.py --salida "$CTX/modelos" --cache "$P3/hf" \
    --revision "$E5_REV" --comprobar) > "$P3/e5-export.log" 2>&1 || { tail -20 "$P3/e5-export.log" >&2; return 1; }
  (cd "$SRC" && "$PY" -I runtime/tok/exportar.py "$SNAPDIR" "$CTX/modelos/e5.chispa-tok") > "$P3/e5-tok.log" 2>&1 \
    || { tail -20 "$P3/e5-tok.log" >&2; return 1; }
  [ -s "$CTX/modelos/e5.onnx" ] && [ -s "$CTX/modelos/e5.chispa-tok" ] || { echo "faltan e5.onnx o e5.chispa-tok" >&2; return 1; }
  if [ -n "${E5_ONNX_SHA256:-}" ] && [ "$(sha "$CTX/modelos/e5.onnx")" != "$E5_ONNX_SHA256" ]; then
    echo "e5.onnx no tiene el sha256 pedido (E5_ONNX_SHA256)" >&2; return 1
  fi
}

# Banco kNN pequeño con ejemplos del propio repo: el fixture de hatecheck (17 frases inventadas,
# CC-BY-4.0 según su ficha) como oro de una tarea de dos clases, con generar_banco.py (los vectores del
# servidor). Si falla, el banco se queda vacío y las peticiones llevan los ejemplos en examples[].
banco() {
  mkdir -p "$CTX/bancos"
  python3 -I -c '
import csv, json, sys
preg = sys.argv[2]
with open(sys.argv[1], encoding="utf-8") as f, open(sys.argv[3], "w", encoding="utf-8") as o:
    for r in csv.DictReader(f):
        et = "odio" if r["label_gold"] == "hateful" else "no_odio"
        o.write(json.dumps({"id": r["mhc_case_id"], "pregunta": preg, "tipo": "choice", "texto": r["test_case"],
                            "opciones": ["odio", "no_odio"], "etiqueta": et}, ensure_ascii=False) + "\n")' \
    "$SRC/model/datos/fixtures/hatecheck/test.csv" "$PREG" "$P3/oro.jsonl" || return 1
  BANCO=0
  if (cd "$SRC" && CHISPA_E5="$SNAPDIR" HF_HUB_OFFLINE=1 "$PY" -m model.alt.generar_banco --tarea "$TAREA" \
        --oro "$P3/oro.jsonl" --salida "$CTX/bancos" --k 8) > "$P3/banco.log" 2>&1 \
     && [ -s "$CTX/bancos/banco-$TAREA.npy" ]; then
    BANCO=1
  else
    echo "AVISO p3: generar_banco falló (ver $P3/banco.log): las peticiones llevan examples[]" >&2
    tail -5 "$P3/banco.log" >&2
    rm -rf "${CTX:?}/bancos"; mkdir -p "$CTX/bancos"
  fi
  echo "$BANCO" > "$P3/banco.ok"
}

# peticion <texto>: el cuerpo de /v1/decide (con examples[] si no hay banco).
peticion() {
  local ej='null'
  [ "$(cat "$P3/banco.ok")" = 1 ] || ej=$(jq -sc 'map({text: .texto, label: .etiqueta})' "$P3/oro.jsonl")
  jq -nc --arg s "$1" --arg q "$PREG" --argjson ej "$ej" '{state:$s, questions:[{id:"q1", question:$q, kind:"choice",
    options:[{id:"odio", description:"discurso de odio contra un grupo"}, {id:"no_odio", description:"no es discurso de odio"}]}
    + (if $ej == null then {} else {examples:$ej} end)]}'
}

# config: la clave en la instancia (0600, fuera del contexto de la imagen) y en la imagen solo su hash.
config() {
  umask 077
  [ -s "$P3/chispa.key" ] || od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > "$P3/chispa.key"
  chmod 600 "$P3/chispa.key"
  printf 'Authorization: Bearer %s\n' "$(cat "$P3/chispa.key")" > "$P3/auth.hdr"
  umask 022
  local h tareas='{}'
  h=$(tr -d '\n' < "$P3/chispa.key" | sha256sum | cut -d' ' -f1)
  [ "$(cat "$P3/banco.ok")" = 1 ] && tareas=$(jq -nc --arg t "$TAREA" --arg q "$PREG" \
    '{($t): {banco: ("/opt/chispa/bancos/banco-" + $t), preguntas: [$q]}}')
  local knn
  knn=$(jq -nc --argjson t "$tareas" --argjson s "$P3_SESIONES" '{modo:"cpu", codificador:"e5-small",
    e5_onnx:"/opt/chispa/modelos/e5.onnx", e5_sesiones:$s, cache_vectores_mb:64, umbral:0, tareas:$t}')
  jq -n --argjson k "$knn" '{knn:$k, limites:{max_ejemplos:256, max_ejemplo_texto:16384}}' > "$CTX/config-abierta.json"
  # rps alto: el cubo por clave no debe ser el cuello del banco.
  jq --arg h "$h" '{api_keys:[{id:"banco-p3", hash_sha256:$h, rps:100000, rafaga:100000}]} + .' \
    "$CTX/config-abierta.json" > "$CTX/config-claves.json"
  peticion "Detesto a los vecinos de la otra calle." > "$P3/peticion.json"
  local s
  : > "$P3/warmup.jsonl"
  for s in "Respeto a los vecinos zurdos." "Detesto a la gente pelirroja." "Hoy hace buen día para pasear."; do
    jq -nc --argjson b "$(peticion "$s")" '{method:"POST", path:"/v1/decide", body:$b}' >> "$P3/warmup.jsonl"
  done
  # La clave no puede acabar en la imagen.
  if grep -rqF -- "$(cat "$P3/chispa.key")" "$CTX"; then echo "¡la clave está en el contexto de la imagen!" >&2; return 1; fi
}

# Dockerfile hijo por variante (C1 del análisis): pesos, banco y config horneados; sin entrada.sh (que
# pide -modelo, el .chispa clásico) y fuera de /modelos (la base lo declara VOLUME).
imagenes() {
  local v cfg extra
  for v in $P3_VARIANTES; do
    case $v in claves) cfg=config-claves.json; extra='' ;; abierto) cfg=config-abierta.json; extra='"-abierto", ' ;;
      *) echo "variante desconocida: $v" >&2; return 1 ;; esac
    cat > "$CTX/Dockerfile.$v" <<EOF
FROM chispa-p3-base:ort
COPY modelos/ /opt/chispa/modelos/
COPY bancos/ /opt/chispa/bancos/
COPY $cfg /opt/chispa/config.json
ENV GOMEMLIMIT=512MiB
ENTRYPOINT ["/usr/local/bin/chispa-server", "-addr", "0.0.0.0:$PUERTO", ${extra}"-config", "/opt/chispa/config.json", "-tok", "/opt/chispa/modelos/e5.chispa-tok", "-timeout", "60s", "-apagado", "35s"]
CMD []
EOF
    DOCKER_BUILDKIT=0 "$DOCKER" build -q --network host -f "$CTX/Dockerfile.$v" -t "chispa-p3:$v" "$CTX" > "$P3/build-$v.log" 2>&1 \
      || { tail -20 "$P3/build-$v.log" >&2; return 1; }
    "$DOCKER" save -o "$P3/chispa-p3-$v.tar" "chispa-p3:$v" || return 1
    "$KLING" image import -archive "$P3/chispa-p3-$v.tar" -name "chispa-p3-$v" -replace || return 1
  done
}

# El binario y libonnxruntime.so de la imagen, para la referencia directa en el host (mismo Debian 12).
binario_host() {
  local id
  mkdir -p "$P3/host"
  id=$("$DOCKER" create --network none chispa-p3-base:ort) || return 1
  "$DOCKER" cp "$id:/usr/local/bin/chispa-server" "$P3/host/chispa-server" \
    && "$DOCKER" cp "$id:/usr/lib/libonnxruntime.so" "$P3/host/libonnxruntime.so"; local rc=$?
  "$DOCKER" rm "$id" >/dev/null
  [ "$rc" = 0 ] && chmod 755 "$P3/host/chispa-server"
}

# ultimo_dorado <modelo>: la versión más reciente <modelo>-v<n> en el daemon.
ultimo_dorado() {
  "$KLING" snapshots -json | jq -r --arg m "$1" '[.[] | select(.name | test("^" + $m + "-v[0-9]+$"))]
    | sort_by(.created_at) | last | .name // empty'
}

dorados() {
  local v n t0 rc dor wu
  [ -f "$P3/ai.json" ] || echo '{"models": {}}' > "$P3/ai.json"
  chmod 600 "$P3/ai.json"
  : > "$P3/dorados.jsonl"
  for v in $P3_VARIANTES; do
    n=$(modelo_de "$v") || return 1
    wu=(); [ "$v" = abierto ] && wu=(-warmup "$P3/warmup.jsonl")
    t0=$(ms)
    timeout 1800 "$KLING" ai model deploy "$n" -config "$P3/ai.json" -image "chispa-p3-$v" -contract chispa-decide \
      -port "$PUERTO" -health /healthz -health-timeout 10s -cpus "$P3_CPUS" -mem "$P3_MEM" -max-inflight 16 -wait 15m \
      ${wu[@]+"${wu[@]}"}; rc=$?
    [ "$rc" = 0 ] || { echo "falló kling ai model deploy $n" >&2; return 1; }
    dor=$(ultimo_dorado "$n")
    [ -n "$dor" ] || { echo "no encuentro el dorado de $n" >&2; return 1; }
    # deploy no reescribe el bloque de un modelo que ya estaba: se apunta a la versión nueva aquí, y la
    # variante con claves recibe la credencial que añade el gateway.
    jq --arg n "$n" --arg d "$dor" --arg v "$v" --arg k "$P3/chispa.key" \
      '.models[$n].snapshot = $d | if $v == "claves" then .models[$n].auth = {header:"Authorization", scheme:"Bearer", file:$k}
        else .models[$n] |= del(.auth) end' "$P3/ai.json" > "$P3/ai.json.n" && mv "$P3/ai.json.n" "$P3/ai.json" || return 1
    chmod 600 "$P3/ai.json"
    jq -nc --arg v "$v" --arg n "$n" --arg d "$dor" --argjson ms "$(( $(ms) - t0 ))" --argjson w "${#wu[@]}" \
      '{variante:$v, modelo:$n, dorado:$d, fabricar_ms:$ms, calentado:($w > 0)}' >> "$P3/dorados.jsonl"
  done
}

preparar() {
  [ "$(id -u)" = 0 ] || [ "${P3_APT:-1}" = 0 ] || { echo "preparar va como root (o con P3_APT=0)" >&2; exit 2; }
  mkdir -p "$P3" "$RES"
  exec > >(tee -a "$P3/preparar.log") 2>&1
  : > "$P3/tiempos.jsonl"
  command -v jq >/dev/null || [ "${P3_APT:-1}" = 1 ] || falla "falta jq"
  local t0
  t0=$(ms)
  etapa paquetes paquetes || exit 1
  etapa fuente fuente || exit 1
  etapa gomod gomod || exit 1
  etapa imagen-base imagen_base || exit 1
  etapa python python_venv || exit 1
  [ -n "${PY:-}" ] || PY=${P3_PY:-$P3/venv/bin/python}
  etapa e5 e5 || exit 1
  etapa banco banco || exit 1
  etapa config config || exit 1
  etapa imagenes imagenes || exit 1
  etapa binario-host binario_host || exit 1
  etapa dorados dorados || exit 1
  local tam=()
  for v in $P3_VARIANTES; do tam+=("$v=$("$DOCKER" image inspect -f '{{.Size}}' "chispa-p3:$v" 2>/dev/null || echo 0)"); done
  jq -n --arg c "$CHISPA_COMMIT" --arg e5 "$E5_NOMBRE@$E5_REV" --arg so "$(sha "$CTX/modelos/e5.onnx")" \
    --arg st "$(sha "$CTX/modelos/e5.chispa-tok")" --arg sw "$(sha "$SNAPDIR/model.safetensors")" \
    --argjson bo "$(cat "$P3/banco.ok")" --argjson cpus "$P3_CPUS" --argjson mem "$P3_MEM" --argjson ses "$P3_SESIONES" \
    --argjson t "$(jq -sc . "$P3/tiempos.jsonl")" --argjson d "$(jq -sc . "$P3/dorados.jsonl")" \
    --argjson total "$(( $(ms) - t0 ))" --arg tam "${tam[*]}" \
    '{chispa_commit:$c, e5:$e5, sha256:{"e5.onnx":$so, "e5.chispa-tok":$st, "model.safetensors":$sw}, banco_knn:($bo == 1),
      vcpus:$cpus, mem_mib:$mem, e5_sesiones:$ses, etapas:$t, dorados:$d, preparar_ms:$total,
      imagen_bytes:($tam | split(" ") | map(select(. != "") | split("=") | {(.[0]): (.[1] | tonumber)}) | add)}' \
    > "$P3/preparado.json" || exit 1
  cp "$P3/preparado.json" "$RES/p3-preparado.json"
  paso "fin preparar ($(( ( $(ms) - t0 ) / 1000 )) s)"
}

# ── medir ─────────────────────────────────────────────────────────────────────
# directo_arriba <variante>: chispa-server en el host con la misma config (rutas del host), los mismos
# vCPU (taskset: NumCPU de Go respeta la afinidad, así que las sesiones e hilos de ORT son los de la
# réplica) y escuchando en loopback; deja el pid en $DPID.
directo_arriba() {
  local cfg=config-abierta.json ts=() i
  [ "$1" = claves ] && cfg=config-claves.json
  sed "s#/opt/chispa/#$CTX/#g" "$CTX/$cfg" > "$P3/host/config-$1.json"
  command -v taskset >/dev/null && ts=(taskset -c "0-$(( P3_CPUS - 1 ))")
  ${ts[@]+"${ts[@]}"} env GOMEMLIMIT=512MiB "$P3/host/chispa-server" -addr "127.0.0.1:$PUERTO_DIRECTO" \
    -config "$P3/host/config-$1.json" -tok "$CTX/modelos/e5.chispa-tok" -ort-lib "$P3/host/libonnxruntime.so" \
    -timeout 60s > "$RES/p3-directo-$1.log" 2>&1 &
  DPID=$!
  for i in $(seq 1 120); do
    curl -fs -o /dev/null "http://127.0.0.1:$PUERTO_DIRECTO/healthz" && break
    kill -0 "$DPID" 2>/dev/null || { echo "el chispa-server directo murió: $(tail -3 "$RES/p3-directo-$1.log")" >&2; return 1; }
    sleep 0.5
  done
  [ "$i" -lt 120 ] || return 1
  # Caliente como la réplica: las peticiones del warmup.
  local hdr=() w
  [ "$1" = claves ] && hdr=(-H "@$P3/auth.hdr")
  while read -r w <&4; do
    jq -c .body <<<"$w" | curl -fs -o /dev/null ${hdr[@]+"${hdr[@]}"} -H 'Content-Type: application/json' --data-binary @- \
      "http://127.0.0.1:$PUERTO_DIRECTO/v1/decide" || { echo "el directo no contesta a /v1/decide" >&2; return 1; }
  done 4< "$P3/warmup.jsonl"
}
# mem_proc <pid> → {"rss_mib","pss_mib"} de /proc/<pid>/smaps_rollup (o null).
mem_proc() {
  if [ -r "/proc/$1/smaps_rollup" ]; then
    awk '/^Rss:/{r=$2} /^Pss:/{p=$2} END {printf "{\"rss_mib\":%d,\"pss_mib\":%d}\n", r/1024, p/1024}' "/proc/$1/smaps_rollup"
  else echo null; fi
}
directo_abajo() {
  [ -n "${DPID:-}" ] || return 0
  kill "$DPID" 2>/dev/null
  for _ in $(seq 1 20); do kill -0 "$DPID" 2>/dev/null || break; sleep 0.5; done
  kill -9 "$DPID" 2>/dev/null; wait "$DPID" 2>/dev/null; DPID=""
}

# bloque <modelo> <max_inflight>: el bloque del modelo en el ai.json de preparar, para el gateway del banco.
bloque() {
  jq -c --arg n "$1" --argjson mi "$2" '.models[$n] + {max_inflight:$mi}' "$P3/ai.json" > "$P3/bloque-$1-$2.json" || return 1
  echo "$P3/bloque-$1-$2.json"
}

medir() {
  mkdir -p "$RES"
  exec > >(tee -a "$RES/p3-medir.log") 2>&1
  [ -s "$P3/dorados.jsonl" ] || falla "falta $P3/dorados.jsonl: corre antes chispa-p3.sh preparar"
  if [ -z "${VB:-}" ]; then
    for VB in "$KB/96-von-bench.sh" "$AQUI/../../96-von-bench.sh" ""; do [ -e "$VB" ] && break; done
  fi
  [ -n "$VB" ] || falla "no encuentro 96-von-bench.sh"
  export KLING
  local carga=${P3_CARGA:-1:16 4:4} primera_vez=1 l v n dor tok base out rm mi bf ext dmem tanda dir
  trap 'directo_abajo' EXIT
  while read -r l <&3; do
    v=$(jq -r .variante <<<"$l"); n=$(jq -r .modelo <<<"$l"); dor=$(jq -r .dorado <<<"$l")
    tok=""; [ "$v" = claves ] && tok=$P3/chispa.key
    base=(CONTRATO=chispa-decide PUERTO="$PUERTO" REQ_PATH=/v1/decide REQ_BODY="@$P3/peticion.json" HEALTH_PATH=/healthz
          MODELO="$n" TOKEN_FILE="$tok" RUNS="${RUNS:-10}" REPLICAS="${REPLICAS:-4}" CLIENTES="${CLIENTES:-1 4 16}"
          CARGA_S="${CARGA_S:-20}")
    paso "$v ($dor): frío, restaurar, primera y memoria"
    bf=$(bloque "$n" 16) || exit 1
    env "${base[@]}" GW_BLOQUE_FILE="$bf" COLD=1 SECCIONES="frio restaurar primera memoria" \
      JSON_OUT="$P3/parcial-$dor.json" timeout "$(( ${PLAZO_P3_VAR_MIN:-30} * 60 ))" bash "$VB" "$dor" \
      > "$RES/p3-$dor.txt" 2>&1 || echo "p3: 96-von-bench ($v) rc=$?" >&2
    ext='{}'; dmem=null; tanda=0
    for rm in $carga; do
      mi=${rm#*:}; rm=${rm%%:*}; tanda=$(( tanda + 1 ))
      bf=$(bloque "$n" "$mi") || exit 1
      dir=""
      if [ "$tanda" = 1 ]; then
        paso "$v: chispa-server directo en el host (referencia)"
        if directo_arriba "$v"; then dir="http://127.0.0.1:$PUERTO_DIRECTO/v1/decide"; dmem=$(mem_proc "$DPID"); fi
      fi
      paso "$v: carga por el gateway con $rm réplica(s), max_inflight $mi${dir:+, y directo}"
      out=$P3/parcial-$dor-carga-r$rm.json
      env "${base[@]}" GW_BLOQUE_FILE="$bf" CARGA_REPLICAS="$rm" SECCIONES=carga LOAD_DIRECT_URL="$dir" \
        JSON_OUT="$out" timeout "$(( ${PLAZO_P3_VAR_MIN:-30} * 60 ))" bash "$VB" "$dor" \
        >> "$RES/p3-$dor.txt" 2>&1 || echo "p3: carga r$rm ($v) rc=$?" >&2
      [ -n "$dir" ] && { [ "$dmem" = null ] || dmem=$(jq -c --argjson a "$(mem_proc "$DPID")" '. + {tras_carga:$a}' <<<"$dmem"); directo_abajo; }
      [ -s "$out" ] && ext=$(jq -c --arg k "r$rm" --slurpfile c "$out" '. + {($k): $c[0].carga}' <<<"$ext")
    done
    if [ "$primera_vez" = 1 ] && [ "${P3_SOSTENIDA_S:-0}" -gt 0 ]; then
      paso "$v: carga sostenida, 16 clientes durante ${P3_SOSTENIDA_S} s"
      bf=$(bloque "$n" 16) || exit 1
      out=$P3/parcial-$dor-sostenida.json
      env "${base[@]}" GW_BLOQUE_FILE="$bf" CARGA_REPLICAS=1 CLIENTES=16 CARGA_S="$P3_SOSTENIDA_S" SECCIONES=carga \
        JSON_OUT="$out" timeout "$(( P3_SOSTENIDA_S + 600 ))" bash "$VB" "$dor" >> "$RES/p3-$dor.txt" 2>&1 \
        || echo "p3: carga sostenida rc=$?" >&2
      [ -s "$out" ] && ext=$(jq -c --slurpfile c "$out" '. + {sostenida: $c[0].carga}' <<<"$ext")
    fi
    primera_vez=0
    # Un JSON por dorado con el esquema de 96-von-bench.sh (lo lee ia.py): la carga es la de la primera
    # tanda (la que se compara con el directo); las demás y lo de P3 van en claves extra.
    if [ -s "$P3/parcial-$dor.json" ]; then
      local c1=$P3/parcial-$dor-carga-r${carga%%:*}.json
      [ -s "$c1" ] || c1=/dev/null
      jq --slurpfile c "$c1" --argjson ext "$ext" --argjson dm "$dmem" \
        --argjson d "$l" --slurpfile p "$P3/preparado.json" \
        '(if ($c | length) > 0 then .carga = $c[0].carga else . end)
         | . + {carga_tandas:$ext, p3:{variante:$d.variante, dorado:$d, directo_mem:$dm, preparado:($p[0] // null)}}' \
        "$P3/parcial-$dor.json" > "$RES/ia-p3-$dor.json" || echo "p3: no pude juntar el JSON de $dor" >&2
    fi
  done 3< "$P3/dorados.jsonl"
  resumen
}

# resumen: p3-resumen.json y .txt con las cifras que pide el plan, por variante.
resumen() {
  local fs=()
  for f in "$RES"/ia-p3-*.json; do [ -e "$f" ] && fs+=("$f"); done
  [ ${#fs[@]} -gt 0 ] || { echo "p3: sin resultados que resumir" >&2; return 1; }
  jq -s 'def p50: (map(select(. != null)) | sort) as $s | if ($s|length) == 0 then null else $s[(($s|length) - 1) / 2 | floor] end;
    def pts($c): ($c.gateway.puntos // []) | map({clientes, rps, p50_ms, p95_ms, p99_ms, errores});
    map({variante: .p3.variante, dorado: .modelo.snapshot, fabricar_ms: .p3.dorado.fabricar_ms, calentado: .p3.dorado.calentado,
         frio_hasta_primera_ms: (.frio.hasta_primera_ms // null),
         restaurar_primera_p50_ms: ([.primera.restaurar_caliente[]?.total_ms] | p50),
         restaurar_primera_peticion_p50_ms: ([.primera.restaurar_caliente[]?.peticion_ms] | p50),
         restaurar_fria_p50_ms: (if (.primera.restaurar_fria | type) == "array" then [.primera.restaurar_fria[].total_ms] | p50 else null end),
         descongelar_primera_p50_ms: ([.primera.descongelar_caliente[]?.total_ms] | p50),
         pss_por_replicas: ([.memoria.restauradas[]? | {replicas, pss_total_mib, rss_total_mib}]),
         carga_gateway: pts(.carga), carga_directo: ((.carga.directo.puntos // []) | map({clientes, rps, p50_ms, p95_ms, p99_ms, errores})),
         carga_tandas: (.carga_tandas | with_entries(.value |= pts(.))), directo_mem: .p3.directo_mem})' "${fs[@]}" \
    > "$RES/p3-resumen.json" || return 1
  {
    echo "P3 · chispa-server en kindling (modo CPU, e5 en ORT)"
    jq -r '.chispa_commit as $c | "chispa \($c[0:12]), \(.e5), banco kNN: \(.banco_knn), \(.vcpus) vCPU, \(.mem_mib) MiB, \(.e5_sesiones) sesiones ORT, preparar \(.preparar_ms/1000|floor) s"' \
      "$P3/preparado.json" 2>/dev/null
    jq -r '.[] | "\n[\(.variante)] \(.dorado): fabricar \(.fabricar_ms/1000|floor) s, calentado \(.calentado)",
      "  frío (kling run -image → 1.ª decisión) \(.frio_hasta_primera_ms) ms",
      "  restaurar → 1.ª decisión p50 \(.restaurar_primera_p50_ms) ms (petición \(.restaurar_primera_peticion_p50_ms) ms; caché fría \(.restaurar_fria_p50_ms) ms); descongelar \(.descongelar_primera_p50_ms) ms",
      (.pss_por_replicas[] | "  \(.replicas) réplica(s): PSS \(.pss_total_mib) MiB, RSS \(.rss_total_mib) MiB"),
      (.carga_gateway[] as $g | "  gateway \($g.clientes) cl: \($g.rps|floor) dec/s p50 \($g.p50_ms) p95 \($g.p95_ms) ms err \($g.errores)"),
      (.carga_directo[] as $d | "  directo \($d.clientes) cl: \($d.rps|floor) dec/s p50 \($d.p50_ms) p95 \($d.p95_ms) ms err \($d.errores)"),
      (.carga_tandas | to_entries[] | .key as $k | .value[] |
        "  tanda \($k) \(.clientes) cl: \(.rps|floor) dec/s p50 \(.p50_ms) p95 \(.p95_ms) ms err \(.errores)"),
      "  directo en el host: \(.directo_mem)"' "$RES/p3-resumen.json"
  } > "$RES/p3-resumen.txt"
  cat "$RES/p3-resumen.txt"
}

case "${1:-}" in
preparar) preparar ;;
medir) medir ;;
*) echo "uso: chispa-p3.sh preparar|medir" >&2; exit 2 ;;
esac
