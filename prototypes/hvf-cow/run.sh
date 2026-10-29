#!/bin/sh
# Corre las pruebas del spike. Uso: ./run.sh DIRECTORIO_DE_DATOS [N]
# Necesita ~3 × 256 MiB libres en disco. Sin sudo.
set -e
cd "$(dirname "$0")"
D=${1:?falta el directorio de datos}
N=${2:-6}
mkdir -p "$D"
B=./hvcow
[ -x $B ] || ./build.sh

# vm_stat en MiB (página de 16K).
memsis() {
  vm_stat | awk -v t="$1" '
    /page size of/ { ps = $8 }
    /Anonymous pages/ { a = $3 } /File-backed pages/ { f = $3 } /Pages free/ { l = $3 }
    /occupied by compressor/ { c = $5 }
    END { printf "%-8s anon=%6d file=%6d free=%6d compressor=%6d MiB\n", t, a*ps/1048576, f*ps/1048576, l*ps/1048576, c*ps/1048576 }'
}

echo "== 1. fichero MAP_PRIVATE: leer 1 MiB, escribir 64 KiB (fichero recién creado, fuera de caché)"
$B create $D/a.img 256 >/dev/null
H0=$(shasum -a 256 $D/a.img | cut -d' ' -f1)
$B create $D/a.img 256 >/dev/null   # reescrito con F_NOCACHE: el shasum lo había metido en caché
$B run -t $D/a.img
H1=$(shasum -a 256 $D/a.img | cut -d' ' -f1)
[ "$H0" = "$H1" ] && echo "hash del fichero sin cambios: $H1" || echo "EL FICHERO CAMBIÓ: $H0 -> $H1"

echo; echo "== 2. control: hv_vm_allocate + copiar el fichero"
$B run -m alloc $D/a.img

echo; echo "== 3. lectura grande (128 MiB) desde fichero frío y luego caliente"
$B create $D/b.img 256 >/dev/null
$B run -r 128 -w 64 -W 200 $D/b.img | grep -E 'hv_vcpu_run|tras run|suma'
$B run -r 128 -w 64 -W 200 $D/b.img | grep -E 'hv_vcpu_run|tras run'

# 4. N procesos escalonados sobre el mismo fichero (frío al principio); cada uno
# lee 192 MiB, escribe 64 KiB y se queda vivo mientras arrancan los demás.
for m in file alloc; do
  echo; echo "== 4. $N procesos escalonados, modo $m"
  $B create $D/c.img 256 >/dev/null
  memsis base
  i=1
  while [ $i -le $N ]; do
    $B run -m $m -r 192 -w 64 -W 200 -h $((3 * N)) $D/c.img > $D/log$i.txt &
    sleep 2.5
    memsis proc$i
    grep -h 'hv_vcpu_run' $D/log$i.txt
    i=$((i + 1))
  done
  wait
  grep -h 'fin espera' $D/log*.txt | awk '{ split($0,p,"phys_footprint="); split(p[2],q," "); fp+=q[1];
    split($0,r,"resident="); split(r[2],s," "); rs+=s[1] }
    END { printf "suma de %d procesos: phys_footprint=%.1f MiB resident=%.1f MiB\n", NR, fp, rs }'
  rm -f $D/c.img $D/log*.txt
done
rm -f $D/*.img
