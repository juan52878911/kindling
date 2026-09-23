#!/usr/bin/env bash
# Mide un modelo VON (kling models add) en el daemon al que apunte kling: lo que
# docs/von.md cuenta en sus tablas.
#
#   ./96-von-bench.sh von-smol                 dorado ya hecho
#   COLD=1 ./96-von-bench.sh von-smol          + arranque en frío desde la imagen
#   RUNS=10 REPLICAS=4 ./96-von-bench.sh von-smol
#
# Mide:
#   1. del arranque en frío al primer token (COLD=1): kling run -image + carga
#      del modelo + una respuesta de 1 token;
#   2. del thaw al primer token: kling run -from + una respuesta de 1 token, RUNS
#      veces, con una réplica nueva cada vez;
#   3. tokens/s de prompt y de generación (los tiempos que da llama-server, sin
#      red ni proxy), en una réplica caliente;
#   4. memoria de 1..REPLICAS réplicas del mismo dorado (kling top: PSS; en
#      Linux también la suma de RSS, que cuenta las páginas compartidas una vez
#      por réplica: la diferencia ES lo que se comparte);
#   5. que dos réplicas del mismo dorado no repiten la misma muestra con la
#      misma petición a temperatura 1 (semillas distintas tras restaurar).
#
# Los tiempos incluyen arrancar el CLI de kling (~10 ms) y el proxy del daemon:
# es lo que ve un cliente que llega por el socket.

set -uo pipefail

KLING="${KLING:-kling}"
SNAP="${1:?usage: 96-von-bench.sh <golden snapshot> (made with kling models add)}"
RUNS="${RUNS:-5}"
REPLICAS="${REPLICAS:-4}"
COLD="${COLD:-0}"
PREFIX="von-bench-$$"

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need jq; need perl

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
step()   { printf "\n\033[1m%s\033[0m\n" "$1"; }
row()    { printf "  %-44s %s\n" "$1" "$2"; }
# stats lee números por stdin y saca "p50 (min–max)".
stats()  { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "n/a"; exit} m=a[int((NR+1)/2)]; printf "%s (%s–%s, n=%d)\n", m, a[1], a[NR], NR}'; }

made=()
cleanup() {
  for m in "${made[@]:-}"; do
    [ -n "$m" ] && "$KLING" rm "$m" >/dev/null 2>&1
  done
}
trap cleanup EXIT

info="$("$KLING" snapshots -json | jq -c --arg s "$SNAP" '.[] | select(.name==$s)')"
[ -n "$info" ] || { echo "no snapshot named $SNAP" >&2; exit 1; }
model="$(jq -r '.labels["von.model"] // empty' <<<"$info")"
[ -n "$model" ] || { echo "$SNAP is not a VON model (no von.model label)" >&2; exit 1; }
image="$(jq -r .image <<<"$info")"
vcpus="$(jq -r .vcpus <<<"$info")"
mem="$(jq -r .mem_mib <<<"$info")"
cpupct="$(jq -r '.cpu_pct // 0' <<<"$info")"
backend="$("$KLING" info -json | jq -r '.backend // "?"')"

step "Model $model  (snapshot $SNAP, image $image, $vcpus vCPU, $mem MiB, backend $backend)"
row "snapshot memory file (allocated)" "$(jq -r '.mem_bytes/1048576|floor' <<<"$info") MiB"
row "snapshot on disk (memory + state + overlay)" "$(jq -r '.disk_bytes/1048576|floor' <<<"$info") MiB"
row "image layer on disk" "$("$KLING" images ls -json | jq -r --arg i "$image" '.[]|select(.name==$i)|.disk_bytes/1048576|floor') MiB"

ask() { # ask <machine> <max_tokens> <temperature> <prompt>
  "$KLING" models ask -json -max-tokens "$2" -temperature "$3" "$1" "$4"
}

# ── 1. frío ───────────────────────────────────────────────────────────────────
if [ "$COLD" = 1 ]; then
  step "1. Cold boot to first token (kling run -image, load, 1 token)"
  m="$PREFIX-cold"; made+=("$m")
  t0=$(now_ms)
  args=(-name "$m" -image "$image" -cpus "$vcpus" -mem "$mem" -label kling.ports=8000 -label "von.model=$model")
  [ "$cpupct" -gt 0 ] && args+=(-cpu-pct "$cpupct")
  if "$KLING" run "${args[@]}" >/dev/null; then
    t1=$(now_ms)
    out="$("$KLING" models ask -json -wait 30m -max-tokens 1 -temperature 0 "$m" "Hi")"
    t2=$(now_ms)
    row "boot (kling run returns)" "$((t1 - t0)) ms"
    row "boot to first token" "$((t2 - t0)) ms"
    row "  of which the 1-token request itself" "$(jq -r .request_ms <<<"$out") ms"
  else
    row "cold boot" "FAILED"
  fi
  "$KLING" rm "$m" >/dev/null 2>&1
