#!/usr/bin/env bash
# Banco de energía de kindling en ESTE Mac, reproducible y sin sudo.
#
# Levanta su propio daemon en una raíz de usarse y tirarse (nunca el de
# siempre) y mide tres escenarios de DUR segundos cada uno:
#
#   1. idle-daemon     el daemon solo, sin máquinas;
#   2. idle-3          tres máquinas corriendo sin hacer nada (como un compose
#                      de tres servicios ocioso);
#   3. frozen-3        las tres congeladas (sin VMM: solo queda el daemon).
#
# Sin sudo mide con `top -l N -c d -stats pid,command,power,idlew,csw` filtrado a
# los procesos de la prueba: el daemon, cada kling-hv/kling-vz y, con vz, el
# auxiliar de Apple que corre los vCPU. POWER es el "impacto energético" de
# macOS (la columna Energía de Monitor de Actividad: un índice, no vatios),
# IDLEW los despertares desde reposo por segundo y CSW los cambios de
# contexto por segundo. La primera muestra de top no vale (no tiene intervalo
# con el que comparar) y se descarta. Con `sudo -n` disponible añade
# powermetrics (--samplers tasks) para el mismo intervalo.
#
# La salida es un JSON por ejecución en docs/bench-data/ (OUT), con la media,
# el p50 y el p90 por escenario y por proceso, y la fuente de alimentación.
#
#   KLING=./kling VMM=hv ./scripts/94-energia-mac.sh
#   DUR=180 LABEL=qos0 KLING_HV_QOS=0 ./scripts/94-energia-mac.sh
#
# Variables: KLING (binario; su kling-hv/kling-vz junto a él), VMM (hv o vz),
# IMAGES_DIR (de donde se clonan vmlinux, min.ext4 y la imagen), IMG (la
# imagen de las tres máquinas, redis-7-alpine por defecto, o min), MEM (MiB
# por máquina, 256), DUR (segundos por escenario, 600), STEP (segundos entre
# muestras, 5), SETTLE (segundos de espera antes de medir cada escenario, 20),
# ROOT, SOCK, LABEL (va en el nombre del JSON), OUT, KEEP=1 (no limpia).
#
# Mientras mide, todo lo demás que corra en el Mac cuenta en el ruido pero no
# en la cifra (solo se suman los procesos de la prueba). Para comparar dos
# ajustes, mejor dos ejecuciones seguidas con el Mac en el mismo estado.
set -uo pipefail

