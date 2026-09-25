#!/usr/bin/env bash
# Mide las palancas de CPU de VON (docs/von-cpu.md): cada una con su métrica, y
# con una medida de calidad al lado, porque una palanca solo entra en los
# valores por defecto si mejora lo medido SIN empeorar las respuestas.
#
# La tarea de referencia es la de scripts/von-bench/: un asistente de domótica
# con un prompt de sistema de ~790 tokens (lista de dispositivos, esquema JSON y
# ejemplos) y 21 peticiones con su respuesta esperada. Es el caso realista de un
# prefijo largo y fijo por tarea y una pregunta corta.
#
#   ./97-von-cpu-bench.sh restart <máquina> [args de llama-server...]
#       relanza llama-server en la máquina con los argumentos de su imagen MÁS
#       estos (los últimos mandan en llama.cpp). Sin argumentos, los de la imagen.
#       Necesita una máquina con -allow-exec (las de `kling models add` lo son).
#   ./97-von-cpu-bench.sh ttft <máquina>
#       primer token con el prefijo sin cachear y con el prefijo ya en la caché
#       de la ranura (lo que ahorra precalcularlo).
#   ./97-von-cpu-bench.sh prefix <dorado>...
#       la métrica de la palanca 1: primer token de la PRIMERA petición de la
#       tarea en una réplica recién restaurada de cada dorado (RUNS veces).
#       SLOT_FILE=<fichero> restaura antes esa ranura (POST /slots/0?action=restore).
#   ./97-von-cpu-bench.sh switch <máquina>
#       dos tareas alternas en la misma réplica (domótica y tickets): lo que
#       cuesta cambiar de tarea, que es donde se nota una caché de prefijos.
#   ./97-von-cpu-bench.sh gen <máquina>
#       generación: tok/s, aceptación del borrador (si lo hay) y acierto, a
#       temperatura 0 y 0,7, con respuestas JSON cortas y con un párrafo.
#   ./97-von-cpu-bench.sh schema <máquina>
#       JSON válido y acierto con y sin json_schema, y lo que cuesta.
#   ./97-von-cpu-bench.sh quality <máquina>
#       solo el acierto de la tarea (temperatura 0, con el esquema).
#
# Variables: KLING (kling), RUNS (5), DATA (scripts/von-bench), N (peticiones
# de la tarea que se usan; todas por defecto), TEMPS (las de gen: "0 0.7").
#
# Lo que se mide va directo a la réplica (su dirección: el reenvío de kling-vz
# en macOS, la IP en Linux), sin el proxy del daemon, y los tiempos de
# llama-server (`timings`) se leen de la respuesta. Por eso se corre en el host
# del daemon (KLING_HOST o el contexto de kling apuntan al daemon; la IP de una
# microVM de Linux solo se alcanza desde su host).

set -o pipefail

KLING="${KLING:-kling}"
RUNS="${RUNS:-5}"
DATA="${DATA:-$(cd "$(dirname "$0")" && pwd)/von-bench}"
SYSTEM_FILE="$DATA/smarthome-system.txt"
REQS="$DATA/smarthome-requests.jsonl"
SCHEMA="$DATA/smarthome-schema.json"
SYSTEM2_FILE="$DATA/tickets-system.txt"
REQS2="$DATA/tickets-requests.txt"
PARAGRAPH="Write one paragraph of about 120 words explaining what a microVM is and why it starts faster than a regular virtual machine."

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need jq; need curl; need perl

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
step()   { printf "\n\033[1m%s\033[0m\n" "$1"; }
row()    { printf "  %-46s %s\n" "$1" "$2"; }
# stats lee números por stdin y saca "p50 (mín–máx, n)".
stats()  { grep -E '^[0-9]' | sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "n/a"; exit} m=a[int((NR+1)/2)]; printf "%s (%s–%s, n=%d)\n", m, a[1], a[NR], NR}'; }

addr_of() {
  "$KLING" inspect "$1" | jq -r 'if (.forwards["8000"] // "") != "" then .forwards["8000"] else .ip + ":8000" end'
}

requests() { if [ -n "${N:-}" ]; then head -n "$N" "$REQS"; else cat "$REQS"; fi; }

# chat <addr> <cuerpo JSON>: imprime la respuesta en una línea y, en la
# siguiente, los milisegundos de punta a punta.
chat() {
  curl -s --max-time 600 -H 'Content-Type: application/json' -w '\n%{time_total}\n' \
    "http://$1/v1/chat/completions" -d "$2" | awk 'NR==1{print; next} {printf "%d\n", $1*1000}'
}