fi

# ── 2. thaw ───────────────────────────────────────────────────────────────────
step "2. Thaw to first token (kling run -from $SNAP, 1 token), $RUNS runs"
thaw=(); first=(); req=()
for i in $(seq "$RUNS"); do
  m="$PREFIX-t$i"; made+=("$m")
  t0=$(now_ms)
  out="$("$KLING" run -from "$SNAP" -name "$m" 2>&1)" || { row "run $i" "FAILED: $out"; continue; }
  a="$(ask "$m" 1 0 "Hi")" || { row "ask $i" "FAILED"; "$KLING" rm "$m" >/dev/null 2>&1; continue; }
  t1=$(now_ms)
  thaw+=("$(sed -n 's/.* in \([0-9]*\) ms.*/\1/p' <<<"$out")")
  first+=("$((t1 - t0))")
  req+=("$(jq -r .request_ms <<<"$a")")
  "$KLING" rm "$m" >/dev/null 2>&1
done
row "restore (daemon's thaw_ms), p50" "$(printf '%s\n' "${thaw[@]}" | stats) ms"
row "run -from to first token, p50" "$(printf '%s\n' "${first[@]}" | stats) ms"
row "  of which the 1-token request" "$(printf '%s\n' "${req[@]}" | stats) ms"

# ── 3. tokens/s ───────────────────────────────────────────────────────────────
step "3. Throughput on a warm replica (llama-server timings)"
m="$PREFIX-tp"; made+=("$m")
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
row "prompt eval ($pn tokens), tok/s" "$(printf '%s\n' "${pp[@]}" | stats)"
row "generation ($gn tokens), tok/s" "$(printf '%s\n' "${tg[@]}" | stats)"
"$KLING" rm "$m" >/dev/null 2>&1

# ── 4. memoria de N réplicas ─────────────────────────────────────────────────
step "4. Memory of 1..$REPLICAS replicas of the same snapshot (after one request each)"
printf "  %-10s %-16s %-18s %s\n" "replicas" "total PSS MiB" "PSS per replica" "sum of RSS MiB (Linux)"
reps=()
for n in $(seq "$REPLICAS"); do
  m="$PREFIX-r$n"; made+=("$m"); reps+=("$m")
  "$KLING" run -from "$SNAP" -name "$m" >/dev/null || { echo "  replica $n FAILED"; break; }
  ask "$m" 16 0 "Name a color." >/dev/null
  top="$("$KLING" top -json)"
  pss="$(jq -r --arg p "$PREFIX-r" '[.machines[]|select(.name|startswith($p))|.pss_mib]|add' <<<"$top")"
  per="$(jq -r --arg p "$PREFIX-r" '[.machines[]|select(.name|startswith($p))|.pss_mib]|map(tostring)|join(" ")' <<<"$top")"
  rss="n/a"
  if [ -d /proc ] && [ "$(uname -s)" = Linux ]; then
    rss=0
    for pid in $(jq -r --arg p "$PREFIX-r" '.machines[]|select(.name|startswith($p))|.pid' <<<"$top"); do
      k=$(awk '/^VmRSS/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
      rss=$((rss + ${k:-0} / 1024))
    done
  fi
  printf "  %-10s %-16s %-18s %s\n" "$n" "$pss" "$per" "$rss"
done

# ── 5. semillas ──────────────────────────────────────────────────────────────
step "5. Sampling seeds across replicas (same request, temperature 1)"
if [ "${#reps[@]}" -ge 2 ]; then
  q="Invent a name for a new planet. Answer with the name only."
  a1="$(ask "${reps[0]}" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  a2="$(ask "${reps[1]}" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  a3="$(ask "${reps[0]}" 12 1 "$q" | jq -r '.response.choices[0].message.content')"
  row "replica 1" "${a1//$'\n'/ }"
  row "replica 2" "${a2//$'\n'/ }"
  row "replica 1 again" "${a3//$'\n'/ }"
  if [ "$a1" = "$a2" ]; then row "verdict" "SAME output on two replicas (check the seed)"; else row "verdict" "different outputs: replicas do not share a seed"; fi
fi
