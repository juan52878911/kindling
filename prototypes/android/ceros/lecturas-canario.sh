#!/bin/bash
# lecturas-canario.sh — reproductor del issue #87 (docs/sigill.md, "La causa").
#
#   KLING_HOST=unix://$HOME/.kindling-X/kling.sock \
#     prototypes/android/ceros/lecturas-canario.sh -from DORADO [-cycles 8] \
#       [-canary 256] [-cpu-pct 50] [-out DIR] [-keep]
#
# En un clon de DORADO (Android 13 con la capa en vdc):
#   - el canario (i87probe canary; cómo compilarlo, abajo):
#     N MiB anónimos con un patrón no nulo por página, comprobados cada 2 s;
#     cada página mala va a la consola del invitado (sobrevive a un pánico)
#     con su PFN y su grupo de 16 KiB (una página del host);
#   - cada ciclo, 10 pasadas de drop_caches + md5sum -P16 de /android/system
#     (vdc) y /usr (vda) contra una referencia de dos pasadas iguales; una
#     lectura mala dice el dispositivo, las páginas de 4 KiB y si son ceros;
#   - las líneas nuevas de "device-mapper: verity" (con la capa en verity:
#     si hay md5 malos de vdc sin líneas de verity, el daño es en la RAM, no
#     en la lectura).
# Deja OUT/ciclos.csv, malas.txt, canary.txt y console.txt.
#
# Compilar la sonda (en Linux arm64, p. ej. la VM Lima):
#   gcc -O2 -static -o i87probe prototypes/android/ceros/i87probe.c
# y dejarla junto a este script (o I87PROBE=/ruta).
#
# El fallo necesita un Mac con presión de memoria (memorystatus_level 30–50,
# compresor trabajando) y E/S de disco en el invitado; con -cpu-pct bajo (el
# freno de señales de kling-vz) sale mucho más. Mira el Mac mientras corre.
set -u
KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PROBE="${I87PROBE:-$HERE/i87probe}"
FROM="" C=8 MIB=256 CPU="" OUT="" KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    -from) FROM="$2"; shift 2 ;;
    -cycles) C="$2"; shift 2 ;;
    -canary) MIB="$2"; shift 2 ;;
    -cpu-pct) CPU="$2"; shift 2 ;;
    -out) OUT="$2"; shift 2 ;;
    -keep) KEEP=1; shift ;;
    *) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
  esac
done
[ -n "$FROM" ] || { echo "lecturas-canario: -from DORADO is required" >&2; exit 2; }
[ -x "$PROBE" ] || { echo "lecturas-canario: no i87probe at $PROBE" >&2; exit 2; }
OUT="${OUT:-$HERE/../results/ceros-$(date +%Y%m%d-%H%M%S)}"; mkdir -p "$OUT"; touch "$OUT/malas.txt"
k() { "$KLING" "$@"; }
m="ceros$$"
LECTURAS='
R=/run/stress-lecturas; mkdir -p $R
pasada() { sync; echo 3 > /proc/sys/vm/drop_caches
  tr "\n" "\0" < $R/files | xargs -0 -P 16 -n 16 md5sum 2>$R/err | sort -k2 > $1
  [ -s $R/err ] && sed "s/^/READ_EIO pass=$i /" $R/err | head -5; return 0; }
if [ ! -f $R/ref ]; then
  find /android/system /usr -xdev -type f -size +16k 2>/dev/null | sort > $R/files
  pasada $R/ref; pasada $R/ref2
  cmp -s $R/ref $R/ref2 || echo "READ_BAD_REF reference passes differ"
fi
i=0
while [ $i -lt $1 ]; do
  i=$((i+1)); pasada $R/cur
  cmp -s $R/ref $R/cur && continue
  diff $R/ref $R/cur | sed -n "s/^> [0-9a-f]*  //p" | while read -r f; do
    dd if="$f" iflag=direct bs=1M status=none > $R/good 2>/dev/null
    cat "$f" > $R/bad
    case "$f" in /android/*) dev=vdc ;; *) dev=vda ;; esac
    echo "READ_BAD dev=$dev pass=$i $f pages4k=[$(cmp -l $R/good $R/bad | awk "{print int((\$1-1)/4096)}" | uniq | tr "\n" " " | cut -c1-60)] bytes=$(cmp -l $R/good $R/bad | wc -l) nonzero_in_bad=$(cmp -l $R/good $R/bad | awk "\$3 != 0" | wc -l)"
  done
done'
canario() {
  k cp "$PROBE" "$m":/run/i87probe >/dev/null 2>&1
  k exec "$m" -- sh -c "echo on > /proc/sys/kernel/printk_devkmsg; chmod +x /run/i87probe; nohup /run/i87probe canary $MIB 2 1000000 > /run/canary.log 2>&1 &" >/dev/null 2>&1
}
k run -from "$FROM" -name "$m" ${CPU:+-cpu-pct $CPU} >/dev/null || exit 1
sleep 3; canario
echo "cycle,time,memlevel,md5_bad,verity_lines,uptime_s" > "$OUT/ciclos.csv"
up_prev=0
for c in $(seq 1 "$C"); do
  out="$(k exec -timeout 240s "$m" -- sh -c "$LECTURAS" lecturas 10 2>&1)"
  echo "$out" | grep -E "READ_BAD|READ_EIO" | sed "s/^/c=$c /" >> "$OUT/malas.txt"
  v=$(k exec -timeout 20s "$m" -- sh -c 'dmesg | grep "device-mapper: verity" | grep -vc "using implementation"' 2>/dev/null | tr -d '\r\n ')
  up=$(k exec -timeout 10s "$m" -- cut -d. -f1 /proc/uptime 2>/dev/null | tr -d '\r\n ')
  # tras un pánico el invitado se reinicia (panic=1): se vuelve a poner el canario
  if [ -n "$up" ] && [ "$up" -lt "$up_prev" ]; then canario; fi
  up_prev=${up:-0}
  echo "$c,$(date +%T),$(sysctl -n kern.memorystatus_level),$(echo "$out" | grep -c "READ_BAD dev"),${v:-?},${up:-?}" >> "$OUT/ciclos.csv"
done
k logs -tail 1000000 "$m" > "$OUT/console.txt" 2>/dev/null
grep CANARY_BAD "$OUT/console.txt" > "$OUT/canary.txt"
echo "cycles=$C md5_bad=$(grep -c 'READ_BAD dev' "$OUT/malas.txt" 2>/dev/null || echo 0) canary_bad=$(wc -l < "$OUT/canary.txt" | tr -d ' ') panics=$(grep -c 'Kernel panic' "$OUT/console.txt") out=$OUT"
[ "$KEEP" = 1 ] || k rm -f "$m" >/dev/null 2>&1