# body <usuario> <max_tokens> <temperatura> [esquema]: la petición de la tarea.
body() {
  local extra='{}'
  [ -n "${4:-}" ] && extra="$(jq -c '{json_schema: .}' "$4")"
  jq -nc --rawfile s "$SYSTEM_FILE" --arg u "$1" --argjson mt "$2" --argjson t "$3" --argjson x "$extra" \
    '{messages:[{role:"system",content:$s},{role:"user",content:$u}],max_tokens:$mt,temperature:$t,seed:42} + $x'
}

# score <respuesta> <esperado>: 1 si las acciones son exactamente las esperadas
# (el orden no cuenta; los valores se comparan como texto en minúsculas).
score() {
  jq -nr --arg out "$1" --argjson exp "$2" '
    def norm: map({d: .device, a: .action, v: (.value | if . == null then "null" else tostring | ascii_downcase end)}) | sort_by(.d, .a, .v);
    ($out | try fromjson catch null) as $o
    | if ($o | type) == "object" and ($o.actions | type) == "array"
         and ([$o.actions[] | type == "object"] | all)
      then (if ($o.actions | norm) == ($exp | norm) then 1 else 0 end) else 0 end'
}

# valid <respuesta>: 1 si es JSON y cumple el esquema en lo que importa
# (dispositivos y acciones de la lista, reply texto).
valid() {
  jq -nr --arg out "$1" --slurpfile sc "$SCHEMA" '
    ($sc[0].properties.actions.items.properties) as $p
    | ($out | try fromjson catch null) as $o
    | if ($o | type) == "object" and ($o.reply | type) == "string" and ($o.actions | type) == "array"
         and ([$o.actions[] | (type == "object") and (.device as $d | $p.device.enum | index([$d]) != null)
               and (.action as $a | $p.action.enum | index([$a]) != null) and has("value")] | all)
      then 1 else 0 end'
}

cmd_restart() {
  local m="${1:?usage: restart <machine> [llama-server args...]}"; shift
  # El script de arranque de la imagen se guarda la primera vez; el nuevo es
  # ese mismo con los argumentos añadidos. El entrypoint relanza llama-server
  # al segundo de matarlo.
  local before; before="$("$KLING" exec "$m" -- /bin/sh -c 'wc -l < /var/log/service.log' | tr -d ' \r')"
  "$KLING" exec "$m" -- /bin/sh -c '
    o=/etc/von/run.sh.orig; [ -f "$o" ] || cp /etc/von/run.sh "$o"
    { grep -v "^exec " "$o"; printf "%s" "$(grep "^exec " "$o")"; for a; do printf " '\''%s'\''" "$a"; done; echo; } > /etc/von/run.sh
    for p in /proc/[0-9]*; do [ "$(readlink "$p/exe" 2>/dev/null)" = /opt/llama.cpp/llama-server ] && kill "${p#/proc/}"; done
    exit 0' sh "$@" || return 1
  local a t0; a="$(addr_of "$m")"; t0=$(now_ms)
  sleep 1.5
  until [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$a/health")" = 200 ]; do
    # Si el servidor sale con error (un argumento mal escrito), el bucle del
    # entrypoint lo relanzaría para siempre: se corta al primer "exited" que
    # no sea el de matarlo (0 o 143, SIGTERM).
    if [ $(( $(now_ms) - t0 )) -gt 600000 ] ||
       "$KLING" exec "$m" -- /bin/sh -c "tail -n +$((before + 1)) /var/log/service.log | grep '^service exited' | grep -qvE 'with (0|143),'"; then
      echo "llama-server did not come back; log:" >&2
      "$KLING" exec "$m" -- /bin/sh -c "tail -n +$((before + 1)) /var/log/service.log | grep -v '^[0-9]' | tail -5" >&2
      return 1
    fi
    sleep 0.5
  done
  echo "llama-server restarted in $(( $(now_ms) - t0 )) ms with: ${*:-the arguments of the image}"
}

