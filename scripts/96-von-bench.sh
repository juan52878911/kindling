#!/usr/bin/env bash
# Mide un modelo de IA alojado en kindling (un dorado de `kling ai model add`,
# o de `kling ai model deploy` con otro contrato) en el daemon al que apunte
# kling: lo que docs/von.md cuenta en sus tablas y lo que piden P1, P2 y P3 del
# plan de la ronda 8. Saca un resumen legible por la salida estándar y un JSON
# con todas las claves en JSON_OUT (las secciones que no se miden dicen por qué
# en {"omitida": …}). scripts/bench/ia.py lo valida y lo resume.
#
#   ./96-von-bench.sh von-smol                 dorado ya hecho
#   COLD=1 ./96-von-bench.sh von-smol          + arranque en frío desde la imagen
#   RUNS=10 REPLICAS=4 ./96-von-bench.sh von-smol
#   CONTRATO=chispa-decide REQ_BODY=@peticion.json MODELO=chispa ./96-von-bench.sh chispa
#
# Secciones (SECCIONES, en este orden; por defecto todas, y frio solo con COLD=1):
#   frio       del arranque en frío (kling run -image) a la primera respuesta;
#   restaurar  kling run -from + primera respuesta, RUNS veces (lo de siempre);
#   primera    P1: primera respuesta tras restaurar y tras descongelar, con la
#              caché de páginas caliente y tras drop_caches; fallos de página
#              del VMM, bytes leídos de disco y fases del despertar (resync).
#              Las calientes van todas antes que las frías, cada una con el
#              dorado traído a la caché (API warm, ficheros bajo KLING_ROOT y
#              una restauración descartada); la que aun así lee más de
#              UMBRAL_CALIENTE_BYTES (0) se aparta y se repite (REINTENTOS_CALIENTE, 2);
#   tps        tokens/s de prompt y de generación en una réplica caliente (openai);
#   memoria    P2: PSS/RSS de 1..REPLICAS réplicas en tres variantes —restauradas,
#              descongeladas (congeladas y vueltas a despertar, lo que hace el
#              segador) e idle: remove por el gateway— con Cached y MemAvailable
#              del host y el disco de cada réplica dormida;
#   ociosa     P2: «ociosa → primera respuesta», CYCLES ciclos alternando thaw y
#              restaurar, y por el gateway con idle freeze, idle remove e idle
#              remove con una réplica de repuesto (spare: 1);
#   arranque   primera respuesta tras arrancar el gateway con la caché fría: en
#              cuanto contesta, tras ARRANQUE_ESPERA_S (precalentado al arrancar)
#              y anclado (pin: true); ARRANQUE_RUNS vueltas; necesita drop_caches;
#   semillas   dos réplicas no repiten la misma muestra a temperatura 1 (openai);
#   carga      P3: CLIENTES clientes por /v1/m/<modelo>/… del gateway durante
#              CARGA_S s cada tanda (y contra LOAD_DIRECT_URL si se da).
#
# Entorno: KLING, RUNS (5), REPLICAS (4), CYCLES (10), MEM_RONDAS (1: rondas de
# memoria, el orden de las variantes rota en cada una), JSON_OUT, GATEWAY (1;
# 0 = sin gateway: se omiten remove, la parte de gateway de ociosa y carga),
# IDLE_S (5: -idle del gateway del banco), CLIENTES ("1 4 16"), CARGA_S (20),
# DROP_CACHES (auto: si /proc/sys/vm/drop_caches se puede escribir; 0 = nunca),
# HIGIENE (0; 1 = `pkill -9 -x firecracker` entre variantes: SOLO en una
# instancia del banco, nunca en el laboratorio), KLING_ROOT (raíz de datos del
# daemon, si se puede leer: calienta los ficheros del dorado), UMBRAL_CALIENTE_BYTES,
# REINTENTOS_CALIENTE, ARRANQUE_RUNS (3), ARRANQUE_ESPERA_S (20), IA_BENCH (cliente de
# scripts/ia-bench), y para modelos que no son VON: CONTRATO, PUERTO,
# REQ_PATH, REQ_BODY (texto o @fichero), HEALTH_PATH y MODELO (nombre en el
# ai.json del gateway del banco; por defecto el del dorado). Para un servidor
# con claves (chispa-server con api_keys, P3): TOKEN_FILE (fichero 0600 con la
# clave, ruta absoluta: va como Bearer en las peticiones directas a la réplica y
# a LOAD_DIRECT_URL, y como `auth` del modelo en el gateway del banco).
# GW_BLOQUE_FILE: bloque del modelo para el gateway del banco (el de un ai.json
# de `kling ai model deploy`); el banco solo le pone snapshot, idle y
# max_replicas. MAX_INFLIGHT (1) del bloque generado y CARGA_REPLICAS
# (REPLICAS) de la sección carga.
#
# Los tiempos incluyen arrancar el CLI de kling (~10 ms) y el proxy del daemon:
# es lo que ve un cliente que llega por el socket.

set -uo pipefail

KLING="${KLING:-kling}"
SNAP="${1:?usage: 96-von-bench.sh <template> (made with kling ai model add or deploy)}"
RUNS="${RUNS:-5}"
REPLICAS="${REPLICAS:-4}"
COLD="${COLD:-0}"
CYCLES="${CYCLES:-10}"
MEM_RONDAS="${MEM_RONDAS:-1}"
GATEWAY="${GATEWAY:-1}"
IDLE_S="${IDLE_S:-5}"
CLIENTES="${CLIENTES:-1 4 16}"
CARGA_S="${CARGA_S:-20}"
DROP_CACHES="${DROP_CACHES:-auto}"
HIGIENE="${HIGIENE:-0}"
KLING_ROOT="${KLING_ROOT:-}"
UMBRAL_CALIENTE_BYTES="${UMBRAL_CALIENTE_BYTES:-0}"
REINTENTOS_CALIENTE="${REINTENTOS_CALIENTE:-2}"
case $UMBRAL_CALIENTE_BYTES$REINTENTOS_CALIENTE in *[!0-9]*) echo "UMBRAL_CALIENTE_BYTES and REINTENTOS_CALIENTE must be integers" >&2; exit 1 ;; esac
TODAS="restaurar primera tps memoria ociosa arranque semillas carga"
[ "$COLD" = 1 ] && TODAS="frio $TODAS"
SECCIONES="${SECCIONES:-$TODAS}"
[ "$COLD" = 1 ] && case " $SECCIONES " in *" frio "*) ;; *) SECCIONES="frio $SECCIONES" ;; esac
PREFIX="von-bench-$$"
JSON_OUT="${JSON_OUT:-${TMPDIR:-/tmp}/von-bench-$SNAP-$$.json}"

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need jq; need perl
IA_BENCH="${IA_BENCH:-ia-bench}"
command -v "$IA_BENCH" >/dev/null 2>&1 || IA_BENCH=""

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
step()   { printf "\n\033[1m%s\033[0m\n" "$1"; }
row()    { printf "  %-44s %s\n" "$1" "$2"; }
# stats lee números por stdin y saca "p50 (min–max)".
stats()  { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "n/a"; exit} m=a[int((NR+1)/2)]; printf "%s (%s–%s, n=%d)\n", m, a[1], a[NR], NR}'; }
pide()   { case " $SECCIONES " in *" $1 "*) return 0 ;; esac; return 1; }
LINUX=0; [ "$(uname -s)" = Linux ] && [ -r /proc/meminfo ] && LINUX=1

