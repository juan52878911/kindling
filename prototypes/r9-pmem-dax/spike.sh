#!/usr/bin/env bash
# Spike C9-18 (2): pesos por virtio-pmem con DAX, fuera de la RAM del invitado. Tres etapas, cada una
# corta el spike con su cifra o su motivo (ver README.md). No toca kindling ni su kernel: si hace falta
# kernel, se compila uno de prueba aparte (VARIANT=pmem sobre una COPIA de scripts/builders/kernel).
#
#   spike.sh <dir de salida>        (root, en el host del banco; remoto/ia.sh con IA_PRUEBAS=s-pmem)
#   spike.sh --comprobar            requisitos y etapa 1 (¿Firecracker admite pmem?), sin compilar
#
# Etapas:
#   1. ¿Firecracker (el instalado) tiene virtio-pmem? Se mira la especificación de su API
#      ($KB/firecracker-api.yaml, la deja ia.sh preparar) y se prueba PUT /pmem/<id> en una VM sin arrancar.
#      No → resultado «descartar», con la evidencia.
#   2. Kernel de invitado de prueba con VIRTIO_PMEM + FS_DAX (KERNEL_BUILDER: una copia de
#      scripts/builders/kernel con check-kernel-config.sh dos niveles arriba; la sube ec2.sh).
#   3. Misma VM de prueba dos veces: pesos en RAM (como hoy) y pesos en un ext4 por pmem montado con
#      dax; volcado Full (MiB escritos, tamaño del mem.file) y restaurar + primera pasada por los pesos.
# Entorno: los de lib-fc.sh, KB (/opt/kbench), KERNEL_BUILDER ($KB/builders/kernel), VMLINUX_PMEM (un
# kernel de prueba ya hecho: se salta la etapa 2), VUELTAS (2).
set -uo pipefail
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=SCRIPTDIR/../r9-hugepages/lib-fc.sh
. "$AQUI/../r9-hugepages/lib-fc.sh"
KB=${KB:-/opt/kbench}
SPEC=${FC_API_YAML:-$KB/firecracker-api.yaml}
VUELTAS=${VUELTAS:-2}
OUT=${1:?uso: spike.sh <dir de salida> | --comprobar}
COMPROBAR=0; [ "$OUT" = --comprobar ] && { COMPROBAR=1; OUT=$(mktemp -d); }
mkdir -p "$OUT"
fc_requisitos || exit 2
FC_W=$(mktemp -d /var/tmp/r9-pmem.XXXXXX)
limpiar() { fc_parar; rm -rf "$FC_W"; [ "$COMPROBAR" = 1 ] && rm -rf "$OUT"; }
trap limpiar EXIT
fin() { # fin <recomendación> [json extra]: escribe resultado.json y sale
  local x="${2:-}"
  [ -n "$x" ] || x='{}'
  jq -n --arg r "$1" --argjson x "$x" --arg fc "$("$FC" --version 2>&1 | head -1)" \
    '{spike:"r9-pmem-dax", firecracker:$fc, recomendacion:$r} + $x' > "$OUT/resultado.json"
  echo "$1"; [ "$COMPROBAR" = 1 ] && cat "$OUT/resultado.json"
  exit 0
}

# ── etapa 1 ──────────────────────────────────────────────────────────────────
fc_paso "etapa 1: ¿virtio-pmem en $("$FC" --version 2>&1 | head -1)?"
en_spec=false; trozo=""
if [ -r "$SPEC" ] && grep -qi 'pmem' "$SPEC"; then
  en_spec=true; trozo=$(grep -i -n -A12 'pmem' "$SPEC" | head -60)
fi
head -c $(( 4 << 20 )) /dev/zero > "$FC_W/prueba.img"
fc_lanzar sonda || exit 1
cuerpo="{\"id\":\"p0\",\"path_on_host\":\"$FC_W/prueba.img\",\"root_device\":false,\"read_only\":true}"
api_ok=false
fc_api "$FC_SOCK" PUT /pmem/p0 "$cuerpo" 2> "$FC_W/sonda.err" && api_ok=true
respuesta=$(cat "$FC_W/sonda.err" 2>/dev/null)
fc_parar
e1=$(jq -nc --argjson s "$en_spec" --argjson a "$api_ok" --arg t "$trozo" --arg r "$respuesta" \
  '{etapa1:{en_especificacion:$s, put_pmem_2xx:$a, especificacion:$t, respuesta:$r}}')