cmd_ttft() {
  local m="${1:?usage: ttft <machine>}" a out cold=() warm=() pn=0 cn=0
  a="$(addr_of "$m")"
  step "Time to first token with the task prefix ($m, $a)"
  local i=0
  while IFS= read -r line && [ "$i" -lt "$RUNS" ]; do
    i=$((i+1))
    # Otra conversación en la ranura: la siguiente petición no encuentra el
    # prefijo y lo evalúa entero.
    chat "$a" '{"messages":[{"role":"user","content":"Hi"}],"max_tokens":1}' >/dev/null
    out="$(chat "$a" "$(body "$(jq -r .text <<<"$line")" 1 0)")"
    cold+=("$(tail -1 <<<"$out")"); pn="$(head -1 <<<"$out" | jq -r '.timings.prompt_n')"
    # La misma tarea con otra pregunta: el prefijo ya está en la ranura.
    out="$(chat "$a" "$(body "and then $(jq -r .text <<<"$line")" 1 0)")"
    warm+=("$(tail -1 <<<"$out")"); cn="$(head -1 <<<"$out" | jq -r '.timings.cache_n')"
  done < <(requests)
  row "prefix not cached (evaluates $pn tokens), ms" "$(printf '%s\n' "${cold[@]}" | stats)"
  row "prefix cached ($cn tokens reused), ms" "$(printf '%s\n' "${warm[@]}" | stats)"
}