D="$(mktemp -d)"
GWS=""   # etiquetas de los gateways del banco que siguen vivos
# Piezas del JSON: una por sección en $D/j-<clave>.json; al final se juntan.
put()  { printf '{"%s":%s}\n' "$1" "$2" > "$D/j-$1.json"; }
omit() { put "$1" "$(jq -nc --arg r "$2" '{omitida: $r}')"; }
# arr convierte los argumentos (números) en una lista JSON.
arr()  { [ $# -gt 0 ] || { echo '[]'; return; }; printf '%s\n' "$@" | jq -sc 'map(select(. != null))'; }
# jsonl junta las líneas de un fichero en una lista (vacía si no existe).
jsonl() { if [ -s "$1" ]; then jq -sc . "$1"; else echo '[]'; fi; }

nuestras() { "$KLING" ps -a -json 2>/dev/null | jq -r --arg p "$1" '.[]? | select(.name|startswith($p)) | .name'; }
borrar_prefijo() {
  local ms
  ms="$(nuestras "$1")"
  # shellcheck disable=SC2086
  [ -n "$ms" ] && "$KLING" rm -f $ms >/dev/null 2>&1
  return 0
}
gw_down() { # gw_down <etq>
  local pid
  pid="$(cat "$D/gw-$1/pid" 2>/dev/null)"
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null; for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done; kill -9 "$pid" 2>/dev/null; fi
  rm -f "$D/gw-$1/pid"
  borrar_prefijo "$PREFIX-g$1-"
}
cleanup() {
  for g in $GWS; do gw_down "$g"; done
  borrar_prefijo "$PREFIX-"
  rm -rf "$D"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# higiene entre variantes: nada nuestro vivo y, en la instancia del banco, ningún VMM suelto.
higiene() {
  borrar_prefijo "$PREFIX-"
  if [ "$HIGIENE" = 1 ]; then pkill -9 -x firecracker >/dev/null 2>&1; sleep 1; fi
  [ -z "$(nuestras "$PREFIX-")" ] || echo "  aviso: quedan máquinas $PREFIX-* tras la higiene" >&2
}

# ── el modelo ─────────────────────────────────────────────────────────────────
info="$("$KLING" snapshots -json | jq -c --arg s "$SNAP" '.[] | select(.name==$s)')"
[ -n "$info" ] || { echo "no snapshot named $SNAP" >&2; exit 1; }
model="$(jq -r '.labels["von.model"] // empty' <<<"$info")"
CONTRATO="${CONTRATO:-}"
if [ -z "$CONTRATO" ]; then
  [ -n "$model" ] || { echo "$SNAP is not a VON model (no von.model label): set CONTRATO" >&2; exit 1; }
  CONTRATO=openai
fi
[ -n "$model" ] || model="$SNAP"
image="$(jq -r .image <<<"$info")"
vcpus="$(jq -r .vcpus <<<"$info")"
mem="$(jq -r .mem_mib <<<"$info")"
cpupct="$(jq -r '.cpu_pct // 0' <<<"$info")"
puerto_def="$(jq -r '(.labels["kling.ports"] // "8000") | split(",")[0]' <<<"$info")"
PUERTO="${PUERTO:-$puerto_def}"
# VON: un dorado de `kling ai model add` (llama-server), al que se habla con `kling ai model ask`.
VON=0; [ "$CONTRATO" = openai ] && [ -n "$(jq -r '.labels["von.model"] // empty' <<<"$info")" ] && VON=1
case $CONTRATO in
  openai) path_def=/v1/chat/completions; health_def=/health
          body_def='{"messages":[{"role":"user","content":"Hi"}],"max_tokens":1,"temperature":0}' ;;
  openai-embeddings) path_def=/v1/embeddings; health_def=/health; body_def='{"input":"hola"}' ;;
  chispa-decide) path_def=/v1/decide; health_def=/healthz; body_def="" ;;
  chispa-classify) path_def=/v1/classify; health_def=/healthz; body_def="" ;;
  *) path_def=""; health_def=/health; body_def="" ;;
