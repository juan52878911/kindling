#!/usr/bin/env bash
# Spike C9-18 (1): ¿mapea KVM los pesos a 2 MiB si el mem.file del dorado vive en un tmpfs huge=always,
# y cuánto baja la primera «petición» tras restaurar? Ver README.md. No toca kindling.
#
#   spike.sh <dir de salida>        (root, en el host del banco; lo lanza remoto/ia.sh con IA_PRUEBAS=s-huge)
#   spike.sh --comprobar            solo los requisitos
#
# Deja en el dir: resultado.json (cifras y recomendación) y los logs de consola de cada VM.
# Entorno: los de lib-fc.sh, VUELTAS (3 restauraciones por variante), TMPFS_MIB (MEM_MIB + 256).
set -uo pipefail
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=SCRIPTDIR/lib-fc.sh
. "$AQUI/lib-fc.sh"
[ "${1:-}" = --comprobar ] && { fc_requisitos && echo "requisitos OK"; exit $?; }
OUT=${1:?uso: spike.sh <dir de salida> | --comprobar}
mkdir -p "$OUT"
fc_requisitos || exit 2
kvm_debugfs || echo "AVISO: sin debugfs de KVM: no habrá salidas ni pages_2m" >&2
VUELTAS=${VUELTAS:-3}
FC_W=$(mktemp -d /var/tmp/r9-huge.XXXXXX); HUGE=$FC_W/huge
limpiar() { fc_parar; mountpoint -q "$HUGE" && umount "$HUGE"; rm -rf "$FC_W"; }
trap limpiar EXIT

fc_paso "invitado de prueba"
fc_rootfs "$FC_W" || exit 1
origen=$(fc_datos "$FC_W/datos.img")
fc_arrancar base || exit 1
carga=$(grep -o 'R9 cargar ms=[0-9.]*' "$FC_LOG" | head -1)
snap=$(fc_snapshot "$FC_W/mem.file" "$FC_W/snap.file") || exit 1
fc_parar
cp "$FC_W/base.log" "$OUT/consola-base.log"

# Variante B: el mismo volcado en un tmpfs con páginas grandes (y A, en el disco, como kindling hoy).
mkdir -p "$HUGE"
mount -t tmpfs -o "size=${TMPFS_MIB:-$(( MEM_MIB + 256 ))}m,huge=always" tmpfs "$HUGE" || exit 1
cp --sparse=always "$FC_W/mem.file" "$HUGE/mem.file" || exit 1
shmem_thp=$(cat /sys/kernel/mm/transparent_hugepage/shmem_enabled 2>/dev/null || echo "?")
huge_mapeadas=$(awk '/^ShmemHugePages:/{print $2}' /proc/meminfo)

: > "$FC_W/muestras.jsonl"
orden="disco tmpfs_huge"
for v in $(seq "$VUELTAS"); do
  for var in $orden; do
    mem=$FC_W/mem.file; [ "$var" = tmpfs_huge ] && mem=$HUGE/mem.file
    # Caché caliente para las dos: aquí se mide la EPT, no el disco.
    cat "$mem" > /dev/null
    load=$(fc_restaurar "$var-$v" "$mem" "$FC_W/snap.file") || { fc_parar; continue; }
    t1=$(fc_tocar 1) || { fc_parar; continue; }
    t2=$(fc_tocar 2) || { fc_parar; continue; }
    pmd=$(awk '/^(ShmemPmdMapped|FilePmdMapped):/{s+=$2} END {print s+0}' /proc/meminfo)
    jq -nc --arg var "$var" --argjson v "$v" --argjson load "$load" --argjson a "$t1" --argjson b "$t2" --argjson pmd "$pmd" \
      '{variante:$var, vuelta:$v, load_ms:$load, primera:$a, segunda:$b, pmd_mapeado_kib:$pmd}' >> "$FC_W/muestras.jsonl"
    cp "$FC_LOG" "$OUT/consola-$var-$v.log"
    fc_parar
  done
  orden="${orden#* } ${orden%% *}"
done

python3 -I - "$FC_W/muestras.jsonl" "$OUT/resultado.json" "$origen" "$carga" "$snap" "$shmem_thp" "${huge_mapeadas:-0}" <<'PY'
import json, sys
f, out, origen, carga, snap, thp, huge = sys.argv[1:8]
ms = [json.loads(l) for l in open(f) if l.strip()]
def p50(xs):
    xs = sorted(x for x in xs if isinstance(x, (int, float)))
    return xs[(len(xs) - 1) // 2] if xs else None
res = {"spike": "r9-hugepages", "datos": origen, "carga_invitado": carga, "volcado": json.loads(snap or "null"),
       "shmem_enabled": thp, "shmem_huge_kib_tras_copiar": int(huge or 0), "muestras": ms, "variantes": {}}
for var in ("disco", "tmpfs_huge"):
    m = [x for x in ms if x["variante"] == var]
    res["variantes"][var] = {
        "n": len(m),
        "peticion_ms_p50": p50([x["primera"]["peticion_ms"] for x in m]),
        "invitado_ms_p50": p50([x["primera"]["invitado_ms"] for x in m]),
        "segunda_ms_p50": p50([x["segunda"]["invitado_ms"] for x in m]),
        "salidas_vm_p50": p50([x["primera"]["salidas_vm"] for x in m]),
        "pages_2m_p50": p50([x["primera"]["pages_2m"] for x in m]),
        "pages_4k_p50": p50([x["primera"]["pages_4k"] for x in m]),
    }
d, h = res["variantes"]["disco"], res["variantes"]["tmpfs_huge"]
if not d["n"] or not h["n"]:
    res["recomendacion"] = "sin cifra: alguna variante no llegó a medirse (ver consola-*.log)"
elif (h["pages_2m_p50"] or 0) > 0 and h["invitado_ms_p50"] <= 0.5 * d["invitado_ms_p50"]:
    res["recomendacion"] = ("seguir: con huge=always KVM mapea a 2 MiB (pages_2m %s) y la primera pasada baja "
                            "de %s a %s ms; ronda 10: prototipo en producto con C9-09" % (
                                h["pages_2m_p50"], d["invitado_ms_p50"], h["invitado_ms_p50"]))
else:
    res["recomendacion"] = ("descartar: pages_2m %s y primera pasada %s → %s ms (no baja a la mitad); "
                            "MAP_PRIVATE sobre shmem no se mapea por PMD aquí" % (
                                h["pages_2m_p50"], d["invitado_ms_p50"], h["invitado_ms_p50"]))
json.dump(res, open(out, "w"), indent=1, ensure_ascii=False)
print(res["recomendacion"])
PY