[ "$api_ok" = true ] || fin "descartar: este Firecracker no acepta PUT /pmem (ni la especificación lo nombra: $en_spec); para la ronda 10 haría falta otra versión de Firecracker" "$e1"
[ "$COMPROBAR" = 1 ] && fin "etapa 1 superada: Firecracker acepta virtio-pmem (--comprobar no compila)" "$e1"

# ── etapa 2 ──────────────────────────────────────────────────────────────────
if [ -z "${VMLINUX_PMEM:-}" ]; then
  KBLD=${KERNEL_BUILDER:-$KB/builders/kernel}
  [ -x "$KBLD/build.sh" ] && [ -r "$KBLD/../../check-kernel-config.sh" ] \
    || fin "sin cifra: falta el constructor de kernel ($KBLD y check-kernel-config.sh dos niveles arriba)" "$e1"
  fc_paso "etapa 2: kernel de prueba con VIRTIO_PMEM y FS_DAX (copia aparte del constructor)"
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev >/dev/null 2>&1
  K=$FC_W/k/scripts/builders/kernel
  mkdir -p "$K"; cp -a "$KBLD/." "$K/"; cp "$KBLD/../../check-kernel-config.sh" "$FC_W/k/scripts/"
  cp "$AQUI/config-amd64-pmem" "$K/config-amd64-pmem"
  t0=$(fc_ms)
  if ! VARIANT=pmem OUT="$FC_W/kout" bash "$K/build.sh" > "$OUT/kernel-build.log" 2>&1; then
    fin "sin cifra: el kernel de prueba no compila (ver kernel-build.log)" "$e1"
  fi
  VMLINUX_PMEM=$(find "$FC_W/kout" -name 'vmlinux-*-pmem' -type f | head -1)
  grep -E 'CONFIG_(VIRTIO_PMEM|FS_DAX|LIBNVDIMM|ZONE_DEVICE)=' "$VMLINUX_PMEM.config" > "$OUT/kernel-pmem.config" 2>/dev/null
  e1=$(jq -c --argjson ms "$(( $(fc_ms) - t0 ))" '. + {etapa2:{kernel_ms:$ms}}' <<<"$e1")
fi
[ -r "$VMLINUX_PMEM" ] || fin "sin cifra: no hay kernel de prueba" "$e1"
grep -q 'CONFIG_VIRTIO_PMEM=y' "$VMLINUX_PMEM.config" 2>/dev/null || [ -n "${VMLINUX_PMEM_SIN_CONFIG:-}" ] \
  || fin "sin cifra: el kernel de prueba no lleva CONFIG_VIRTIO_PMEM=y (dependencias no satisfechas: kernel-build.log)" "$e1"

