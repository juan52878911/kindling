#!/usr/bin/env bash
# Spike C9-02 M: ¿cuánto cuesta el commit de hoy (volcado Full al disco + fallocate --dig-holes + digest
# releyendo) frente a volcar a un tmpfs y pasarlo al disco con una copia dispersa que hashea a la vez?
# Y, de paso, lo que C9-02 pide confirmar: si el volcado de Firecracker deja la caché sucia o vacía.
# Ver README.md. No toca kindling.
#
#   spike.sh <dir de salida>        (root, en el host del banco; remoto/ia.sh con IA_PRUEBAS=s-volcado)
#   spike.sh --comprobar
#
# Entorno: los de lib-fc.sh (MEM_MIB 1600 y DATOS_MIB 1065 = von-q4), VUELTAS (2), DESTINO (el disco a
# medir; por defecto /var/tmp, que en la EC2 es el gp3 de la raíz).
set -uo pipefail
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=SCRIPTDIR/../r9-hugepages/lib-fc.sh
. "$AQUI/../r9-hugepages/lib-fc.sh"
[ "${1:-}" = --comprobar ] && { fc_requisitos && command -v fallocate >/dev/null && echo "requisitos OK"; exit $?; }
OUT=${1:?uso: spike.sh <dir de salida> | --comprobar}
mkdir -p "$OUT"
fc_requisitos || exit 2
command -v fallocate >/dev/null || { echo "falta fallocate (util-linux)" >&2; exit 2; }
VUELTAS=${VUELTAS:-2}
FC_W=$(mktemp -d "${DESTINO:-/var/tmp}/r9-volcado.XXXXXX"); TMPFS=$FC_W/tmpfs
limpiar() { fc_parar; mountpoint -q "$TMPFS" && umount "$TMPFS"; rm -rf "$FC_W"; }
trap limpiar EXIT
mkdir -p "$TMPFS"
mount -t tmpfs -o "size=$(( MEM_MIB + 64 ))m" tmpfs "$TMPFS" || exit 1

fc_paso "invitado de prueba"
fc_rootfs "$FC_W" || exit 1
origen=$(fc_datos "$FC_W/datos.img")

# sucio_mib / en_cache_mib <fichero>: lo que dice el núcleo justo después del volcado.
sucio_mib() { awk '/^Dirty:/{printf "%d", $2/1024}' /proc/meminfo; }
en_cache_mib() { python3 -I - "$1" <<'PY'
import ctypes, os, sys
f = sys.argv[1]
n = os.path.getsize(f)
if n == 0:
    print(0)
    sys.exit()
libc = ctypes.CDLL(None, use_errno=True)
libc.mmap.restype = ctypes.c_void_p
libc.mmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_long]
fd = os.open(f, os.O_RDONLY)
base = libc.mmap(None, n, 1, 1, fd, 0)  # PROT_READ, MAP_SHARED
if base in (None, ctypes.c_void_p(-1).value):
    print(-1)
    sys.exit()