esac
REQ_PATH="${REQ_PATH:-$path_def}"
REQ_BODY="${REQ_BODY:-$body_def}"
HEALTH_PATH="${HEALTH_PATH:-$health_def}"
MODELO="${MODELO:-$SNAP}"
[ -n "$REQ_PATH" ] || { echo "set REQ_PATH for contract $CONTRATO" >&2; exit 1; }
[ "$VON" = 1 ] || [ -n "$REQ_BODY" ] || { echo "set REQ_BODY for contract $CONTRATO" >&2; exit 1; }
TOKEN_FILE="${TOKEN_FILE:-}"
TOK=()
if [ -n "$TOKEN_FILE" ]; then
  case $TOKEN_FILE in /*) ;; *) echo "TOKEN_FILE must be an absolute path" >&2; exit 1 ;; esac
  [ -r "$TOKEN_FILE" ] || { echo "cannot read TOKEN_FILE $TOKEN_FILE" >&2; exit 1; }
  TOK=(-token-file "$TOKEN_FILE")
fi
BODY_FILE="$D/peticion.json"
case $REQ_BODY in @*) cp "${REQ_BODY#@}" "$BODY_FILE" || exit 1 ;; *) printf '%s' "$REQ_BODY" > "$BODY_FILE" ;; esac
backend="$("$KLING" status -v -json | jq -r '.backend // "?"')"

step "Model $model  (snapshot $SNAP, image $image, $vcpus vCPU, $mem MiB, backend $backend, contract $CONTRATO)"
snap_mem="$(jq -r '(.mem_bytes // 0)/1048576|floor' <<<"$info")"
snap_disk="$(jq -r '(.disk_bytes // 0)/1048576|floor' <<<"$info")"
img_disk="$("$KLING" image ls -json | jq -r --arg i "$image" '[.[]|select(.name==$i)|.disk_bytes/1048576|floor][0] // null')"
row "snapshot memory file (allocated)" "$snap_mem MiB"
row "snapshot on disk (memory + state + overlay)" "$snap_disk MiB"
row "image layer on disk" "$img_disk MiB"
put modelo "$(jq -nc --arg s "$SNAP" --arg m "$model" --arg c "$CONTRATO" --arg i "$image" --arg b "$backend" \
  --argjson p "$PUERTO" --argjson v "$vcpus" --argjson mm "$mem" --argjson cp "$cpupct" \
  --argjson sm "$snap_mem" --argjson sd "$snap_disk" --argjson id "$img_disk" --arg rp "$REQ_PATH" --arg n "$MODELO" \
  '{snapshot:$s, modelo:$m, nombre:$n, contrato:$c, imagen:$i, backend:$b, puerto:$p, vcpus:$v, mem_mib:$mm, cpu_pct:$cp,
    snapshot_mem_mib:$sm, snapshot_disk_mib:$sd, imagen_disco_mib:$id, ruta:$rp}')"

# ── el host ───────────────────────────────────────────────────────────────────
DROP_OK=0; DROP_MOTIVO=""
if [ "$DROP_CACHES" = 0 ]; then DROP_MOTIVO="DROP_CACHES=0"
elif [ "$LINUX" != 1 ]; then DROP_MOTIVO="sin /proc/sys/vm/drop_caches (no es Linux)"
elif [ ! -w /proc/sys/vm/drop_caches ]; then DROP_MOTIVO="/proc/sys/vm/drop_caches no se puede escribir (hace falta root fuera de un contenedor)"
else DROP_OK=1; fi
soltar_cache() { [ "$DROP_OK" = 1 ] && sync && echo 3 > /proc/sys/vm/drop_caches; }
meminfo() { # meminfo → {"cached_mib","available_mib"} del host
  if [ "$LINUX" = 1 ]; then
    awk '/^Cached:/{c=$2} /^MemAvailable:/{a=$2} END {printf "{\"cached_mib\":%d,\"available_mib\":%d}\n", c/1024, a/1024}' /proc/meminfo
  else
    "$KLING" top -json 2>/dev/null | jq -c '{cached_mib: null, available_mib: (.available_mib // null)}'
  fi
}
total_mib="null"; [ "$LINUX" = 1 ] && total_mib="$(awk '/^MemTotal:/{printf "%d", $2/1024}' /proc/meminfo)"
put host "$(jq -nc --arg u "$(uname -srm)" --arg f "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson t "$total_mib" \
  --argjson d "$DROP_OK" --arg dm "$DROP_MOTIVO" --argjson h "$HIGIENE" --arg sec "$SECCIONES" \
  --argjson ncpu "$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo null)" \
  '{uname:$u, fecha:$f, cpus:$ncpu, mem_total_mib:$t, drop_caches:($d==1), drop_caches_motivo:(if $dm=="" then null else $dm end),
    higiene:($h==1), secciones:($sec|split(" ")|map(select(.!="")))}')"
put esquema '"von-bench/2"'

ask() { # ask <machine> <max_tokens> <temperature> <prompt>
  "$KLING" ai model ask -json -max-tokens "$2" -temperature "$3" "$1" "$4"
}
# pedido <machine>: la petición del banco a una réplica; saca los ms de la petición.
pedido() {
  local a
  if [ "$VON" = 1 ]; then
    a="$(ask "$1" 1 0 "Hi")" || return 1
    jq -r .request_ms <<<"$a"
  else
    [ -n "$IA_BENCH" ] || return 1
    a="$("$IA_BENCH" guest ${TOK[@]+"${TOK[@]}"} -machine "$1" -port "$PUERTO" -path "$REQ_PATH" -body "@$BODY_FILE")" || return 1
    jq -r .ms <<<"$a"
  fi
}
# flt <pid> → {"minflt","majflt"} del proceso (Linux) o null.
flt() {
  if [ "$LINUX" = 1 ] && [ -r "/proc/$1/stat" ]; then
    sed 's/^.*) //' "/proc/$1/stat" | awk '{printf "{\"minflt\":%s,\"majflt\":%s}\n", $8, $10}'
  else echo null; fi
}
disco_leido() {
  local v
  v="$("$KLING" top -json 2>/dev/null | jq -r --arg n "$1" '[.machines[]? | select(.name==$n) | .disk_read_bytes // 0][0] // null')"
  echo "${v:-null}"
}

# ── 1. frío ───────────────────────────────────────────────────────────────────
if pide frio; then
  step "1. Cold boot to first response (kling run -image, load, first request)"
  m="$PREFIX-cold"
  t0=$(now_ms)
  args=(-name "$m" -image "$image" -cpus "$vcpus" -mem "$mem" -label "kling.ports=$PUERTO")
  [ "$VON" = 1 ] && args+=(-label "von.model=$model")
  [ "$cpupct" -gt 0 ] && args+=(-cpu-pct "$cpupct")
  if "$KLING" run "${args[@]}" >/dev/null; then
    t1=$(now_ms)
    if [ "$VON" = 1 ]; then
      out="$("$KLING" ai model ask -json -wait 30m -max-tokens 1 -temperature 0 "$m" "Hi")"; req="$(jq -r .request_ms <<<"$out")"
    elif [ -n "$IA_BENCH" ]; then
      out="$("$IA_BENCH" guest ${TOK[@]+"${TOK[@]}"} -wait 30m -machine "$m" -port "$PUERTO" -path "$REQ_PATH" -body "@$BODY_FILE")"; req="$(jq -r .ms <<<"$out")"
    else req=null; fi
    t2=$(now_ms)
    row "boot (kling run returns)" "$((t1 - t0)) ms"
    row "boot to first response" "$((t2 - t0)) ms"
    row "  of which the request itself" "$req ms"
    put frio "$(jq -nc --argjson a "$((t1 - t0))" --argjson b "$((t2 - t0))" --argjson r "${req:-null}" '{arranque_ms:$a, hasta_primera_ms:$b, peticion_ms:$r}')"
  else
    row "cold boot" "FAILED"; omit frio "falló kling run -image"
  fi
  "$KLING" rm -f "$m" >/dev/null 2>&1
else omit frio "no pedida en SECCIONES (COLD=1 la añade)"; fi

# ── 2. restaurar ──────────────────────────────────────────────────────────────
if pide restaurar; then
  step "2. Restore to first response (kling run -from $SNAP), $RUNS runs"
  thaw=(); first=(); req=()
  for i in $(seq "$RUNS"); do
    m="$PREFIX-t$i"
    t0=$(now_ms)
    out="$("$KLING" run -from "$SNAP" -name "$m" -json 2>"$D/err")" || { row "run $i" "FAILED: $(cat "$D/err")"; continue; }
    r="$(pedido "$m")" || { row "request $i" "FAILED"; "$KLING" rm -f "$m" >/dev/null 2>&1; continue; }
    t1=$(now_ms)
    thaw+=("$(jq -r '.thaw_ms // empty' <<<"$out")")
    first+=("$((t1 - t0))")
    req+=("$r")
    "$KLING" rm -f "$m" >/dev/null 2>&1
  done
  row "restore (daemon's thaw_ms), p50" "$(printf '%s\n' "${thaw[@]+"${thaw[@]}"}" | stats) ms"
  row "run -from to first response, p50" "$(printf '%s\n' "${first[@]+"${first[@]}"}" | stats) ms"
  row "  of which the request" "$(printf '%s\n' "${req[@]+"${req[@]}"}" | stats) ms"
  put restaurar "$(jq -nc --argjson t "$(arr ${thaw[@]+"${thaw[@]}"})" --argjson f "$(arr ${first[@]+"${first[@]}"})" \
    --argjson r "$(arr ${req[@]+"${req[@]}"})" '{thaw_ms:$t, hasta_primera_ms:$f, peticion_ms:$r}')"
else omit restaurar "no pedida en SECCIONES"; fi

# ── calentar (C9-14) ──────────────────────────────────────────────────────────
# Una muestra «caliente» solo lo es si su VMM no leyó nada del disco. Antes de cada una se trae el
# dorado a la caché por todas las vías que haya: la API del daemon (POST /snapshots/<n>/warm, Z1; un
# daemon sin ella contesta 404/501 y se sigue), los ficheros del dorado y de su imagen si KLING_ROOT es
# legible, y una restauración descartada con su petición (toca lo mismo que la muestra).
CAL_USADOS=""
usado() { case " $CAL_USADOS " in *" $1 "*) ;; *) CAL_USADOS="$CAL_USADOS $1" ;; esac; }
calentado_por() { echo "$CAL_USADOS"; }
# leer_a_cache <ruta>...: lee ficheros (o directorios enteros, siguiendo enlaces) a la caché de página.
leer_a_cache() {
  local f
  for f in "$@"; do
    [ -e "$f" ] || continue
    find -L "$f" -type f -exec cat {} + >/dev/null 2>&1
  done
  return 0
}
warm_api() { # warm_api: 0 si el daemon trajo el dorado a la caché (y esperó a tenerlo entero)
  local s c
  case ${KLING_HOST:-} in unix://*) s=${KLING_HOST#unix://} ;; *) return 1 ;; esac
  [ -S "$s" ] || return 1
  c="$(curl -s -o "$D/warm.json" -w '%{http_code}' --max-time 300 --unix-socket "$s" -H 'Content-Type: application/json' \
       -X POST --data '{"wait":true}' "http://kling/snapshots/$SNAP/warm" 2>/dev/null)"
  [ "$c" = 200 ]
}
calentar_dorado() {
  warm_api && usado api
  if [ -n "$KLING_ROOT" ] && [ -r "$KLING_ROOT/snapshots/$SNAP" ]; then
    leer_a_cache "$KLING_ROOT/snapshots/$SNAP" "$KLING_ROOT/images/$image".* && usado ficheros
  fi
  return 0
}
# calentar_maquina <nombre>: los ficheros de una máquina congelada (su volcado y su overlay).
calentar_maquina() {
  local id
  [ -n "$KLING_ROOT" ] || return 0
  id="$("$KLING" ps -a -json 2>/dev/null | jq -r --arg n "$1" '[.[]? | select(.name==$n) | .id][0] // empty')"
  [ -n "$id" ] && [ -r "$KLING_ROOT/machines/$id" ] && leer_a_cache "$KLING_ROOT/machines/$id" && usado congelada
  return 0
}
# caliente <restaurar_una|descongelar_una> <rc|dc> <nombre>: una muestra caliente de verdad. Si aun
# calentada su VMM leyó del disco más de UMBRAL_CALIENTE_BYTES, la muestra se aparta (p-<k>-disco, va
# al JSON en primera.calientes_con_disco) y se repite, hasta REINTENTOS_CALIENTE veces. Donde no se
# sabe lo leído (null: no es Linux) se queda como está.
caliente() {
  local f="$D/p-$2-intento.jsonl" n=0 dr
  while :; do
    : > "$f"
    CALENTAR=1 "$1" "$f" "$3-$n" 0 || return 1
    [ -s "$f" ] || return 1
    dr="$(jq -r '.disco_leido_bytes // "null"' "$f")"
    if [ "$dr" = null ] || [ "$dr" -le "$UMBRAL_CALIENTE_BYTES" ]; then cat "$f" >> "$D/p-$2.jsonl"; return 0; fi
    jq -c --argjson n "$n" '. + {intento:$n}' "$f" >> "$D/p-$2-disco.jsonl"
    n=$((n + 1))
    [ "$n" -le "$REINTENTOS_CALIENTE" ] || { echo "  $3: still reading the disk after $n warm-ups" >&2; return 1; }
  done
}

# ── 3. primera petición (P1) ──────────────────────────────────────────────────
# muestra <fichero> <máquina> <json de run/thaw> <t0>: hace la petición y apunta una muestra.
muestra() {
  local r t1 pid
  r="$(pedido "$2")" || { echo "  request on $2 FAILED" >&2; return 1; }
  t1=$(now_ms)
  pid="$(jq -r '.pid // 0' <<<"$3")"
  jq -nc --argjson tot "$((t1 - $4))" --argjson r "$r" --argjson w "$3" --argjson f "$(flt "$pid")" \
    --argjson dr "$(disco_leido "$2" || true)" \
    '{total_ms:$tot, peticion_ms:$r, thaw_ms:($w.thaw_ms // null), resync_ms:($w.wake.resync_ms // null),
      load_ms:($w.wake.load_ms // null), fallos:$f, disco_leido_bytes:$dr}' >> "$1"
}
restaurar_una() { # restaurar_una <fichero> <nombre> <fría 0|1>; con CALENTAR=1, ver caliente
  local out t0
  if [ "${CALENTAR:-0}" = 1 ]; then
    calentar_dorado
    # La restauración descartada toca lo mismo que la muestra, también lo que no es del dorado.
    if "$KLING" run -from "$SNAP" -name "$2-cal" >/dev/null 2>&1 && pedido "$2-cal" >/dev/null; then usado descartada; fi
    "$KLING" rm -f "$2-cal" >/dev/null 2>&1
  fi
  [ "$3" = 1 ] && soltar_cache
  t0=$(now_ms)
  out="$("$KLING" run -from "$SNAP" -name "$2" -json 2>/dev/null)" || { echo "  run -from $2 FAILED" >&2; return 1; }
  muestra "$1" "$2" "$out" "$t0"
  "$KLING" rm -f "$2" >/dev/null 2>&1
}
descongelar_una() { # descongelar_una <fichero> <nombre> <fría 0|1>
  local out t0
  if ! { "$KLING" run -from "$SNAP" -name "$2" -json >/dev/null 2>&1 && pedido "$2" >/dev/null && "$KLING" freeze -json "$2" >/dev/null 2>&1; }; then
    echo "  preparing frozen $2 FAILED" >&2; "$KLING" rm -f "$2" >/dev/null 2>&1; return 1
  fi
  if [ "${CALENTAR:-0}" = 1 ]; then calentar_dorado; calentar_maquina "$2"; fi
  [ "$3" = 1 ] && soltar_cache
  t0=$(now_ms)
  out="$("$KLING" thaw -json "$2" 2>/dev/null)" || { echo "  thaw $2 FAILED" >&2; "$KLING" rm -f "$2" >/dev/null 2>&1; return 1; }
  muestra "$1" "$2" "$out" "$t0"
  "$KLING" rm -f "$2" >/dev/null 2>&1
}
if pide primera; then
  step "3. First response after restore and after thaw, warm page cache and after drop_caches ($RUNS runs)"
  [ "$DROP_OK" = 1 ] || row "cold page cache" "skipped: $DROP_MOTIVO"
  # C9-14. Calientes primero y todas juntas; las frías (cada una tras drop_caches) después: en la
  # ronda 8 el orden mezclado dejaba la mitad de las «calientes» detrás de un drop_caches.
  restaurar_una "$D/descartada" "$PREFIX-p0" 0 >/dev/null 2>&1
  for i in $(seq "$RUNS"); do
    # A/B alternado: las vueltas impares restauran primero; las pares descongelan primero.
    if [ $((i % 2)) = 1 ]; then orden="r d"; else orden="d r"; fi
    for k in $orden; do
      if [ "$k" = r ]; then caliente restaurar_una rc "$PREFIX-p$i-rc"
      else caliente descongelar_una dc "$PREFIX-p$i-dc"; fi
    done
  done
  if [ "$DROP_OK" = 1 ]; then
    for i in $(seq "$RUNS"); do
      if [ $((i % 2)) = 1 ]; then orden="r d"; else orden="d r"; fi
      for k in $orden; do
        if [ "$k" = r ]; then restaurar_una "$D/p-rf.jsonl" "$PREFIX-p$i-rf" 1
        else descongelar_una "$D/p-df.jsonl" "$PREFIX-p$i-df" 1; fi
      done
    done
  fi
  for k in rc:restore,warm rf:restore,cold dc:thaw,warm df:thaw,cold; do
    f="$D/p-${k%%:*}.jsonl"
    [ -s "$f" ] && row "${k#*:} to first response, p50" "$(jq -r .total_ms "$f" | stats) ms (request $(jq -r .peticion_ms "$f" | stats))"
  done
  for k in rc dc; do
    [ -s "$D/p-$k-disco.jsonl" ] && row "  warm samples set aside ($k: read the disk)" "$(wc -l < "$D/p-$k-disco.jsonl" | tr -d ' ')"
  done
  fria() { if [ "$DROP_OK" = 1 ]; then jsonl "$1"; else jq -nc --arg r "$DROP_MOTIVO" '{omitida:$r}'; fi; }
  put primera "$(jq -nc --argjson rc "$(jsonl "$D/p-rc.jsonl")" --argjson rf "$(fria "$D/p-rf.jsonl")" \
    --argjson dc "$(jsonl "$D/p-dc.jsonl")" --argjson df "$(fria "$D/p-df.jsonl")" \
    --argjson arc "$(jsonl "$D/p-rc-disco.jsonl")" --argjson adc "$(jsonl "$D/p-dc-disco.jsonl")" \
    --argjson u "$UMBRAL_CALIENTE_BYTES" --argjson rep "$REINTENTOS_CALIENTE" --arg cal "$(calentado_por)" \
    '{restaurar_caliente:$rc, restaurar_fria:$rf, descongelar_caliente:$dc, descongelar_fria:$df,
      calientes_con_disco:{restaurar_caliente:$arc, descongelar_caliente:$adc},
      umbral_caliente_bytes:$u, reintentos_caliente:$rep, calentado_por:($cal|split(" ")|map(select(.!="")))}')"
else omit primera "no pedida en SECCIONES"; fi

# ── 4. tokens/s ───────────────────────────────────────────────────────────────
if pide tps && [ "$VON" = 1 ]; then
  step "4. Throughput on a warm replica (llama-server timings)"
  m="$PREFIX-tp"
  "$KLING" run -from "$SNAP" -name "$m" >/dev/null
  ask "$m" 8 0 "Hello" >/dev/null   # la primera petición tras restaurar paga los fallos de página
  prompt="Explain in detail how a virtual machine differs from a container. Cover the kernel, isolation, startup time, memory use, security boundaries, and typical use cases for each, and finish with a short recommendation for running untrusted code from the internet safely on a shared server."
  pp=(); tg=(); pn=0; gn=0
  for i in 1 2 3; do
    # El número va DELANTE: llama-server reutiliza el prefijo común con la
    # petición anterior (cache_prompt) y solo evaluaría lo que cambia al final.
    a="$(ask "$m" 128 0 "Request $i. $prompt")" || continue
    pp+=("$(jq -r '.response.timings.prompt_per_second|floor' <<<"$a")")
    tg+=("$(jq -r '.response.timings.predicted_per_second*10|floor/10' <<<"$a")")
    pn="$(jq -r .response.timings.prompt_n <<<"$a")"; gn="$(jq -r .response.timings.predicted_n <<<"$a")"
  done
  row "prompt eval ($pn tokens), tok/s" "$(printf '%s\n' "${pp[@]+"${pp[@]}"}" | stats)"
  row "generation ($gn tokens), tok/s" "$(printf '%s\n' "${tg[@]+"${tg[@]}"}" | stats)"
  put tps "$(jq -nc --argjson p "$(arr ${pp[@]+"${pp[@]}"})" --argjson g "$(arr ${tg[@]+"${tg[@]}"})" --argjson pn "${pn:-0}" --argjson gn "${gn:-0}" \
    '{prompt_tok_s:$p, generacion_tok_s:$g, prompt_n:$pn, generados_n:$gn}')"
  "$KLING" rm -f "$m" >/dev/null 2>&1
elif pide tps; then omit tps "solo para el contrato openai de un modelo VON"
else omit tps "no pedida en SECCIONES"; fi

# ── gateway del banco ────────────────────────────────────────────────────────
# gw_up <etq> <idle> <max_replicas> [json extra]: un `kling ai serve` propio en un socket del
# directorio temporal, con su id y su prefijo, para no tocar el gateway de nadie. El extra se
# mezcla en el bloque del modelo (spare, pin: ronda 9).
gw_up() {
  local g="$D/gw-$1" bloque x="${4:-}"
  [ -n "$x" ] || x='{}'
  mkdir -p "$g"
  if [ -n "${GW_BLOQUE_FILE:-}" ]; then
    bloque="$(jq -c --arg s "$SNAP" --arg i "$2" --argjson r "$3" '. + {snapshot:$s, idle:$i, max_replicas:$r}' "$GW_BLOQUE_FILE")" || return 1
  elif [ "$VON" = 1 ]; then
    bloque="$(jq -nc --arg s "$SNAP" --arg i "$2" --argjson r "$3" '{kind:"von", snapshot:$s, idle:$i, max_replicas:$r}')"
  else
    bloque="$(jq -nc --arg s "$SNAP" --arg i "$2" --argjson r "$3" --argjson p "$PUERTO" --arg c "$CONTRATO" --arg h "$HEALTH_PATH" \
      --argjson mi "${MAX_INFLIGHT:-1}" --arg tf "$TOKEN_FILE" \
      '{kind:"http", snapshot:$s, port:$p, contract:$c, health:{path:$h}, idle:$i, max_replicas:$r, max_inflight:$mi}
       + (if $tf == "" then {} else {auth:{header:"Authorization", scheme:"Bearer", file:$tf}} end)')"
  fi
  jq -n --arg n "$MODELO" --argjson b "$bloque" --argjson x "$x" '{models: {($n): ($b + $x)}}' > "$g/ai.json"
  "$KLING" ai serve -config "$g/ai.json" -socket "$g/ai.sock" -id "vb$$$1" -name-prefix "$PREFIX-g$1-" \
    -idle "${IDLE_S}s" -paused-mib 0 -max-inflight 1 -max-replicas "$3" > "$g/log" 2>&1 &
  echo $! > "$g/pid"
  GWS="$GWS $1"
  for _ in $(seq 1 60); do
    curl -fsS --unix-socket "$g/ai.sock" http://gw/healthz >/dev/null 2>&1 && return 0
    kill -0 "$(cat "$g/pid")" 2>/dev/null || break
    sleep 0.5
  done
  echo "  gateway $1 did not come up: $(tail -3 "$g/log" | tr '\n' ' ')" >&2
  return 1
}
# gw_ruta <etq>: la ruta por la que el gateway sirve el modelo: /v1/m/<modelo>… si
# lo sabe (ronda 8), si no la de siempre con "model" en el cuerpo (solo openai).
gw_ruta() {
  local g="$D/gw-$1" c
  c="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "$g/ai.sock" -H 'Content-Type: application/json' \
       --data-binary "@$BODY_FILE" "http://gw/v1/m/$MODELO$REQ_PATH")"
  if [ "$c" != 404 ] || [ "$CONTRATO" != openai ]; then
    cp "$BODY_FILE" "$g/cuerpo.json"; echo "/v1/m/$MODELO$REQ_PATH"; return
  fi
  jq -c --arg m "$MODELO" '.model=$m' "$BODY_FILE" > "$g/cuerpo.json"; echo "$REQ_PATH"
}
# gw_pedir <etq> <clientes>: tantas peticiones a la vez como clientes (una cada uno).
gw_pedir() {
  local g="$D/gw-$1"
  [ -f "$g/ruta" ] || gw_ruta "$1" > "$g/ruta"
  "$IA_BENCH" carga -socket "$g/ai.sock" -url "http://gw$(cat "$g/ruta")" -body "@$g/cuerpo.json" \
    -clientes "$2" -n "$2" -calentar 0 -timeout 10m
}
# dormidas <prefijo>: espera a que el segador duerma (congele o borre) todas las
# réplicas; saca {"estado": "borradas"|"congeladas"|"mixto"|"despiertas", "disco_mib": …}.
dormidas() {
  local ps vivas plazo=$((IDLE_S * 4 + 30)) t0
  t0=$(now_ms)
  while :; do
    ps="$("$KLING" ps -a -json 2>/dev/null | jq -c --arg p "$1" '[.[]? | select(.name|startswith($p))]')"
    vivas="$(jq '[.[] | select(.state=="running" or .state=="paused" or .state=="created")] | length' <<<"$ps")"
    [ "$vivas" = 0 ] && break
    [ $(( $(now_ms) - t0 )) -gt $((plazo * 1000)) ] && break
    sleep 0.5
  done
  jq -c --argjson ms "$(( $(now_ms) - t0 ))" '{estado: (if length==0 then "borradas"
      elif all(.state=="frozen") then "congeladas" elif any(.state=="running" or .state=="paused") then "despiertas" else "mixto" end),
    replicas: length, disco_mib: ([.[].disk_bytes // 0] | add // 0 | ./1048576 | floor), espera_ms: $ms}' <<<"$ps"
}

# ── 5. memoria de N réplicas (P2) ────────────────────────────────────────────
# punto <prefijo> <n> [extra json]: PSS de kling top, RSS de /proc y la memoria del host.
punto() {
  local top rss=null pid k x="${3:-}"
  [ -n "$x" ] || x='{}'
  top="$("$KLING" top -json)"
  if [ "$LINUX" = 1 ]; then
    rss=0
    for pid in $(jq -r --arg p "$1" '.machines[]?|select(.name|startswith($p))|.pid' <<<"$top"); do
      k=$(awk '/^VmRSS/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
      rss=$((rss + ${k:-0} / 1024))
    done
  fi
  jq -c --arg p "$1" --argjson n "$2" --argjson rss "$rss" --argjson h "$(meminfo)" --argjson x "$x" \
    '[.machines[]?|select(.name|startswith($p))] as $m |
     {replicas:$n, vivas:($m|length), pss_total_mib:([$m[].pss_mib]|add // 0), pss_mib:[$m[].pss_mib], rss_total_mib:$rss, host:$h} + $x' <<<"$top"
}
fila_mem() { jq -r '"  \(.replicas)          \(.pss_total_mib)              \(.pss_mib|map(tostring)|join(" "))      rss \(.rss_total_mib)  cached \(.host.cached_mib)"' <<<"$1"; }
mem_restauradas() { # mem_restauradas <ronda>
  local p="$PREFIX-mr$1-" n pt
  for n in $(seq "$REPLICAS"); do
    "$KLING" run -from "$SNAP" -name "$p$n" >/dev/null || { echo "  replica $n FAILED"; break; }
    pedido "$p$n" >/dev/null
    pt="$(punto "$p" "$n" "{\"ronda\":$1}")"; fila_mem "$pt"; echo "$pt" >> "$D/m-restauradas.jsonl"
  done
}
mem_descongeladas() { # congeladas y vueltas a despertar: lo que hace el segador con kling freeze/thaw
  local p="$PREFIX-md$1-" n pt fz disco
  for n in $(seq "$REPLICAS"); do
    "$KLING" run -from "$SNAP" -name "$p$n" >/dev/null || { echo "  replica $n FAILED"; break; }
    pedido "$p$n" >/dev/null
    fz="$("$KLING" freeze -json "$p$n")" || { echo "  freeze $n FAILED"; break; }
    disco="$("$KLING" ps -a -json | jq -r --arg n "$p$n" '[.[]|select(.name==$n)|(.disk_bytes // 0)/1048576|floor][0] // null')"
    "$KLING" thaw "$p$n" >/dev/null || { echo "  thaw $n FAILED"; break; }
    pedido "$p$n" >/dev/null
    pt="$(punto "$p" "$n" "$(jq -nc --argjson r "$1" --argjson d "$disco" --argjson f "$fz" '{ronda:$r, disco_dormida_mib:$d, freeze_ms:($f.freeze_ms // null)}')")"
    fila_mem "$pt"; echo "$pt" >> "$D/m-descongeladas.jsonl"
  done
}
mem_remove() { # por el gateway con idle: remove: despertar n, dejar que el segador las borre, despertar n
  local etq="r$1" n pt dorm
  gw_up "$etq" remove "$REPLICAS" || return 1
  for n in $(seq "$REPLICAS"); do
    gw_pedir "$etq" "$n" >/dev/null || { echo "  wake $n FAILED"; break; }
    dorm="$(dormidas "$PREFIX-g$etq-")"
    gw_pedir "$etq" "$n" >/dev/null || { echo "  re-wake $n FAILED"; break; }
    pt="$(punto "$PREFIX-g$etq-" "$n" "$(jq -nc --argjson r "$1" --argjson d "$dorm" '{ronda:$r, dormidas:$d, disco_dormida_mib:$d.disco_mib}')")"
    fila_mem "$pt"; echo "$pt" >> "$D/m-remove.jsonl"
  done
  gw_down "$etq"
}
if pide memoria; then
  step "5. Memory of 1..$REPLICAS replicas: restored, thawed after a freeze, and idle: remove ($MEM_RONDAS round(s))"
  printf "  %-10s %-16s %-18s %s\n" "replicas" "total PSS MiB" "PSS per replica" "sum of RSS MiB (Linux), host Cached MiB"
  variantes="restauradas descongeladas remove"
  for ronda in $(seq "$MEM_RONDAS"); do
    for v in $variantes; do
      if [ "$v" = remove ] && { [ "$GATEWAY" != 1 ] || [ -z "$IA_BENCH" ]; }; then continue; fi
      echo "  -- $v (round $ronda)"
      higiene
      "mem_$v" "$ronda"
      higiene
    done
    # El orden rota en cada ronda (A/B alternado): nadie va siempre primero.
    variantes="${variantes#* } ${variantes%% *}"
  done
  rem="$(jsonl "$D/m-remove.jsonl")"
  if [ "$GATEWAY" != 1 ]; then rem='{"omitida":"GATEWAY=0"}'
  elif [ -z "$IA_BENCH" ]; then rem='{"omitida":"sin ia-bench (IA_BENCH)"}'; fi
  put memoria "$(jq -nc --argjson a "$(jsonl "$D/m-restauradas.jsonl")" --argjson b "$(jsonl "$D/m-descongeladas.jsonl")" \
    --argjson c "$rem" '{restauradas:$a, descongeladas:$b, remove:$c}')"
else omit memoria "no pedida en SECCIONES"; fi

# ── 6. ociosa → primera respuesta (P2) ───────────────────────────────────────
if pide ociosa; then
  step "6. Idle to first response, $CYCLES cycles: thaw a frozen replica vs restore a new one"
  f="$PREFIX-of"; dz=(); rs=()
  if "$KLING" run -from "$SNAP" -name "$f" >/dev/null && pedido "$f" >/dev/null && "$KLING" freeze "$f" >/dev/null; then
    for i in $(seq "$CYCLES"); do
      if [ $((i % 2)) = 1 ]; then orden="d r"; else orden="r d"; fi
      for k in $orden; do
        if [ "$k" = d ]; then
          t0=$(now_ms); "$KLING" thaw "$f" >/dev/null && pedido "$f" >/dev/null && dz+=("$(( $(now_ms) - t0 ))")
          "$KLING" freeze "$f" >/dev/null
        else
          m="$PREFIX-or$i"; t0=$(now_ms)
          "$KLING" run -from "$SNAP" -name "$m" >/dev/null && pedido "$m" >/dev/null && rs+=("$(( $(now_ms) - t0 ))")
          "$KLING" rm -f "$m" >/dev/null 2>&1
        fi
      done
    done
  else row "frozen replica" "FAILED"; fi
  "$KLING" rm -f "$f" >/dev/null 2>&1
  row "thaw + request, p50" "$(printf '%s\n' "${dz[@]+"${dz[@]}"}" | stats) ms"
  row "restore + request, p50" "$(printf '%s\n' "${rs[@]+"${rs[@]}"}" | stats) ms"
  # Lo mismo por el gateway: una réplica que el segador congela frente a una que borra.
  gwj='{"omitida":"GATEWAY=0"}'
  [ "$GATEWAY" = 1 ] && [ -z "$IA_BENCH" ] && gwj='{"omitida":"sin ia-bench (IA_BENCH)"}'
  if [ "$GATEWAY" = 1 ] && [ -n "$IA_BENCH" ]; then
    if gw_up of freeze 1 && gw_up or remove 1; then
      # «Con repuesto» (C9-08, Z4): idle remove con spare 1, una réplica restaurada y ya tocada
      # esperando. Un daemon sin Z4 ignora spare y la fila sale igual que idle remove.
      orden="of or"; repj='{"omitida":"el gateway con spare: 1 no arrancó"}'
      gw_up os remove 1 '{"spare":1}' && orden="of or os"
      for g in $orden; do gw_pedir "$g" 1 >/dev/null; done
      for i in $(seq "$CYCLES"); do
        for g in $orden; do
          if [ "$g" = os ]; then
            # El repuesto sigue vivo: no se espera a «todas dormidas», sino lo que tarda el segador.
            sleep $((IDLE_S * 2 + 5)); dorm="$(jq -nc --argjson v "$(nuestras "$PREFIX-gos-" | wc -l)" '{vivas:$v}')"
          else dorm="$(dormidas "$PREFIX-g$g-")"; fi
          gw_pedir "$g" 1 | jq -c --argjson d "$dorm" '{ms: .puntos[0].p50_ms, errores: .puntos[0].errores, dormidas: $d}' >> "$D/o-$g.jsonl"
        done
        # A/B rotado: nadie va siempre primero.
        orden="${orden#* } ${orden%% *}"
      done
      [ -s "$D/o-os.jsonl" ] && repj="$(jsonl "$D/o-os.jsonl")"
      gwj="$(jq -nc --argjson a "$(jsonl "$D/o-of.jsonl")" --argjson b "$(jsonl "$D/o-or.jsonl")" --argjson c "$repj" \
        '{idle_freeze:$a, idle_remove:$b, idle_remove_repuesto:$c}')"
      row "gateway, idle freeze, p50" "$(jq -r .ms "$D/o-of.jsonl" 2>/dev/null | stats) ms"
      row "gateway, idle remove, p50" "$(jq -r .ms "$D/o-or.jsonl" 2>/dev/null | stats) ms"
      row "gateway, idle remove + spare 1, p50" "$(jq -r .ms "$D/o-os.jsonl" 2>/dev/null | stats) ms"
    else gwj='{"omitida":"el gateway del banco no arrancó (¿ai.json sin las claves de la ronda 8?)"}'; fi
    gw_down of; gw_down or; gw_down os
  fi
  put ociosa "$(jq -nc --argjson d "$(arr ${dz[@]+"${dz[@]}"})" --argjson r "$(arr ${rs[@]+"${rs[@]}"})" --argjson g "$gwj" \
    '{descongelar_ms:$d, restaurar_ms:$r, gateway:$g}')"
else omit ociosa "no pedida en SECCIONES"; fi

# ── 6b. primera petición tras arrancar el gateway (C9-10) ─────────────────────
# Con la caché fría (drop_caches) se arranca un gateway del banco y se pide: en cuanto contesta
# (inmediata), tras ARRANQUE_ESPERA_S (espera: lo que gana precalentar al arrancar, Z4) y con pin: true
# y un drop_caches DESPUÉS de la espera (anclado: el dorado sigue en RAM). Sin Z1/Z4 las tres salen frías.
ARRANQUE_RUNS="${ARRANQUE_RUNS:-3}"
ARRANQUE_ESPERA_S="${ARRANQUE_ESPERA_S:-20}"
# disco_prefijo <prefijo>: bytes leídos del disco por los VMM de las máquinas con ese prefijo.
disco_prefijo() {
  "$KLING" top -json 2>/dev/null | jq -r --arg p "$1" '[.machines[]? | select(.name|startswith($p)) | .disk_read_bytes // 0] | if length == 0 then null else add end'
}
arranque_una() { # arranque_una <variante> <vuelta>
  local etq="a$1$2" x='{}' t0 out
  [ "$1" = anclado ] && x='{"pin":true}'
  [ "$1" = anclado ] || soltar_cache
  t0=$(now_ms)
  gw_up "$etq" remove 1 "$x" || return 1
  case $1 in espera|anclado) sleep "$ARRANQUE_ESPERA_S" ;; esac
  [ "$1" = anclado ] && soltar_cache
  out="$(gw_pedir "$etq" 1)" || { gw_down "$etq"; return 1; }
  jq -c --arg v "$1" --argjson t "$(( $(now_ms) - t0 ))" --argjson dr "$(disco_prefijo "$PREFIX-g$etq-")" \
    '{variante:$v, ms:.puntos[0].p50_ms, errores:.puntos[0].errores, desde_arranque_ms:$t, disco_leido_bytes:$dr}' <<<"$out" >> "$D/a-$1.jsonl"
  gw_down "$etq"
}
if pide arranque && [ "$GATEWAY" = 1 ] && [ -n "$IA_BENCH" ] && [ "$DROP_OK" = 1 ]; then
  step "6b. First request after starting the gateway with a cold page cache ($ARRANQUE_RUNS runs, wait ${ARRANQUE_ESPERA_S} s)"
  orden="inmediata espera anclado"
  for i in $(seq "$ARRANQUE_RUNS"); do
    for v in $orden; do higiene; arranque_una "$v" "$i" || echo "  start $v $i FAILED" >&2; done
    orden="${orden#* } ${orden%% *}"
  done
  for v in inmediata espera anclado; do
    row "gateway start → first response, $v, p50" "$(jq -r .ms "$D/a-$v.jsonl" 2>/dev/null | stats) ms"
  done
  put arranque "$(jq -nc --argjson a "$(jsonl "$D/a-inmediata.jsonl")" --argjson b "$(jsonl "$D/a-espera.jsonl")" \
    --argjson c "$(jsonl "$D/a-anclado.jsonl")" --argjson e "$ARRANQUE_ESPERA_S" '{inmediata:$a, espera:$b, anclado:$c, espera_s:$e}')"
elif pide arranque && [ "$GATEWAY" != 1 ]; then omit arranque "GATEWAY=0"
elif pide arranque && [ -z "$IA_BENCH" ]; then omit arranque "sin ia-bench (IA_BENCH)"
elif pide arranque; then omit arranque "hace falta drop_caches: $DROP_MOTIVO"
else omit arranque "no pedida en SECCIONES"; fi

# ── 7. semillas ──────────────────────────────────────────────────────────────
if pide semillas && [ "$VON" = 1 ]; then
  step "7. Sampling seeds across replicas (same request, temperature 1)"
  q="Invent a name for a new planet. Answer with the name only."
  "$KLING" run -from "$SNAP" -name "$PREFIX-s1" >/dev/null && "$KLING" run -from "$SNAP" -name "$PREFIX-s2" >/dev/null
  a1="$(ask "$PREFIX-s1" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  a2="$(ask "$PREFIX-s2" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  a3="$(ask "$PREFIX-s1" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  row "replica 1" "${a1//$'\n'/ }"
  row "replica 2" "${a2//$'\n'/ }"
  row "replica 1 again" "${a3//$'\n'/ }"
  if [ "$a1" = "$a2" ]; then row "verdict" "SAME output on two replicas (check the seed)"; else row "verdict" "different outputs: replicas do not share a seed"; fi
  put semillas "$(jq -nc --arg a "$a1" --arg b "$a2" --arg c "$a3" '{replica1:$a, replica2:$b, replica1_otra:$c, iguales:($a==$b)}')"
  "$KLING" rm -f "$PREFIX-s1" "$PREFIX-s2" >/dev/null 2>&1
elif pide semillas; then omit semillas "solo para el contrato openai de un modelo VON"
else omit semillas "no pedida en SECCIONES"; fi

# ── 8. carga por el gateway (P3) ─────────────────────────────────────────────
if pide carga && [ "$GATEWAY" = 1 ] && [ -n "$IA_BENCH" ]; then
  maxc=1; for c in $(echo "$CLIENTES" | tr ',' ' '); do [ "$c" -gt "$maxc" ] && maxc=$c; done
  rep=${CARGA_REPLICAS:-$REPLICAS}; [ "$rep" -lt 1 ] && rep=1
  step "8. Load through the gateway: $CLIENTES clients, ${CARGA_S} s each, up to $rep replicas"
  if gw_up c remove "$rep"; then
    ruta="$(gw_ruta c)"; echo "$ruta" > "$D/gw-c/ruta"
    gwout="$("$IA_BENCH" carga -socket "$D/gw-c/ai.sock" -url "http://gw$ruta" -body "@$D/gw-c/cuerpo.json" \
      -clientes "$CLIENTES" -duracion "${CARGA_S}s" -calentar "$rep")" || gwout='{"error":"falló ia-bench carga"}'
    jq -r '.puntos[]? | "  \(.clientes) clients: \(.rps|floor) req/s, p50 \(.p50_ms) ms, p99 \(.p99_ms) ms, errors \(.errores)"' <<<"$gwout"
    dir='null'
    if [ -n "${LOAD_DIRECT_URL:-}" ]; then
      dir="$("$IA_BENCH" carga ${TOK[@]+"${TOK[@]}"} -url "$LOAD_DIRECT_URL" -body "@$BODY_FILE" -clientes "$CLIENTES" -duracion "${CARGA_S}s")" || dir='{"error":"falló ia-bench carga directa"}'
      jq -r '.puntos[]? | "  direct, \(.clientes) clients: \(.rps|floor) req/s, p50 \(.p50_ms) ms, p99 \(.p99_ms) ms"' <<<"$dir"
    fi
    put carga "$(jq -nc --arg r "$ruta" --argjson g "$gwout" --argjson d "$dir" --argjson h "$(punto "$PREFIX-gc-" 0)" \
      '{ruta:$r, gateway:$g, directo:$d, memoria_al_final:$h}')"
  else
    omit carga "el gateway del banco no arrancó (¿ai.json sin las claves de la ronda 8?)"
  fi
  gw_down c
elif pide carga && [ "$GATEWAY" != 1 ]; then omit carga "GATEWAY=0"
elif pide carga; then omit carga "sin ia-bench (IA_BENCH)"
else omit carga "no pedida en SECCIONES"; fi

jq -s 'add' "$D"/j-*.json > "$JSON_OUT" || { echo "could not write $JSON_OUT" >&2; exit 1; }
step "JSON: $JSON_OUT"