cmd_prefix() {
  [ $# -gt 0 ] || { echo "usage: prefix <golden>..." >&2; exit 1; }
  local first; first="$(head -1 "$REQS" | jq -r .text)"
  for g in "$@"; do
    step "First task request on a fresh replica of $g${SLOT_FILE:+ (restoring slot file $SLOT_FILE)}"
    local total=() req=() rest=() thaw=() pn="" cn=""
    for r in $(seq 1 "$RUNS"); do
      local m="vc-prefix-$$-$r" t0 t1 t2 a out
      t0=$(now_ms)
      out="$("$KLING" run -from "$g" -name "$m" 2>&1)" || { echo "  run -from $g failed: $out" >&2; continue; }
      thaw+=("$(sed -n 's/.* in \([0-9]*\) ms.*/\1/p' <<<"$out")")
      a="$(addr_of "$m")"
      t1=$(now_ms)
      if [ -n "${SLOT_FILE:-}" ]; then
        curl -s --max-time 60 -H 'Content-Type: application/json' "http://$a/slots/0?action=restore" \
          -d "$(jq -nc --arg f "$SLOT_FILE" '{filename:$f}')" >/dev/null
        rest+=("$(( $(now_ms) - t1 ))")
      fi
      out="$(chat "$a" "$(body "$first" 1 0)")"
      t2=$(now_ms)
      req+=("$(( t2 - t1 ))"); total+=("$(( t2 - t0 ))")
      pn="$(head -1 <<<"$out" | jq -r '.timings.prompt_n')"; cn="$(head -1 <<<"$out" | jq -r '.timings.cache_n')"
      "$KLING" rm "$m" >/dev/null 2>&1
    done
    row "restore (daemon), ms" "$(printf '%s\n' "${thaw[@]}" | stats)"
    [ -n "${SLOT_FILE:-}" ] && row "slot restore call, ms" "$(printf '%s\n' "${rest[@]}" | stats)"
    row "first request, ms (evaluated $pn, reused $cn)" "$(printf '%s\n' "${req[@]}" | stats)"
    row "run -from → first token, ms" "$(printf '%s\n' "${total[@]}" | stats)"
  done
}

cmd_switch() {
  local m="${1:?usage: switch <machine>}" a out ms=() ev=() i=0 t2
  a="$(addr_of "$m")"
  step "Alternating two tasks on one replica ($m, $a)"
  while IFS= read -r line && [ "$i" -lt "$RUNS" ]; do
    i=$((i+1))
    t2="$(sed -n "$(( (i - 1) % $(wc -l < "$REQS2") + 1 ))p" "$REQS2")"
    out="$(chat "$a" "$(body "$(jq -r .text <<<"$line")" 1 0)")"
    ms+=("$(tail -1 <<<"$out")"); ev+=("$(head -1 <<<"$out" | jq -r .timings.prompt_n)")
    out="$(chat "$a" "$(SYSTEM_FILE="$SYSTEM2_FILE" body "$t2" 1 0)")"
    ms+=("$(tail -1 <<<"$out")"); ev+=("$(head -1 <<<"$out" | jq -r .timings.prompt_n)")
  done < <(requests)
  row "first token after a task switch, ms" "$(printf '%s\n' "${ms[@]}" | stats)"
  row "tokens evaluated per request" "$(printf '%s\n' "${ev[@]}" | stats)"
}

# gen_series <addr> <temperatura>: tok/s, aceptación y acierto.
gen_series() {
  local a="$1" t="$2" out tps=() wall=() ok=0 n=0 dn=0 da=0 d x
  while IFS= read -r line; do
    out="$(chat "$a" "$(body "$(jq -r .text <<<"$line")" 160 "$t")")"
    d="$(head -1 <<<"$out")"
    tps+=("$(jq -r '.timings.predicted_per_second|floor' <<<"$d")"); wall+=("$(tail -1 <<<"$out")")
    x="$(jq -r '.timings.draft_n // 0' <<<"$d")"; dn=$((dn + ${x:-0}))
    x="$(jq -r '.timings.draft_n_accepted // 0' <<<"$d")"; da=$((da + ${x:-0}))
    ok=$((ok + $(score "$(jq -r '.choices[0].message.content' <<<"$d")" "$(jq -c .expect <<<"$line")")))
    n=$((n+1))
  done < <(requests)
  row "T=$t JSON answers: generation tok/s" "$(printf '%s\n' "${tps[@]}" | stats)"
  row "T=$t JSON answers: request ms (prefix cached)" "$(printf '%s\n' "${wall[@]}" | stats)"
  [ "$dn" -gt 0 ] && row "T=$t JSON answers: draft accepted" "$da/$dn ($(( 100 * da / dn ))%)"
  row "T=$t JSON answers: exact actions" "$ok/$n"
  tps=(); dn=0; da=0
  for r in $(seq 1 "$RUNS"); do
    d="$(chat "$a" "$(jq -nc --arg u "$PARAGRAPH" --argjson t "$t" --argjson s "$r" \
      '{messages:[{role:"user",content:$u}],max_tokens:160,temperature:$t,seed:$s}')" | head -1)"
    tps+=("$(jq -r '.timings.predicted_per_second|floor' <<<"$d")")
    x="$(jq -r '.timings.draft_n // 0' <<<"$d")"; dn=$((dn + ${x:-0}))
    x="$(jq -r '.timings.draft_n_accepted // 0' <<<"$d")"; da=$((da + ${x:-0}))
  done
  row "T=$t paragraph (160 tokens): tok/s" "$(printf '%s\n' "${tps[@]}" | stats)"
  [ "$dn" -gt 0 ] && row "T=$t paragraph: draft accepted" "$da/$dn ($(( 100 * da / dn ))%)"
  return 0
}

cmd_gen() {
  local m="${1:?usage: gen <machine>}" a; a="$(addr_of "$m")"
  step "Generation ($m, $a)"
  chat "$a" "$(body "$(head -1 "$REQS" | jq -r .text)" 1 0)" >/dev/null # el prefijo, a la ranura
  for t in ${TEMPS:-0 0.7}; do gen_series "$a" "$t"; done
}

cmd_schema() {
  local m="${1:?usage: schema <machine>}" a; a="$(addr_of "$m")"
  step "Constrained output ($m, $a)"
  chat "$a" "$(body "$(head -1 "$REQS" | jq -r .text)" 1 0)" >/dev/null
  for mode in free schema; do
    local sc="" v=0 ok=0 n=0 wall=() tps=() out d txt
    [ "$mode" = schema ] && sc="$SCHEMA"
    while IFS= read -r line; do
      out="$(chat "$a" "$(body "$(jq -r .text <<<"$line")" 160 0 "$sc")")"
      d="$(head -1 <<<"$out")"; txt="$(jq -r '.choices[0].message.content' <<<"$d")"
      wall+=("$(tail -1 <<<"$out")"); tps+=("$(jq -r '.timings.predicted_per_second|floor' <<<"$d")")
      v=$((v + $(valid "$txt"))); ok=$((ok + $(score "$txt" "$(jq -c .expect <<<"$line")")))
      n=$((n+1))
    done < <(requests)
    row "$mode: valid JSON for the schema" "$v/$n"
    row "$mode: exact actions" "$ok/$n"
    row "$mode: request ms" "$(printf '%s\n' "${wall[@]}" | stats)"
    row "$mode: generation tok/s" "$(printf '%s\n' "${tps[@]}" | stats)"
  done
}

cmd_quality() {
  local m="${1:?usage: quality <machine>}" a ok=0 n=0 d; a="$(addr_of "$m")"
  while IFS= read -r line; do
    d="$(chat "$a" "$(body "$(jq -r .text <<<"$line")" 160 0 "$SCHEMA")" | head -1)"
    ok=$((ok + $(score "$(jq -r '.choices[0].message.content' <<<"$d")" "$(jq -c .expect <<<"$line")")))
    n=$((n+1))
  done < <(requests)
  row "exact actions (T=0, schema)" "$ok/$n"
}

case "${1:-}" in
  restart|ttft|prefix|switch|gen|schema|quality) c="$1"; shift; "cmd_$c" "$@" ;;
  *) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