KLING="${KLING:-kling}"
case "$KLING" in */*) KLING="$(cd "$(dirname "$KLING")" && pwd)/$(basename "$KLING")";; esac
VMM="${VMM:-hv}"
case "$VMM" in vz|hv) ;; *) echo "VMM must be vz or hv" >&2; exit 2;; esac
IMG="${IMG:-redis-7-alpine}"
MEM="${MEM:-256}"
DUR="${DUR:-600}"
STEP="${STEP:-5}"
SETTLE="${SETTLE:-20}"
KEEP="${KEEP:-0}"
LABEL="${LABEL:-default}"
IMAGES_DIR="${IMAGES_DIR:-}"
TMPBASE="${TMPDIR:-/tmp}"; TMPBASE="${TMPBASE%/}"
ROOT="${ROOT:-$TMPBASE/kling-energia/$VMM}"
SOCK="${SOCK:-/tmp/kling-energia-$(id -u).sock}"
LOG="${LOG:-$TMPBASE/kling-energia/daemon-$VMM.log}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${OUT:-$REPO/docs/bench-data/energia-mac-$VMM-$LABEL-$(date +%Y%m%d-%H%M).json}"
P="${E2E_PREFIX:-en-$$}"

export KLING_VMM="$VMM"
export KLING_HOST="unix://$SOCK"
# Configuración vacía: que la del usuario no cambie lo que se mide.
export KLING_CONFIG="$TMPBASE/kling-energia-config-$$.json"

[ "$(uname -s)" = Darwin ] || { echo "macOS only" >&2; exit 2; }
command -v "$KLING" >/dev/null || { echo "missing $KLING" >&2; exit 1; }
command -v python3 >/dev/null || { echo "missing python3" >&2; exit 1; }
k() { "$KLING" "$@"; }

DPID=""
cleanup() {
  if [ "$KEEP" = "1" ]; then echo "KEEP=1: daemon pid $DPID, root $ROOT"; return; fi
  if [ -n "$DPID" ]; then
    for m in $(k ps -a -q 2>/dev/null); do k rm "$m" >/dev/null 2>&1; done
    kill -TERM "$DPID" 2>/dev/null
    for _ in $(seq 1 50); do kill -0 "$DPID" 2>/dev/null || break; sleep 0.2; done
  fi
  pkill -KILL -f "kling-$VMM --api-sock $ROOT/" 2>/dev/null
  rm -f "$KLING_CONFIG"
}
trap cleanup EXIT

mkdir -p "$ROOT/images" "$(dirname "$LOG")" "$(dirname "$OUT")"
echo '{}' > "$KLING_CONFIG"
if [ -n "$IMAGES_DIR" ]; then
  for f in "$IMAGES_DIR"/vmlinux "$IMAGES_DIR"/min.ext4 "$IMAGES_DIR/$IMG".ext4 "$IMAGES_DIR/$IMG".layer.ext4 "$IMAGES_DIR/$IMG".recipe.json; do
    [ -f "$f" ] && [ ! -f "$ROOT/images/$(basename "$f")" ] && cp -c "$f" "$ROOT/images/"
  done
fi

"$KLING" daemon -root "$ROOT" -socket "$SOCK" >>"$LOG" 2>&1 &
DPID=$!
for _ in $(seq 1 50); do k status >/dev/null 2>&1 && break; sleep 0.2; done
estado=$(k status -v 2>&1)
case "$estado" in *"$ROOT"*) ;; *) echo "the daemon at $SOCK is not the one on $ROOT:" >&2; echo "$estado" >&2; exit 1;; esac

# pids_prueba: el daemon, los VMM de esta raíz y, con vz, sus auxiliares.
pids_prueba() {
  echo "$DPID"
  pgrep -f "kling-$VMM --api-sock $ROOT/" 2>/dev/null
  local l
  for l in /tmp/kling-"$(id -u)"/*.sock; do
    [ -L "$l" ] || continue
    case "$(readlink "$l")" in "$ROOT"/*) ;; *) continue;; esac
    curl -s -m 2 --unix-socket "$l" http://x/kling/cpu 2>/dev/null |
      python3 -c 'import json,sys
try:
    p=json.load(sys.stdin).get("helper_pid",0)
    print(p) if p else None
except Exception: pass'
  done
}

# medir <escenario>: top durante DUR s sobre los pids de la prueba; deja el
# resultado en $TMPBASE/kling-energia/<escenario>.json
medir() {
  local esc=$1 n=$(( DUR / STEP + 1 )) args=() p
  local pids; pids=$(pids_prueba | sort -un)
  for p in $pids; do args+=(-pid "$p"); done
  echo "  $esc: $(echo "$pids" | wc -w | tr -d ' ') process(es), ${DUR}s"
  local crudo="$TMPBASE/kling-energia/$esc.top"
  # powermetrics, si se puede, a la vez que top: el mismo intervalo.
  local pm="" pmpid=""
  if sudo -n true 2>/dev/null; then
    pm="$TMPBASE/kling-energia/$esc.pm"
    # shellcheck disable=SC2024 # a propósito: el fichero es del usuario, solo powermetrics necesita root
    sudo -n powermetrics --samplers tasks --show-process-energy -i $((STEP*1000)) -n $((n-1)) > "$pm" 2>/dev/null &
    pmpid=$!
  fi
  top -l "$n" -s "$STEP" -c d -stats pid,command,power,idlew,csw "${args[@]}" > "$crudo" 2>/dev/null
  [ -n "$pmpid" ] && wait "$pmpid"
  python3 - "$crudo" "$esc" "$DPID" "$pm" "$STEP" $pids > "$TMPBASE/kling-energia/$esc.json" <<'PY'
import json, statistics, sys
crudo, esc, dpid, pm, step = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], float(sys.argv[5])
pids = set(sys.argv[6:])
muestras, actual, en_tabla = [], None, False
for linea in open(crudo, errors="replace"):
    c = linea.split()
    if c[:2] == ["PID", "COMMAND"]:
        actual, en_tabla = {}, True
        muestras.append(actual)
        continue
    if not en_tabla or len(c) < 5 or c[0] not in pids:
        if not c:
            en_tabla = False
        continue
    try:
        pw, iw, cs = (float(x.rstrip("+-")) for x in c[-3:])
    except ValueError:
        continue
    # -c d: IDLEW y CSW son lo de este intervalo; se pasan a por segundo.
    actual[c[0]] = {"command": " ".join(c[1:-3]), "power": pw, "idlew": iw / step, "csw": cs / step}
muestras = muestras[1:]  # la primera no tiene intervalo
def resumen(xs):
    xs = sorted(xs)
    if not xs:
        return {"mean": 0, "p50": 0, "p90": 0}
    return {"mean": round(statistics.fmean(xs), 3), "p50": round(xs[len(xs)//2], 3),
            "p90": round(xs[min(len(xs)-1, int(len(xs)*0.9))], 3)}
tot = {k: [sum(p[k] for p in m.values()) for m in muestras] for k in ("power", "idlew", "csw")}
por = {}
for m in muestras:
    for pid, v in m.items():
        rol = "daemon" if pid == dpid else ("vmm" if v["command"].startswith("kling") else "helper")
        d = por.setdefault(rol, {"power": [], "idlew": [], "csw": []})
        for k in d:
            d[k].append(v[k])
out = {"scenario": esc, "samples": len(muestras), "processes": len(pids),
       "total": {k: resumen(v) for k, v in tot.items()},
       "by_role": {r: {k: resumen(v) for k, v in d.items()} for r, d in por.items()}}
if pm:
    out["powermetrics_raw"] = pm
print(json.dumps(out))
PY
}

echo "kindling energy bench: VMM=$VMM IMG=$IMG DUR=${DUR}s STEP=${STEP}s LABEL=$LABEL"

# 1. el daemon solo
sleep "$SETTLE"
medir idle-daemon

# 2. tres máquinas ociosas
for i in 1 2 3; do
  k run -name "$P-$i" -image "$IMG" -mem "$MEM" >/dev/null 2>&1 || { echo "kling run $P-$i failed" >&2; exit 1; }
done
sleep "$SETTLE"
medir idle-3

# 3. las tres congeladas
for i in 1 2 3; do k freeze "$P-$i" >/dev/null 2>&1 || echo "  warning: freeze $P-$i failed" >&2; done
sleep "$SETTLE"
medir frozen-3

fuente=$(pmset -g ps 2>/dev/null | head -1 | sed "s/.*'\(.*\)'.*/\1/")
python3 - "$OUT" "$VMM" "$LABEL" "$IMG" "$DUR" "$STEP" "${KLING_HV_QOS:-}" "$fuente" \
  "$(sw_vers -productVersion)" "$(sysctl -n hw.model)" "$("$KLING" version 2>/dev/null | head -1)" \
  "$TMPBASE/kling-energia/idle-daemon.json" "$TMPBASE/kling-energia/idle-3.json" "$TMPBASE/kling-energia/frozen-3.json" <<'PY'
import json, sys, datetime
out, vmm, label, img, dur, step, qos, fuente, macos, modelo, version = sys.argv[1:12]
esc = [json.load(open(p)) for p in sys.argv[12:]]
doc = {"bench": "energia-mac", "date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
       "vmm": vmm, "label": label, "image": img, "duration_s": int(dur), "step_s": int(step),
       "env": {"KLING_HV_QOS": qos}, "power_source": fuente, "macos": macos, "model": modelo,
       "kling": version, "units": {"power": "macOS energy impact (top POWER)", "idlew": "wakeups/s",
       "csw": "context switches/s"}, "scenarios": esc}
json.dump(doc, open(out, "w"), indent=2)
print(f"\n{'scenario':<14}{'power':>10}{'idlew/s':>10}{'csw/s':>10}   (mean of the test's processes)")
for e in esc:
    t = e["total"]
    print(f"{e['scenario']:<14}{t['power']['mean']:>10}{t['idlew']['mean']:>10}{t['csw']['mean']:>10}")
print(f"\nwrote {out}")
PY
