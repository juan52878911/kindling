#!/bin/bash
# bench.sh: mide uidump contra `uiautomator dump` desde el Mac, de extremo a
# extremo (kling exec incluido), y compara los nodos de los dos XML.
#
#   prototypes/android/uidump/bench.sh MAQUINA [N] [ETIQUETA]
#
# Necesita KLING_HOST apuntando al daemon de la máquina, `uidump` en la VM
# (en la imagen, o copiado a /usr/local/bin) y python3 en el Mac. Deja todo en
# results/uidump/ETIQUETA/ (no se versiona). La pantalla que se mide es la que
# haya: el que llama abre la app antes.
set -uo pipefail

M="${1:?usage: bench.sh MACHINE [N] [LABEL]}"
N="${2:-50}"
TAG="${3:-$(date +%Y%m%d-%H%M%S)}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/../results/uidump/$TAG"
BASE_N="${BASE_N:-10}"
mkdir -p "$OUT"

# corre FICHERO N COMANDO...: N dumps seguidos, ms por línea. Un solo python
# lanza y cronometra (arrancar python por medida costaría ~20 ms). Un dump vale
# si sale 0 y trae <hierarchy.
corre() {
  local f="$1" n="$2"; shift 2
  python3 - "$f" "$n" "$@" <<'EOF'
import subprocess, sys, time
f, n, cmd = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
with open(f, "w") as out:
    for i in range(n):
        t0 = time.perf_counter()
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        dt = (time.perf_counter() - t0) * 1000
        ok = p.returncode == 0 and b"<hierarchy" in p.stdout
        out.write("%.1f %s\n" % (dt, "ok" if ok else "fail"))
        if i == 0:
            open(f + ".first.out", "wb").write(p.stdout + p.stderr)
EOF
}
pct() { # pct FICHERO: p50 p95 min max fallos
  python3 - "$1" <<'EOF'
import sys
v = [l.split() for l in open(sys.argv[1])]
t = sorted(float(a) for a, s in v if s == "ok"); bad = sum(1 for a, s in v if s != "ok")
q = lambda p: t[min(len(t) - 1, int(round(p * (len(t) - 1))))] if t else float("nan")
print("n=%d p50=%.0f p95=%.0f min=%.0f max=%.0f fallos=%d" % (len(t), q(.5), q(.95), t[0] if t else 0, t[-1] if t else 0, bad))
EOF
}

A=(kling exec "$M" -- android-sh)
U=(kling exec "$M" -- uidump)

echo "== $TAG en $M ($(date))" | tee "$OUT/resumen.txt"
"${A[@]}" 'dumpsys window | grep -m1 mCurrentFocus' | tee -a "$OUT/resumen.txt"

# 1. línea base: uiautomator dump (con el servidor soltando UiAutomation)
"${U[@]}" release >/dev/null 2>&1
corre "$OUT/base.txt" "$BASE_N" "${A[@]}" 'uiautomator dump /data/local/tmp/ui.xml >/dev/null && cat /data/local/tmp/ui.xml'
echo "uiautomator dump: $(pct "$OUT/base.txt")" | tee -a "$OUT/resumen.txt"
"${A[@]}" 'uiautomator dump /data/local/tmp/ui.xml >/dev/null && cat /data/local/tmp/ui.xml' >"$OUT/base.xml"

# 2. uidump: el primero tras release (reconecta) aparte; luego N en caliente
corre "$OUT/primero.txt" 1 "${U[@]}" dump
echo "uidump tras release (reconecta): $(pct "$OUT/primero.txt")" | tee -a "$OUT/resumen.txt"
corre "$OUT/uidump.txt" "$N" "${U[@]}"
echo "uidump: $(pct "$OUT/uidump.txt")" | tee -a "$OUT/resumen.txt"
"${U[@]}" >"$OUT/uidump.xml"

# 3. mismos nodos: (resource-id, text, class, bounds) de cada nodo, en orden
python3 - "$OUT/base.xml" "$OUT/uidump.xml" <<'EOF' | tee -a "$OUT/resumen.txt"
import sys, xml.etree.ElementTree as ET
def nodos(f):
    r = ET.parse(f).getroot()
    return r.attrib, [(n.get("resource-id"), n.get("text"), n.get("class"), n.get("bounds")) for n in r.iter("node")]
(ra, a), (rb, b) = nodos(sys.argv[1]), nodos(sys.argv[2])
same = a == b
print("comparación: uiautomator %d nodos, uidump %d nodos, rotation %s/%s, %s" % (
    len(a), len(b), ra.get("rotation"), rb.get("rotation"), "IGUALES" if same else "DISTINTOS"))
if not same:
    sa, sb = set(a), set(b)
    for x in list(sa - sb)[:5]: print("  solo uiautomator:", x)
    for x in list(sb - sa)[:5]: print("  solo uidump:", x)
EOF