# ── etapa 3 ──────────────────────────────────────────────────────────────────
fc_paso "etapa 3: pesos en RAM frente a pesos por pmem+dax"
origen=$(fc_datos "$FC_W/datos.img")
# El ext4 con los pesos, tamaño múltiplo de 2 MiB (lo que suelen pedir los dispositivos pmem).
mkdir -p "$FC_W/pdir"; cp "$FC_W/datos.img" "$FC_W/pdir/pesos"
tam=$(( (DATOS_MIB + 64 + 1) / 2 * 2 ))
mkfs.ext4 -q -F -b 4096 -d "$FC_W/pdir" "$FC_W/pesos.ext4" "${tam}M" >/dev/null || fin "sin cifra: mkfs del ext4 de pesos" "$e1"
rm -rf "$FC_W/pdir"
VMLINUX_RAM=$VMLINUX
: > "$FC_W/muestras.jsonl"
orden="ram pmem"
for v in $(seq "$VUELTAS"); do
  for var in $orden; do
    if [ "$var" = pmem ]; then
      VMLINUX=$VMLINUX_PMEM CARGA_INIT=":" PESOS_INVITADO=/mnt/pesos fc_rootfs "$FC_W" "mount -o ro,dax=always /dev/pmem0 /mnt || mount -o ro,dax /dev/pmem0 /mnt" || continue
      extra=("{\"ruta\":\"/pmem/pesos\",\"cuerpo\":{\"id\":\"pesos\",\"path_on_host\":\"$FC_W/pesos.ext4\",\"root_device\":false,\"read_only\":true}}")
      VMLINUX=$VMLINUX_PMEM
    else
      fc_rootfs "$FC_W" || continue; extra=(); VMLINUX=$VMLINUX_RAM
    fi
    fc_arrancar "$var-$v" ${extra[@]+"${extra[@]}"} || { cp "$FC_LOG" "$OUT/consola-$var-$v.log"; fc_parar; continue; }
    vol=$(fc_snapshot "$FC_W/mem-$var.file" "$FC_W/snap-$var.file") || { fc_parar; continue; }
    fc_parar
    asig=$(( $(du -k "$FC_W/mem-$var.file" | cut -f1) * 1024 ))
    cat "$FC_W/mem-$var.file" > /dev/null
    load=$(fc_restaurar "$var-$v-r" "$FC_W/mem-$var.file" "$FC_W/snap-$var.file") || { cp "$FC_LOG" "$OUT/consola-$var-$v-r.log"; fc_parar; continue; }
    t1=$(fc_tocar 1) || { fc_parar; continue; }
    t2=$(fc_tocar 2) || { fc_parar; continue; }
    cp "$FC_LOG" "$OUT/consola-$var-$v-r.log"
    fc_parar
    jq -nc --arg var "$var" --argjson v "$v" --argjson vol "$vol" --argjson a "$asig" --argjson l "$load" --argjson t1 "$t1" --argjson t2 "$t2" \
      '{variante:$var, vuelta:$v, volcado:$vol, mem_file_asignado_bytes:$a, load_ms:$l, primera:$t1, segunda:$t2}' >> "$FC_W/muestras.jsonl"
  done
  orden="${orden#* } ${orden%% *}"
done

python3 -I - "$FC_W/muestras.jsonl" "$OUT/resultado.json" "$origen" "$e1" "$("$FC" --version 2>&1 | head -1)" <<'PY'
import json, sys
f, out, origen, e1, fc = sys.argv[1:6]
ms = [json.loads(l) for l in open(f) if l.strip()]
def p50(xs):
    xs = sorted(x for x in xs if isinstance(x, (int, float)))
    return xs[(len(xs) - 1) // 2] if xs else None
res = {"spike": "r9-pmem-dax", "firecracker": fc, "datos": origen, "muestras": ms, "variantes": {}}
res.update(json.loads(e1))
for var in ("ram", "pmem"):
    m = [x for x in ms if x["variante"] == var]
    res["variantes"][var] = {"n": len(m),
                             "volcado_ms_p50": p50([x["volcado"]["ms"] for x in m]),
                             "volcado_escritos_mib_p50": (p50([x["volcado"]["escritos_bytes"] for x in m]) or 0) >> 20,
                             "mem_file_mib_p50": (p50([x["mem_file_asignado_bytes"] for x in m]) or 0) >> 20,
                             "primera_invitado_ms_p50": p50([x["primera"]["invitado_ms"] for x in m]),
                             "peticion_ms_p50": p50([x["primera"]["peticion_ms"] for x in m]),
                             "salidas_vm_p50": p50([x["primera"]["salidas_vm"] for x in m])}
r, p = res["variantes"]["ram"], res["variantes"]["pmem"]
if not p["n"]:
    res["recomendacion"] = "sin cifra: la variante pmem no arrancó o no restauró (consola-pmem-*.log): ¿snapshot con pmem admitido?"
elif not r["n"]:
    res["recomendacion"] = "sin cifra: la variante de referencia no se midió"
elif p["volcado_escritos_mib_p50"] <= r["volcado_escritos_mib_p50"] / 3:
    res["recomendacion"] = ("seguir: el volcado escribe %s → %s MiB y la primera pasada %s → %s ms; ronda 10: kernel "
                            "de invitado nuevo (invalida dorados) y llama.cpp con mmap" % (
                                r["volcado_escritos_mib_p50"], p["volcado_escritos_mib_p50"],
                                r["primera_invitado_ms_p50"], p["primera_invitado_ms_p50"]))
else:
    res["recomendacion"] = "descartar: el volcado con pmem no baja ni a un tercio (%s → %s MiB)" % (
        r["volcado_escritos_mib_p50"], p["volcado_escritos_mib_p50"])
json.dump(res, open(out, "w"), indent=1, ensure_ascii=False)
print(res["recomendacion"])
PY