buf = (ctypes.c_ubyte * ((n + 4095) // 4096))()
if libc.mincore(ctypes.c_void_p(base), ctypes.c_size_t(n), buf) != 0:
    print(-1)
    sys.exit()
print(sum(x & 1 for x in buf) * 4096 >> 20)
PY
}
# copia_dispersa_hash <origen> <destino>: un solo pase: lee trozos de 4 MiB, salta las páginas a cero
# (quedan como hueco en el destino), escribe el resto, hashea el trozo y hace fsync al final. Es el
# «arreglo M» de C9-02 en Python (el producto lo haría en Go): la cifra es una cota superior.
copia_dispersa_hash() { python3 -I - "$1" "$2" <<'PY'
import hashlib, os, sys, time
src, dst = sys.argv[1:3]
T, P = 4 << 20, 4096
cero = bytes(P)
t0 = time.time()
fi, fo = os.open(src, os.O_RDONLY), os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
n = os.fstat(fi).st_size
os.ftruncate(fo, n)
h = hashlib.sha256()
esc = 0
off = 0
while off < n:
    b = os.pread(fi, T, off)
    if not b:
        break
    mv = memoryview(b)
    i = 0
    while i < len(b):
        if mv[i:i + P] == cero:
            i += P
            continue
        j = i
        while j < len(b) and mv[j:j + P] != cero:
            j += P
        os.pwrite(fo, mv[i:j], off + i)
        esc += j - i
        i = j
    h.update(hashlib.sha256(b).digest())
    off += len(b)
os.fsync(fo)
print('{"ms":%d,"escritos_bytes":%d,"hash":"%s"}' % ((time.time() - t0) * 1000, esc, h.hexdigest()))
PY
}
# releer_hash <fichero>: lo que hace digest.Tree hoy, saltando huecos (SEEK_DATA), en un pase.
releer_hash() { python3 -I - "$1" <<'PY'
import hashlib, os, sys, time
f = sys.argv[1]
T = 4 << 20
t0 = time.time()
fd = os.open(f, os.O_RDONLY)
n = os.fstat(fd).st_size
h = hashlib.sha256()
off = 0
while off < n:
    try:
        d = os.lseek(fd, off, os.SEEK_DATA)
    except OSError:
        break
    b = os.pread(fd, T, d)
    if not b:
        break
    h.update(b)
    off = d + len(b)
print('{"ms":%d}' % ((time.time() - t0) * 1000))
PY
}

: > "$FC_W/muestras.jsonl"
orden="hoy m"
for v in $(seq "$VUELTAS"); do
  for var in $orden; do
    fc_arrancar "$var-$v" || { fc_parar; continue; }
    sync; echo 3 > /proc/sys/vm/drop_caches
    w0=$(disco_bytes_escritos); t0=$(fc_ms)
    if [ "$var" = hoy ]; then
      vol=$(fc_snapshot "$FC_W/mem.file" "$FC_W/snap.file") || { fc_parar; continue; }
      s_mib=$(sucio_mib); c_mib=$(en_cache_mib "$FC_W/mem.file")
      th=$(fc_ms); fallocate --dig-holes "$FC_W/mem.file"; huecos=$(( $(fc_ms) - th ))
      sync -f "$FC_W/mem.file"
      dig=$(releer_hash "$FC_W/mem.file")
      paso2=$(jq -nc --argjson h "$huecos" --argjson d "$dig" '{huecos_ms:$h, digest_ms:$d.ms}')
    else
      vol=$(fc_snapshot "$TMPFS/mem.file" "$FC_W/snap.file") || { fc_parar; continue; }
      s_mib=$(sucio_mib); c_mib=null
      cp=$(copia_dispersa_hash "$TMPFS/mem.file" "$FC_W/mem-m.file")
      rm -f "$TMPFS/mem.file"
      paso2=$(jq -nc --argjson c "$cp" '{copia_hash_ms:$c.ms, copia_escritos_bytes:$c.escritos_bytes}')
    fi
    tot=$(( $(fc_ms) - t0 )); esc=$(( $(disco_bytes_escritos) - w0 ))
    mf=$FC_W/mem.file; [ "$var" = m ] && mf=$FC_W/mem-m.file
    asig=$(( $(du -k "$mf" | cut -f1) * 1024 ))
    jq -nc --arg var "$var" --argjson v "$v" --argjson vol "$vol" --argjson p "$paso2" --argjson tot "$tot" \
      --argjson esc "$esc" --argjson s "${s_mib:-null}" --argjson c "${c_mib:-null}" --argjson a "${asig:-null}" \
      '{variante:$var, vuelta:$v, volcado:$vol, pasos:$p, total_ms:$tot, disco_escritos_bytes:$esc,
        sucio_tras_volcado_mib:$s, en_cache_tras_volcado_mib:$c, asignado_bytes:$a}' >> "$FC_W/muestras.jsonl"
    fc_parar
  done
  orden="${orden#* } ${orden%% *}"
done
# El contenido es el mismo por los dos caminos (los huecos leen como ceros).
igual=null
[ -f "$FC_W/mem.file" ] && [ -f "$FC_W/mem-m.file" ] && { if cmp -s "$FC_W/mem.file" "$FC_W/mem-m.file"; then igual=true; else igual=false; fi; }

python3 -I - "$FC_W/muestras.jsonl" "$OUT/resultado.json" "$origen" "$igual" "$MEM_MIB" <<'PY'
import json, sys
f, out, origen, igual, mem = sys.argv[1:6]
ms = [json.loads(l) for l in open(f) if l.strip()]
def p50(xs):
    xs = sorted(x for x in xs if isinstance(x, (int, float)))
    return xs[(len(xs) - 1) // 2] if xs else None
res = {"spike": "r9-volcado-tmpfs", "datos": origen, "mem_mib": int(mem), "mismo_contenido": json.loads(igual),
       "muestras": ms, "variantes": {}}
for var in ("hoy", "m"):
    m = [x for x in ms if x["variante"] == var]
    res["variantes"][var] = {"n": len(m), "total_ms_p50": p50([x["total_ms"] for x in m]),
                             "volcado_ms_p50": p50([x["volcado"]["ms"] for x in m]),
                             "disco_escritos_mib_p50": (p50([x["disco_escritos_bytes"] for x in m]) or 0) >> 20,
                             "sucio_tras_volcado_mib_p50": p50([x["sucio_tras_volcado_mib"] for x in m]),
                             "en_cache_tras_volcado_mib_p50": p50([x["en_cache_tras_volcado_mib"] for x in m])}
h, m = res["variantes"]["hoy"], res["variantes"]["m"]
if not h["n"] or not m["n"]:
    res["recomendacion"] = "sin cifra: alguna variante no llegó a medirse"
elif res["mismo_contenido"] is False:
    res["recomendacion"] = "descartar tal cual: la copia dispersa no da el mismo contenido (revisar el prototipo)"
elif m["total_ms_p50"] <= 0.6 * h["total_ms_p50"]:
    res["recomendacion"] = ("seguir: commit %s → %s ms y %s → %s MiB escritos; ronda 10: variante M en Go con "
                            "admisión de memoria (RAM transitoria = MemMiB)" % (
                                h["total_ms_p50"], m["total_ms_p50"], h["disco_escritos_mib_p50"], m["disco_escritos_mib_p50"]))
else:
    res["recomendacion"] = ("descartar: %s → %s ms no compensa la RAM transitoria; quedarse con el pase único "
                            "(C9-02 S)" % (h["total_ms_p50"], m["total_ms_p50"]))
json.dump(res, open(out, "w"), indent=1, ensure_ascii=False)
print(res["recomendacion"])
PY
