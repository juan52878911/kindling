#!/bin/bash
# fase0.sh — la fase 0 del "teléfono Android sobre kindling", en el Mac (vz).
#
#   ./fase0.sh -kernel ~/Downloads/vmlinux-6.1.140-kindling-arm64-android \
#              -bundle ~/Downloads/android-fase0-YYYYMMDD-HHMMSS.tar
#   ./fase0.sh ... -apk ~/mi.apk -clones 3 -mem 3G -keep
#   ./fase0.sh --self-test          # solo la lógica de informe, sin kling (vale en Linux)
#
# Solo usa el CLI `kling` y herramientas de serie de macOS (perl, shasum, curl,
# tar, sysctl, vm_stat, ps). Corre como tu usuario, sin sudo.
#
# QUÉ HACE, en orden (cada paso deja datos crudos en results/<fecha>/):
#   0. comprobaciones: Apple Silicon, macOS 14+, kling y kling-vz, e2fsprogs,
#      el kernel es un Image arm64, sha256 del paquete, memoria del Mac;
#   1. arranca un daemon PRIVADO (raíz y socket propios, ~/.kindling-android-fase0)
#      con el kernel Android y la imagen del paquete. Tu daemon de siempre y sus
#      dorados no se tocan: el kernel es uno por daemon (images/vmlinux) y un
#      dorado se niega a restaurar sobre otro kernel (K2, internal/machine/snapshot.go);
#   2. arranque en frío (-cpus 2 -mem 3G -egress none -allow-exec) y tiempo hasta
#      sys.boot_completed=1, sondeando getprop por `kling exec`;
#   3. latencias del dump de la UI (uidump si la imagen lo trae, si no
#      `uiautomator dump`) y de `screencap -p` (5 de cada), con las salidas;
#   4. instala un APK arm64 (por defecto Termux, ver APK_*), lo abre y comprueba
#      que su actividad queda en primer plano;
#   5. memoria de la VM en frío; `kling save` del dorado;
#   6. N clones con `run -from` (en serie: la compuerta de vz es 4 arranques a la
#      vez), midiendo restaurar → primer dump correcto y la memoria de cada uno;
#      android_id de cada clon (identidad compartida); dump/screencap en un clon;
#      `kling machine squeeze` en un clon;
#   7. `kling sandbox create -from` + `kling sandbox fork -n 1`;
#   8. limpia (salvo -keep) y escribe results/<fecha>/resultados.md con los
#      criterios go/no-go de la propuesta, ajustados al Mac.
#
# MEMORIA EN macOS. No hay PSS (no hay /proc/<pid>/smaps). Lo que se mide es
# phys_footprint: memoria sucia + comprimida atribuida al proceso. Es lo que da
# `kling top -json` en macOS (campo pss_mib: en vz es la suma del footprint de
# kling-vz y del proceso auxiliar com.apple.Virtualization.VirtualMachine que
# aloja la VM, docs/backend-vz.md §3). En vz una VM restaurada no comparte
# páginas con el dorado: el framework copia el estado entero a memoria al
# restaurar (docs/vz-mac-prototipo.md), así que PSS y footprint dirían lo mismo.
# Aparte se guardan `ps` (RSS), `vm_stat`, `footprint` (si existe) y
# kern.memorystatus_level para ver la presión del Mac entero.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# ── parámetros ───────────────────────────────────────────────────────────────
KERNEL=""
BUNDLE=""
APK=""
APK_PKG=""                 # si se conoce; si no, se deduce de pm list packages
CLONES=5
MEM_MIB=1536               # Android 13 en reposo usa ~750 MiB; restaurar en vz escala con
                           # la RAM CONFIGURADA (1 GiB 350 ms, 1,5 GiB 550 ms, 3 GiB 750 ms)
CPUS=2
CPU_PCT=""                 # techo de CPU (% de un núcleo); vacío = CPUS*100 (sin techo)
BOOT_TIMEOUT=300           # s hasta sys.boot_completed=1 (el criterio es 120)
RESTORE_TIMEOUT=90         # s de restaurar a dump correcto (el criterio es 3)
REPS=5
KEEP=0
FORCE=0
PRIV_ROOT="${PRIV_ROOT:-$HOME/.kindling-android-fase0}"
OUT=""
KLING="${KLING:-kling}"
GOLDEN="android13-fase0"
P="a0"                     # prefijo de las máquinas de esta prueba

# APK por defecto: Termux, arm64-v8a, de su release oficial en GitHub. Lleva
# código nativo (lib/arm64-v8a/libtermux.so y libtermux-bootstrap.so): si abre
# y extrae su bootstrap, el ARM64 nativo funciona. sha256 calculado el
# 2026-09-27 sobre el fichero descargado de esta URL (no está firmado aparte).
APK_URL="https://github.com/termux/termux-app/releases/download/v0.118.3/termux-app_v0.118.3+github-debug_arm64-v8a.apk"
APK_SHA256="72fdb596045116bf5ba1b5bdf5b26fddb9acc0bd074ad9f2da9eb0ae85e83a4e"
APK_DEFAULT_PKG="com.termux"

usage() {
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  cat <<EOF

Opciones:
  -kernel PATH       kernel de prototypes/android/kernel (obligatorio)
  -bundle PATH.tar   paquete de image/build-image.sh (obligatorio)
  -apk PATH          APK arm64 a probar (por defecto descarga Termux y verifica su sha256)
  -apk-pkg NOMBRE    su paquete, si se sabe
  -clones N          clones desde el dorado (por defecto $CLONES)
  -mem 3G|3072       memoria de la VM (por defecto $MEM_MIB MiB)
  -cpus N            vCPUs (por defecto $CPUS)
  -cpu-pct PCT       techo de CPU en % de un núcleo; por defecto CPUS*100 (sin techo).
                     Con el 50 % de kindling Android tarda 18,5 s en frío en vez de
                     4,3 s. Los clones lo heredan del dorado
  -boot-timeout S    espera máxima a boot_completed (por defecto $BOOT_TIMEOUT)
  -root DIR          raíz del daemon privado (por defecto $PRIV_ROOT)
  -out DIR           dónde dejar los resultados (por defecto results/<fecha>)
  -force             no recortar clones aunque la memoria del Mac no dé
  -keep              no borrar máquinas ni el dorado al terminar
EOF
  exit 2
}

a_mib() { # 3G, 3072M, 3072 -> MiB
  case "$1" in
    *[Gg]) echo $(( ${1%[Gg]} * 1024 )) ;;
    *[Mm]) echo "${1%[Mm]}" ;;
    *) echo "$1" ;;
  esac
}

# ── utilidades (bash 3.2 de macOS: sin arrays asociativos ni mapfile) ────────
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", b - a }'; }
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*"; FATAL="$*"; exit 1; }

# Métricas: una por línea, "clave valor", en $OUT/metrics.txt. m CLAVE la lee.
put() { echo "$1 $2" >>"$OUT/metrics.txt"; }
m() {
  [ -f "$OUT/metrics.txt" ] || return 0
  awk -v k="$1" '$1 == k { v = $2 } END { if (v != "") print v }' "$OUT/metrics.txt"
}
# Series: "serie valor" en $OUT/series.txt; mediana y máximo con sort/awk.
put_s() { [ -n "${2:-}" ] && echo "$1 $2" >>"$OUT/series.txt"; return 0; }
stat_s() { # stat_s SERIE p50|max|min|n
  [ -f "$OUT/series.txt" ] || return 0
  awk -v k="$1" '$1 == k { print $2 }' "$OUT/series.txt" | sort -n | awk -v q="$2" '
    { v[NR] = $1 }
    END {
      if (NR == 0) exit
      if (q == "n") { print NR; exit }
      if (q == "max") { print v[NR]; exit }
      if (q == "min") { print v[1]; exit }
      if (NR % 2) print v[(NR + 1) / 2]; else printf "%.3f\n", (v[NR / 2] + v[NR / 2 + 1]) / 2
    }'
}

k() { "$KLING" "$@"; }
# ax MÁQUINA TIMEOUT_S CMD... : comando dentro de Android (android-sh, en la imagen).
ax() { local mq="$1" t="$2"; shift 2; "$KLING" exec -timeout "${t}s" "$mq" -- android-sh "$@"; }
# dump_ui MÁQUINA TIMEOUT: la jerarquía de la UI en XML por la salida. Con el
# servidor residente de la imagen (uidump, ~20 ms) si lo trae; si no, con
# `uiautomator dump` (~1,9 s: arranca una JVM y espera 1 s de pantalla quieta).
# Se decide una vez (DUMPER) en la máquina en frío.
DUMPER=""
dump_ui() {
  local mq="$1" t="$2"
  if [ -z "$DUMPER" ]; then
    if "$KLING" exec -timeout 10s "$mq" -- sh -c 'command -v uidump' >/dev/null 2>&1; then DUMPER=uidump; else DUMPER=uiautomator; fi
    put dumper "$DUMPER"
  fi
  if [ "$DUMPER" = uidump ]; then
    "$KLING" exec -timeout "${t}s" "$mq" -- uidump
  else
    ax "$mq" "$t" 'uiautomator dump /data/local/tmp/ui.xml >/dev/null 2>&1 && cat /data/local/tmp/ui.xml'
  fi
}

# Lectores de JSON con JSON::PP (viene con el perl de macOS).
json_mem_of() { # json_mem_of FICHERO NOMBRE -> pss_mib (footprint en vz)
  perl -MJSON::PP -e '
    local $/; open my $f, "<", $ARGV[0] or exit; my $d = decode_json(<$f>);
    for my $mc (@{ $d->{machines} || [] }) { if ($mc->{name} eq $ARGV[1]) { print $mc->{pss_mib}, "\n"; exit } }' "$1" "$2"
}
json_names_prefix() { # json_names_prefix FICHERO(ps -json) PREFIJO
  perl -MJSON::PP -e '
    local $/; open my $f, "<", $ARGV[0] or exit; my $d = decode_json(<$f>);
    for my $mc (@$d) { print $mc->{name}, "\n" if index($mc->{name}, $ARGV[1]) == 0 }' "$1" "$2"
}
json_fork_names() { # nombres de las copias en la salida de sandbox fork -json
  perl -MJSON::PP -e '
    local $/; open my $f, "<", $ARGV[0] or exit; my $d = decode_json(<$f>);
    print $_->{name}, "\n" for @{ $d->{sandboxes} || [] }' "$1"
}

es_png() { [ -s "$1" ] && [ "$(head -c 8 "$1" | od -An -tx1 | tr -d ' \n')" = "89504e470d0a1a0a" ]; }
es_dump() { [ -s "$1" ] && grep -q '<hierarchy' "$1"; }

# ── informe ──────────────────────────────────────────────────────────────────
veredicto() { # veredicto VALOR UMBRAL  -> PASA/FALLA/SIN DATO (VALOR <= UMBRAL)
  if [ -z "$1" ]; then echo "SIN DATO"; return; fi
  awk -v v="$1" -v u="$2" 'BEGIN { print (v + 0 <= u + 0) ? "PASA" : "FALLA" }'
}

informe() {
  local f="$OUT/resultados.md"
  local boot dump_p50 dump_max cap_p50 cap_max cdump_p50 ccap_p50 rd_p50 rd_max
  local fp_cold fp_clone_p50 marg_p50 nclones apk fork_ok fork_s ids
  boot="$(m boot_completed_s)"
  dump_p50="$(stat_s dump_cold p50)"; dump_max="$(stat_s dump_cold max)"
  cap_p50="$(stat_s screencap_cold p50)"; cap_max="$(stat_s screencap_cold max)"
  cdump_p50="$(stat_s dump_clone p50)"; ccap_p50="$(stat_s screencap_clone p50)"
  rd_p50="$(stat_s restore_to_dump p50)"; rd_max="$(stat_s restore_to_dump max)"
  fp_cold="$(m footprint_cold_mib)"
  fp_clone_p50="$(stat_s footprint_clone p50)"
  marg_p50="$(stat_s marginal_clone p50)"
  nclones="$(stat_s restore_to_dump n)"
  apk="$(m apk_opens)"
  fork_ok="$(m fork_ok)"; fork_s="$(m fork_s)"
  ids="$(m android_id_distinct)"

  local ram_mib fit
  ram_mib="$(m host_ram_mib)"
  fit=""
  if [ -n "$marg_p50" ] && [ -n "$ram_mib" ]; then
    fit="$(awk -v r="$ram_mib" -v c="$marg_p50" 'BEGIN { if (c > 0) printf "%d", (r * 0.7) / c }')"
  fi

  {
    echo "# Fase 0 — Android (Redroid 13 arm64) sobre kindling vz"
    echo
    echo "Fecha: $(m fecha) · Mac: $(m host_model) · macOS $(m host_macos) · RAM $(m host_ram_mib) MiB · kling $(m kling_version)"
    echo "VM: $(m vm_cpus) vCPU, $(m vm_mem_mib) MiB, techo de CPU $(m vm_cpu_pct) %, egress none · kernel sha256 $(m kernel_sha256) · clones pedidos/hechos: $(m clones_pedidos)/${nclones:-0}"
    [ -n "${FATAL:-}" ] && { echo; echo "**La prueba se cortó:** ${FATAL}"; }
    [ -n "$(m nota_clones)" ] && { echo; echo "Nota: $(m nota_clones | tr '_' ' ')"; }
    echo
    echo "## Criterios go/no-go (propuesta §4, ajustados al Mac)"
    echo
    echo "| # | Criterio | Umbral | Medido | Veredicto |"
    echo "|---|---|---|---|---|"
    echo "| 1 | \`sys.boot_completed=1\` en frío | ≤ 120 s | ${boot:-—} s | $(veredicto "$boot" 120) |"
    echo "| 2 | restaurar (\`run -from\`) → primer dump de la UI correcto ($(m dumper)) | ≤ 3 s (p50) | p50 ${rd_p50:-—} s, máx ${rd_max:-—} s (n=${nclones:-0}) | $(veredicto "$rd_p50" 3) |"
    echo "| 3 | memoria por clon extra | informativo: referencia ~350 MiB/VM restaurada (VM de 256 MiB) | footprint p50 ${fp_clone_p50:-—} MiB; marginal p50 ${marg_p50:-—} MiB; VM en frío ${fp_cold:-—} MiB | ver nota |"
    echo "| 4a | dump de la UI ($(m dumper)) | ≤ 2 s (p50) | frío p50 ${dump_p50:-—} s (máx ${dump_max:-—}); clon p50 ${cdump_p50:-—} s | $(veredicto "$dump_p50" 2) |"
    echo "| 4b | \`screencap -p\` | ≤ 2 s (p50) | frío p50 ${cap_p50:-—} s (máx ${cap_max:-—}); clon p50 ${ccap_p50:-—} s | $(veredicto "$cap_p50" 2) |"
    local apk_v="SIN DATO"
    case "$apk" in si) apk_v="PASA" ;; no) apk_v="FALLA" ;; esac
    echo "| 5 | APK arm64 nativo abre ($(m apk_pkg)) | actividad en primer plano | ${apk:-—} | $apk_v |"
    local fork_v="SIN DATO"
    case "$fork_ok" in si) fork_v="PASA" ;; no) fork_v="FALLA" ;; saltado*) fork_v="SALTADO" ;; esac
    echo "| 6 | \`kling sandbox fork\` en vz | la copia contesta a dump | ${fork_ok:-—} ${fork_s:+(${fork_s} s)} | $fork_v |"
    echo
    echo "Nota sobre (3). La cifra de ~350 MiB por VM restaurada es de una VM de 256 MiB"
    echo "(docs/vz-mac-prototipo.md): en vz restaurar copia TODA la RAM del invitado a memoria"
    echo "(128 MiB → 267, 256 → 397), así que para una VM de $(m vm_mem_mib) MiB lo esperable es del"
    echo "orden de la RAM configurada más ~100 MiB, menos lo que el compresor de macOS gane con"
    echo "las páginas a cero. Con el marginal medido, en el 70 % de la RAM de este Mac cabrían"
    echo "**${fit:-?} teléfonos** a la vez. Tras \`kling machine squeeze\` un clon quedó en"
    echo "$(m footprint_squeezed_mib) MiB (antes $(m footprint_presqueeze_mib)); dump tras apretar: $(m dump_after_squeeze)."
    echo
    echo "## Otros datos"
    echo
    echo "- Arranque: VM creada en $(m run_cold_ms) ms (kling run); agente respondiendo a los $(m agent_s) s; init de Android vivo a los $(m android_init_s) s; boot_completed a los ${boot:-—} s."
    echo "- \`kling save\` del dorado: $(m save_s) s; tamaño en disco del dorado: $(m golden_disk_mib) MiB."
    echo "- \`kling run -from\` (lo que dice kling, sin esperar a Android): p50 $(stat_s run_from_ms p50) ms."
    echo "- Pausado en RAM: \`kling thaw\` p50 $(stat_s thaw_s p50) s; de thaw a un \`screencap\` correcto p50 $(stat_s thaw_to_screencap p50) s (n=$(stat_s thaw_s n))."
    echo "- android_id distintos entre clones: ${ids:-—} de ${nclones:-0} (1 = todos iguales: la identidad viaja en el dorado; ver propuesta §1.1 punto 5)."
    echo "- Instalar el APK: $(m apk_install_s) s; \`am start -W\`: $(m apk_start_ms) ms; libs nativas: $(m apk_native_abi)."
    echo "- ABIs del sistema: $(m abilist); Android $(m android_release); kernel del invitado: $(m guest_uname)."
    echo "- Comprobador de kernel dentro del invitado: $(m guest_kernel_check)."
    echo "- Presión de memoria del Mac al final de los clones: kern.memorystatus_level=$(m memlevel_clones) (el daemon deja de admitir por debajo de 15)."
    echo
    echo "## Ficheros"
    echo
    echo "- \`metrics.txt\`, \`series.txt\`: todas las cifras en crudo (s y MiB)."
    echo "- \`cold/\`, \`clone/\`: dumps XML y capturas PNG; \`apk/\`: instalación y apertura."
    echo "- \`raw/\`: kling top/ps -json, vm_stat, sysctl, ps, footprint, logs del invitado."
    echo "- \`fase0.log\`: la salida de esta ejecución."
  } >"$f"
  log "informe: $f"
}

# ── autocomprobación (sin kling; vale en Linux) ──────────────────────────────
if [ "${1:-}" = "--self-test" ]; then
  OUT="$(mktemp -d)"
  put fecha 2026-09-27; put vm_mem_mib 3072; put host_ram_mib 16384; put clones_pedidos 5
  put boot_completed_s 97.5; put apk_opens si; put apk_pkg com.termux; put fork_ok si; put fork_s 12.1
  put footprint_cold_mib 2900; put android_id_distinct 1
  for v in 1.2 0.9 1.1 3.5 1.0; do put_s dump_cold "$v"; done
  for v in 0.5 0.6 0.4 0.7; do put_s screencap_cold "$v"; done
  for v in 2.1 2.9 3.4; do put_s restore_to_dump "$v"; done
  for v in 3100 3150 3120; do put_s footprint_clone "$v"; put_s marginal_clone "$v"; done
  informe >/dev/null
  fallo=0
  chk() { if grep -qF -- "$1" "$OUT/resultados.md"; then echo "ok   $1"; else echo "FAIL $1"; fallo=1; fi; }
  igual() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: '$2' != '$3'"; fallo=1; fi; }
  igual "mediana impar" "$(stat_s dump_cold p50)" 1.1
  igual "mediana par" "$(stat_s screencap_cold p50)" 0.550
  igual "máximo" "$(stat_s restore_to_dump max)" 3.4
  igual "n" "$(stat_s restore_to_dump n)" 3
  igual "a_mib G" "$(a_mib 3G)" 3072
  igual "a_mib M" "$(a_mib 2048M)" 2048
  igual "a_mib" "$(a_mib 1024)" 1024
  igual "veredicto" "$(veredicto 2.0 2)" PASA
  igual "veredicto vacío" "$(veredicto "" 2)" "SIN DATO"
  put_s vacia ""
  igual "put_s ignora vacíos" "$(stat_s vacia n)" ""
  chk "| 97.5 s | PASA |"
  chk "p50 2.9 s, máx 3.4 s (n=3) | PASA |"
  chk "frío p50 1.1 s (máx 3.5)"
  chk "| si | PASA |"
  chk "**3 teléfonos**"
  put boot_completed_s 130
  informe >/dev/null
  chk "| 130 s | FALLA |"
  rm -rf "$OUT"
  if [ "$fallo" != 0 ]; then echo "self-test: FAIL"; exit 1; fi
  echo "self-test: ok"
  exit 0
fi

while [ $# -gt 0 ]; do
  case "$1" in
    -kernel) KERNEL="$2"; shift 2 ;;
    -bundle) BUNDLE="$2"; shift 2 ;;
    -apk) APK="$2"; shift 2 ;;
    -apk-pkg) APK_PKG="$2"; shift 2 ;;
    -clones) CLONES="$2"; shift 2 ;;
    -mem) MEM_MIB="$(a_mib "$2")"; shift 2 ;;
    -cpus) CPUS="$2"; shift 2 ;;
    -cpu-pct) CPU_PCT="$2"; shift 2 ;;
    -boot-timeout) BOOT_TIMEOUT="$2"; shift 2 ;;
    -root) PRIV_ROOT="$2"; shift 2 ;;
    -out) OUT="$2"; shift 2 ;;
    -force) FORCE=1; shift ;;
    -keep) KEEP=1; shift ;;
    -h|--help) usage ;;
    *) echo "opción desconocida: $1" >&2; usage ;;
  esac
done
if [ -z "$KERNEL" ] || [ -z "$BUNDLE" ]; then usage; fi
for n in "$CLONES" "$MEM_MIB" "$CPUS" "$BOOT_TIMEOUT" ${CPU_PCT:+"$CPU_PCT"}; do
  case "$n" in ''|*[!0-9]*) echo "not a number: $n" >&2; exit 2 ;; esac
done

STAMP="$(date +%Y%m%d-%H%M%S)"
OUT="${OUT:-$HERE/results/$STAMP}"
mkdir -p "$OUT/raw" "$OUT/cold" "$OUT/clone" "$OUT/apk"
OUT="$(cd "$OUT" && pwd)"
exec > >(tee -a "$OUT/fase0.log") 2>&1
FATAL=""
DAEMON_PID=""
SOCK="$PRIV_ROOT/kling.sock"
export KLING_HOST="unix://$SOCK"
put fecha "$(date +%Y-%m-%dT%H:%M:%S%z)"
put vm_mem_mib "$MEM_MIB"; put vm_cpus "$CPUS"; put clones_pedidos "$CLONES"
CPU_PCT="${CPU_PCT:-$(( CPUS * 100 ))}"
put vm_cpu_pct "$CPU_PCT"

# ── limpieza e informe, pase lo que pase ─────────────────────────────────────
limpiar() {
  local rc=$?
  set +e
  [ -n "$FATAL" ] || [ "$rc" -eq 0 ] || FATAL="exited with code $rc (see fase0.log)"
  informe
  if [ "$KEEP" = 0 ] && [ -S "$SOCK" ]; then
    log "limpiando máquinas $P-* y el dorado $GOLDEN (usa -keep para conservarlos)"
    k ps -a -json >"$OUT/raw/ps-final.json" 2>/dev/null
    for mq in $(json_names_prefix "$OUT/raw/ps-final.json" "$P-" 2>/dev/null); do
      k rm -f "$mq" >/dev/null 2>&1
    done
    k template rm -f "$GOLDEN" >/dev/null 2>&1
  fi
  if [ -n "$DAEMON_PID" ]; then
    if [ "$KEEP" = 1 ]; then
      log "daemon privado sigue corriendo (PID $DAEMON_PID, KLING_HOST=$KLING_HOST)"
    else
      log "parando el daemon privado (PID $DAEMON_PID)"
      kill -TERM "$DAEMON_PID" 2>/dev/null
      for _ in $(seq 1 30); do kill -0 "$DAEMON_PID" 2>/dev/null || break; sleep 1; done
    fi
  fi
  log "resultados en $OUT"
}
trap limpiar EXIT
trap 'FATAL="interrupted"; exit 130' INT TERM

# ── 0. comprobaciones ────────────────────────────────────────────────────────
log "fase 0: resultados en $OUT"
[ "$(uname -s)" = Darwin ] || die "this runs on macOS (the vz backend); on Linux use --self-test only"
[ "$(uname -m)" = arm64 ] || die "Apple Silicon required (vz has no x86 backend)"
macos="$(sw_vers -productVersion)"
[ "${macos%%.*}" -ge 14 ] || die "macOS 14 or later required (save/restore of VM state), this is $macos"
put host_macos "$macos"
put host_model "$(sysctl -n hw.model 2>/dev/null || echo '?')"
ram_mib=$(( $(sysctl -n hw.memsize) / 1048576 ))
put host_ram_mib "$ram_mib"
for c in "$KLING" perl shasum curl tar od; do
  command -v "$c" >/dev/null 2>&1 || die "missing $c"
done
perl -MJSON::PP -e 1 2>/dev/null || die "perl without JSON::PP (it ships with macOS perl)"
kbin="$(command -v "$KLING")"
if [ -z "${KLING_VMM:-}" ] && [ ! -x "$(dirname "$kbin")/kling-vz" ] && ! command -v kling-vz >/dev/null 2>&1; then
  die "kling-vz not found next to $kbin nor in PATH: make vz (docs/mac.md)"
fi
debugfs_ok=0
for d in "$(command -v debugfs 2>/dev/null || true)" /opt/homebrew/opt/e2fsprogs/sbin/debugfs /usr/local/opt/e2fsprogs/sbin/debugfs; do
  [ -n "$d" ] && [ -x "$d" ] && debugfs_ok=1
done
[ "$debugfs_ok" = 1 ] || die "e2fsprogs missing: brew install e2fsprogs (docs/mac.md)"
put kling_version "$(k version 2>/dev/null | head -1 | tr ' ' '_')"

[ -f "$KERNEL" ] || die "no kernel at $KERNEL"
# Un Image arm64 lleva "ARMd" en el byte 56 (Documentation/arm64/booting.rst).
[ "$(od -An -c -j56 -N4 "$KERNEL" | tr -d ' ')" = "ARMd" ] || die "$KERNEL is not an arm64 Image (no ARMd magic)"
ksha="$(shasum -a 256 "$KERNEL" | cut -d' ' -f1)"
if [ -f "$KERNEL.sha256" ]; then
  [ "$(cut -d' ' -f1 "$KERNEL.sha256")" = "$ksha" ] || die "kernel sha256 does not match $KERNEL.sha256"
fi
put kernel_sha256 "$ksha"
[ -f "$BUNDLE" ] || die "no bundle at $BUNDLE"
if [ -f "$BUNDLE.sha256" ]; then
  [ "$(cut -d' ' -f1 "$BUNDLE.sha256")" = "$(shasum -a 256 "$BUNDLE" | cut -d' ' -f1)" ] ||
    die "bundle sha256 does not match $BUNDLE.sha256"
fi

# Memoria: en vz una VM restaurada ocupa ~ su RAM entera + ~100 MiB del
# proceso auxiliar (docs/vz-mac-prototipo.md). Lo que está libre ahora
# (libres + inactivas + especulativas + purgables) manda; jetsam mató VMs en el
# prototipo con el Mac lleno de navegadores.
pagesz="$(sysctl -n hw.pagesize)"
vm_stat >"$OUT/raw/vm_stat-inicio.txt"
libres="$(awk -F: '/Pages (free|inactive|speculative|purgeable)/ { gsub(/[ .]/, "", $2); s += $2 } END { print s + 0 }' "$OUT/raw/vm_stat-inicio.txt")"
disp_mib=$(( libres * pagesz / 1048576 ))
por_vm=$(( MEM_MIB + 128 ))
caben=$(( disp_mib * 9 / 10 / por_vm ))
put host_avail_mib_inicio "$disp_mib"
log "Mac: $ram_mib MiB, ~$disp_mib MiB disponibles ahora; peor caso por VM ~$por_vm MiB → caben ~$caben"
if [ "$caben" -lt 1 ]; then
  die "not even one ${MEM_MIB} MiB VM fits in ~$disp_mib MiB available: close apps or use -mem 2G"
fi
if [ "$caben" -lt "$CLONES" ] && [ "$FORCE" = 0 ]; then
  log "AVISO: $CLONES clones no caben en el peor caso; se hacen $caben (usa -force para insistir)"
  put nota_clones "recortado_de_${CLONES}_a_${caben}_clones_por_memoria_disponible_(${disp_mib}_MiB)"
  CLONES="$caben"
fi

# ── 1. daemon privado con el kernel y la imagen ──────────────────────────────
mkdir -p "$PRIV_ROOT/images"
if k status >/dev/null 2>&1; then
  log "ya hay un daemon en $SOCK; se reutiliza (no se parará al final)"
  k ps -json >"$OUT/raw/ps-previo.json"
  if [ -n "$(json_names_prefix "$OUT/raw/ps-previo.json" "" 2>/dev/null)" ]; then
    KEEP=1 # el daemon no es nuestro: la limpieza no debe tocar sus máquinas
    die "the private daemon at $SOCK has machines; remove them (KLING_HOST=$KLING_HOST kling ps) or use another -root"
  fi
fi

TMPB="$(mktemp -d)"
log "desempaquetando $BUNDLE"
tar -xf "$BUNDLE" -C "$TMPB"
PKGDIR="$(find "$TMPB" -maxdepth 1 -mindepth 1 -type d | head -1)"
[ -f "$PKGDIR/SHA256SUMS" ] || die "bundle without SHA256SUMS"
(cd "$PKGDIR" && shasum -a 256 -c SHA256SUMS) >"$OUT/raw/bundle-sha256.txt" 2>&1 ||
  die "bundle files do not match SHA256SUMS (see raw/bundle-sha256.txt)"
cp "$PKGDIR/BUILDINFO" "$OUT/raw/BUILDINFO.txt"
IMAGE="$(sed -n 's/^image_name=//p' "$PKGDIR/BUILDINFO")"
IMAGE="${IMAGE:-android13}"
for f in "$PKGDIR"/images/*; do
  dst="$PRIV_ROOT/images/$(basename "$f")"
  if [ -f "$dst" ] && cmp -s "$f" "$dst"; then continue; fi
  mv -f "$f" "$dst"
done
rm -rf "$TMPB"
# El kernel: uno por daemon, en images/vmlinux (internal/machine/manager.go, KernelPath).
if ! { [ -f "$PRIV_ROOT/images/vmlinux" ] && cmp -s "$KERNEL" "$PRIV_ROOT/images/vmlinux"; }; then
  if k status >/dev/null 2>&1; then
    die "the running private daemon has a different kernel; stop it first (kill its PID) and rerun"
  fi
  cp "$KERNEL" "$PRIV_ROOT/images/vmlinux"
fi
log "imagen $IMAGE y kernel instalados en $PRIV_ROOT/images"

if ! k status >/dev/null 2>&1; then
  log "arrancando el daemon privado (raíz $PRIV_ROOT)"
  # Fuera del SIGINT del terminal: Go reactiva SIGINT aunque bash lo ignore en
  # hijos asíncronos, y un Ctrl-C mataría al daemon antes de que `limpiar`
  # borre las VMs de varios GiB.
  ( trap '' INT; exec nohup "$KLING" daemon -root "$PRIV_ROOT" -socket "$SOCK" ) >"$OUT/raw/daemon.log" 2>&1 &
  DAEMON_PID=$!
  for _ in $(seq 1 60); do k status >/dev/null 2>&1 && break; sleep 0.5; done
  k status >/dev/null 2>&1 || die "the private daemon did not come up (raw/daemon.log)"
fi
k status -v >"$OUT/raw/status.txt" 2>&1 || true
k image ls >"$OUT/raw/images.txt" 2>&1 || true
grep -q "$IMAGE" "$OUT/raw/images.txt" || die "the daemon does not list image $IMAGE (raw/images.txt)"
k template rm -f "$GOLDEN" >/dev/null 2>&1 || true

# ── esperas ──────────────────────────────────────────────────────────────────
# espera_boot MÁQUINA T0 TIMEOUT: sondea hasta sys.boot_completed=1. Apunta
# cuándo contestó el agente y cuándo apareció el init de Android.
espera_boot() {
  local mq="$1" t0="$2" to="$3" t v agent="" init=""
  while :; do
    t="$(now)"
    if awk -v a="$(secs "$t0" "$t")" -v b="$to" 'BEGIN { exit !(a > b) }'; then
      return 1
    fi
    if [ -z "$agent" ] && "$KLING" exec -timeout 5s "$mq" -- true >/dev/null 2>&1; then
      agent="$(secs "$t0" "$(now)")"; put agent_s "$agent"; log "  agente a los ${agent}s"
    fi
    if [ -n "$agent" ] && [ -z "$init" ]; then
      if "$KLING" exec -timeout 5s "$mq" -- android-sh --pid >/dev/null 2>&1; then
        init="$(secs "$t0" "$(now)")"; put android_init_s "$init"; log "  init de Android a los ${init}s"
      else
        # El lanzador deja "failed: <motivo>" si falta algo que no se arregla
        # relanzando (p. ej. binderfs): no tiene sentido esperar al plazo.
        v="$("$KLING" exec -timeout 5s "$mq" -- android-sh --state 2>/dev/null || true)"
        case "$v" in failed*) log "  el lanzador se rindió: $v"; FATAL="launcher: $v"; return 1 ;; esac
      fi
    fi
    if [ -n "$init" ]; then
      v="$(ax "$mq" 10 getprop sys.boot_completed 2>/dev/null | tr -d '\r\n ' || true)"
      if [ "$v" = 1 ]; then
        put boot_completed_s "$(secs "$t0" "$(now)")"
        return 0
      fi
    fi
    sleep 1
  done
}

# espera_dump MÁQUINA T0 TIMEOUT FICHERO: el primer dump correcto.
espera_dump() {
  local mq="$1" t0="$2" to="$3" f="$4"
  while awk -v a="$(secs "$t0" "$(now)")" -v b="$to" 'BEGIN { exit !(a <= b) }'; do
    if dump_ui "$mq" 20 >"$f" 2>/dev/null \
       && es_dump "$f"; then
      secs "$t0" "$(now)"
      return 0
    fi
    sleep 0.2
  done
  return 1
}

diagnostico() { # qué pasó dentro, para cuando algo no llega
  local mq="$1" d="$OUT/raw/diag-$1"
  mkdir -p "$d"
  k logs "$mq" >"$d/consola.txt" 2>&1 || true
  "$KLING" exec -timeout 20s "$mq" -- tail -n 300 /var/log/service.log >"$d/service.log" 2>&1 || true
  "$KLING" exec -timeout 20s "$mq" -- android-sh --state >"$d/state.txt" 2>&1 || true
  "$KLING" exec -timeout 20s "$mq" -- sh -c 'dmesg | tail -n 300' >"$d/dmesg.txt" 2>&1 || true
  ax "$mq" 30 'logcat -d -b all | tail -n 2000' >"$d/logcat.txt" 2>&1 || true
  ax "$mq" 20 'getprop' >"$d/getprop.txt" 2>&1 || true
  ax "$mq" 20 'ps -A' >"$d/ps-android.txt" 2>&1 || true
  log "  diagnóstico en $d"
}

memoria() { # memoria ETIQUETA: foto de memoria del Mac y de cada VM
  local e="$1"
  k top -json >"$OUT/raw/top-$e.json" 2>/dev/null || true
  k ps -a -json >"$OUT/raw/ps-$e.json" 2>/dev/null || true
  vm_stat >"$OUT/raw/vm_stat-$e.txt" 2>/dev/null || true
  sysctl vm.swapusage kern.memorystatus_level >"$OUT/raw/sysctl-$e.txt" 2>/dev/null || true
  ps -axo pid,ppid,rss,vsz,comm | awk 'NR == 1 || /kling-vz|Virtualization/' >"$OUT/raw/procesos-$e.txt" 2>/dev/null || true
  if command -v footprint >/dev/null 2>&1; then
    for pid in $(ps -axo pid,comm | awk '/kling-vz|com.apple.Virtualization.VirtualMachine/ { print $1 }'); do
      footprint "$pid" 2>/dev/null | head -n 5
    done >"$OUT/raw/footprint-$e.txt" 2>&1 || true
  fi
}

mide_dump_cap() { # mide_dump_cap MÁQUINA SERIE_SUFIJO DIR
  local mq="$1" s="$2" d="$3" i t0 t1
  for i in $(seq 1 "$REPS"); do
    t0="$(now)"
    if dump_ui "$mq" 60 >"$d/dump-$i.xml" 2>/dev/null \
       && es_dump "$d/dump-$i.xml"; then
      t1="$(now)"; put_s "dump_$s" "$(secs "$t0" "$t1")"
    else
      log "  dump $i falló"; put_s "dump_fallos_$s" 1
    fi
    t0="$(now)"
    if ax "$mq" 60 'screencap -p' >"$d/screen-$i.png" 2>/dev/null && es_png "$d/screen-$i.png"; then
      t1="$(now)"; put_s "screencap_$s" "$(secs "$t0" "$t1")"
    else
      log "  screencap $i falló"; put_s "screencap_fallos_$s" 1
    fi
  done
  log "  dump p50 $(stat_s "dump_$s" p50)s · screencap p50 $(stat_s "screencap_$s" p50)s"
}

# ── 2. arranque en frío ──────────────────────────────────────────────────────
COLD="$P-cold"
log "arranque en frío de $COLD ($CPUS vCPU, $MEM_MIB MiB, techo de CPU $CPU_PCT %, egress none)"
t0="$(now)"
k run -image "$IMAGE" -name "$COLD" -cpus "$CPUS" -mem "$MEM_MIB" -cpu-pct "$CPU_PCT" -egress none -allow-exec \
  >"$OUT/raw/run-cold.txt" 2>&1 || { cat "$OUT/raw/run-cold.txt"; die "kling run failed"; }
cat "$OUT/raw/run-cold.txt"
put run_cold_ms "$(sed -n 's/.*booted cold in \([0-9]*\) ms.*/\1/p' "$OUT/raw/run-cold.txt")"
if ! espera_boot "$COLD" "$t0" "$BOOT_TIMEOUT"; then
  diagnostico "$COLD"
  die "${FATAL:-no sys.boot_completed=1 after ${BOOT_TIMEOUT}s} (raw/diag-$COLD)"
fi
log "boot_completed a los $(m boot_completed_s)s"
"$KLING" exec -timeout 20s "$COLD" -- sh -c 'zcat /proc/config.gz | /usr/local/lib/kindling-android/check-android-config.sh -' \
  >"$OUT/raw/guest-kernel-check.txt" 2>&1 || true
put guest_kernel_check "$(tail -n 1 "$OUT/raw/guest-kernel-check.txt" | tr ' ' '_')"
put guest_uname "$("$KLING" exec -timeout 10s "$COLD" -- uname -r 2>/dev/null | tr -d '\r\n' || true)"
ax "$COLD" 20 getprop >"$OUT/raw/getprop-cold.txt" 2>&1 || true
put abilist "$(sed -n 's/^\[ro.product.cpu.abilist\]: \[\(.*\)\]$/\1/p' "$OUT/raw/getprop-cold.txt")"
put android_release "$(sed -n 's/^\[ro.build.version.release\]: \[\(.*\)\]$/\1/p' "$OUT/raw/getprop-cold.txt")"
"$KLING" exec -timeout 20s "$COLD" -- tail -n 100 /var/log/service.log >"$OUT/raw/service-cold.log" 2>&1 || true

# Pantalla encendida y sin bloqueo: uiautomator necesita una ventana con foco.
# Sin animaciones: uiautomator espera a que la interfaz quede quieta, y con
# render por CPU las animaciones alargan cada dump ("could not get idle state").
ax "$COLD" 30 'svc power stayon true; settings put system screen_off_timeout 2147483647;
  locksettings set-disabled true; input keyevent KEYCODE_WAKEUP; wm dismiss-keyguard;
  settings put global window_animation_scale 0; settings put global transition_animation_scale 0;
  settings put global animator_duration_scale 0;
  input keyevent KEYCODE_HOME' >"$OUT/raw/prepara-pantalla.txt" 2>&1 || true
sleep 3

# ── 3. dump y screencap en frío ──────────────────────────────────────────────
log "dump y screencap ×$REPS en $COLD"
mide_dump_cap "$COLD" cold "$OUT/cold"

# ── 4. APK arm64 ─────────────────────────────────────────────────────────────
if [ -z "$APK" ]; then
  APK="$OUT/apk/termux-arm64.apk"
  log "descargando el APK por defecto (Termux arm64) y verificando su sha256"
  curl -fsSL --retry 3 -o "$APK" "$APK_URL" || die "APK download failed ($APK_URL); pass -apk"
  [ "$(shasum -a 256 "$APK" | cut -d' ' -f1)" = "$APK_SHA256" ] || die "APK sha256 mismatch"
  [ -n "$APK_PKG" ] || APK_PKG="$APK_DEFAULT_PKG"
fi
[ -f "$APK" ] || die "no APK at $APK"
put apk_sha256 "$(shasum -a 256 "$APK" | cut -d' ' -f1)"
log "instalando $(basename "$APK")"
ax "$COLD" 30 'pm list packages -3' 2>/dev/null | sort >"$OUT/apk/antes.txt" || true
k cp "$APK" "$COLD:/tmp/app.apk" >/dev/null
"$KLING" exec -timeout 120s "$COLD" -- android-sh --push /tmp/app.apk /data/local/tmp/app.apk
t0="$(now)"
if ax "$COLD" 300 'pm install -r -g /data/local/tmp/app.apk' >"$OUT/apk/install.txt" 2>&1; then
  put apk_install_s "$(secs "$t0" "$(now)")"
else
  log "pm install falló: $(tail -n 3 "$OUT/apk/install.txt")"
fi
ax "$COLD" 30 'pm list packages -3' 2>/dev/null | sort >"$OUT/apk/despues.txt" || true
if [ -z "$APK_PKG" ]; then
  APK_PKG="$(comm -13 "$OUT/apk/antes.txt" "$OUT/apk/despues.txt" | head -1 | sed 's/^package://' | tr -d '\r')"
fi
put apk_pkg "${APK_PKG:-?}"
put apk_opens no
if [ -n "$APK_PKG" ] && grep -q "package:$APK_PKG" "$OUT/apk/despues.txt"; then
  comp="$(ax "$COLD" 30 "cmd package resolve-activity --brief -c android.intent.category.LAUNCHER $APK_PKG" 2>/dev/null | tail -n 1 | tr -d '\r')"
  log "abriendo $comp"
  ax "$COLD" 120 "am start -W -n $comp" >"$OUT/apk/am-start.txt" 2>&1 || true
  put apk_start_ms "$(sed -n 's/^TotalTime: *//p' "$OUT/apk/am-start.txt" | tr -d '\r')"
  sleep 5
  ax "$COLD" 30 'dumpsys activity activities | grep -E "mResumedActivity|topResumedActivity|ResumedActivity"' \
    >"$OUT/apk/resumed.txt" 2>&1 || true
  if grep -q "$APK_PKG" "$OUT/apk/resumed.txt"; then put apk_opens si; fi
  apkdir="$(ax "$COLD" 30 "pm path $APK_PKG" 2>/dev/null | head -n 1 | sed 's/^package://; s#/base.apk##' | tr -d '\r')"
  ax "$COLD" 30 "ls $apkdir/lib 2>/dev/null" >"$OUT/apk/lib-abi.txt" 2>&1 || true
  put apk_native_abi "$(tr '\n' ',' <"$OUT/apk/lib-abi.txt" | sed 's/,$//')"
  ax "$COLD" 30 'logcat -d -b crash' >"$OUT/apk/crash.txt" 2>&1 || true
  dump_ui "$COLD" 60 >"$OUT/apk/dump-app.xml" 2>/dev/null || true
  ax "$COLD" 60 'screencap -p' >"$OUT/apk/screen-app.png" 2>/dev/null || true
  ax "$COLD" 20 'input keyevent KEYCODE_HOME' >/dev/null 2>&1 || true
else
  log "el paquete no aparece instalado (apk/install.txt)"
fi
log "APK $APK_PKG abre: $(m apk_opens)"

# ── 5. memoria en frío y dorado ──────────────────────────────────────────────
sleep 10
memoria cold
put footprint_cold_mib "$(json_mem_of "$OUT/raw/top-cold.json" "$COLD")"
log "footprint de $COLD: $(m footprint_cold_mib) MiB"
log "kling save $COLD → $GOLDEN"
t0="$(now)"
if ! k save -replace "$COLD" "$GOLDEN" >"$OUT/raw/save.txt" 2>&1; then
  # El cliente corto corta a los 60 s esperando cabeceras, pero el daemon
  # puede seguir guardando un dorado grande: esperarlo antes de rendirse.
  log "kling save no respondió a tiempo; esperando al dorado hasta 3 min"
  hecho=0
  for _ in $(seq 1 90); do
    if k template ls -q 2>/dev/null | grep -qx "$GOLDEN"; then hecho=1; break; fi
    sleep 2
  done
  [ "$hecho" = 1 ] || { cat "$OUT/raw/save.txt"; die "kling save failed"; }
fi
put save_s "$(secs "$t0" "$(now)")"
cat "$OUT/raw/save.txt"
[ -d "$PRIV_ROOT/snapshots/$GOLDEN" ] && put golden_disk_mib "$(( $(du -sk "$PRIV_ROOT/snapshots/$GOLDEN" | cut -f1) / 1024 ))"
k rm -f "$COLD" >/dev/null
sleep 3

# ── 6. clones desde el dorado ────────────────────────────────────────────────
memoria base
total_prev="$(perl -MJSON::PP -e 'local $/; open my $f, "<", $ARGV[0] or exit; my $d = decode_json(<$f>); print $d->{total_pss_mib} // 0' "$OUT/raw/top-base.json" 2>/dev/null || echo 0)"
for i in $(seq 1 "$CLONES"); do
  c="$P-clone-$i"
  log "clon $i/$CLONES: run -from $GOLDEN"
  t0="$(now)"
  if ! k run -from "$GOLDEN" -name "$c" >"$OUT/raw/run-$c.txt" 2>&1; then
    cat "$OUT/raw/run-$c.txt"
    log "  run -from falló; paro los clones aquí"
    put nota_clones "el_clon_${i}_no_arrancó:_$(tail -n 1 "$OUT/raw/run-$c.txt" | tr ' ' '_')"
    break
  fi
  put_s run_from_ms "$(sed -n 's/.* in \([0-9]*\) ms.*/\1/p' "$OUT/raw/run-$c.txt")"
  if rd="$(espera_dump "$c" "$t0" "$RESTORE_TIMEOUT" "$OUT/clone/primer-dump-$i.xml")"; then
    put_s restore_to_dump "$rd"
    log "  restaurar → dump: ${rd}s"
  else
    log "  sin dump correcto en ${RESTORE_TIMEOUT}s"
    diagnostico "$c"
  fi
  ax "$c" 20 'settings get secure android_id' 2>/dev/null | tr -d '\r' >>"$OUT/raw/android_ids.txt" || true
  sleep 5
  memoria "clon-$i"
  fp="$(json_mem_of "$OUT/raw/top-clon-$i.json" "$c")"
  tot="$(perl -MJSON::PP -e 'local $/; open my $f, "<", $ARGV[0] or exit; my $d = decode_json(<$f>); print $d->{total_pss_mib} // 0' "$OUT/raw/top-clon-$i.json" 2>/dev/null || echo 0)"
  [ -n "$fp" ] && put_s footprint_clone "$fp"
  put_s marginal_clone "$(( tot - total_prev ))"
  log "  footprint $fp MiB · total $tot MiB (+$(( tot - total_prev )))"
  total_prev="$tot"
done
put android_id_distinct "$(sort -u "$OUT/raw/android_ids.txt" 2>/dev/null | grep -c . || true)"
sleep 20
memoria clones
put memlevel_clones "$(sysctl -n kern.memorystatus_level 2>/dev/null || echo '?')"

if k ps -json 2>/dev/null | grep -q "\"$P-clone-1\""; then
  log "dump y screencap ×$REPS en $P-clone-1"
  mide_dump_cap "$P-clone-1" clone "$OUT/clone"
  # Pausado en RAM: en vz es el nivel que sustituye al "warm" de Firecracker
  # (restaurar copia toda la RAM; reanudar una VM pausada no copia nada).
  log "pause/thaw ×$REPS en $P-clone-1"
  for i in $(seq 1 "$REPS"); do
    k pause "$P-clone-1" >/dev/null 2>&1 || { log "  pause falló"; break; }
    sleep 1
    t0="$(now)"
    k thaw "$P-clone-1" >/dev/null 2>&1 || { log "  thaw falló"; break; }
    put_s thaw_s "$(secs "$t0" "$(now)")"
    if ax "$P-clone-1" 60 'screencap -p' >"$OUT/clone/screen-thaw-$i.png" 2>/dev/null && es_png "$OUT/clone/screen-thaw-$i.png"; then
      put_s thaw_to_screencap "$(secs "$t0" "$(now)")"
    else
      log "  screencap tras thaw $i falló"
    fi
  done
  log "  thaw p50 $(stat_s thaw_s p50)s · thaw → screencap p50 $(stat_s thaw_to_screencap p50)s"
  put footprint_presqueeze_mib "$(json_mem_of "$OUT/raw/top-clones.json" "$P-clone-1")"
  log "kling machine squeeze $P-clone-1"
  k machine squeeze "$P-clone-1" >"$OUT/raw/squeeze.txt" 2>&1 || true
  sleep 5
  memoria squeeze
  put footprint_squeezed_mib "$(json_mem_of "$OUT/raw/top-squeeze.json" "$P-clone-1")"
  if dump_ui "$P-clone-1" 60 \
       >"$OUT/clone/dump-tras-squeeze.xml" 2>/dev/null && es_dump "$OUT/clone/dump-tras-squeeze.xml"; then
    put dump_after_squeeze ok
  else
    put dump_after_squeeze falla
  fi
fi
for i in $(seq 1 "$CLONES"); do k rm -f "$P-clone-$i" >/dev/null 2>&1 || true; done
sleep 3

# ── 7. sandbox + fork ────────────────────────────────────────────────────────
# fork = Commit de la máquina viva (pausa, vuelca, reanuda) + restaurar N copias
# (internal/machine/fork.go). Con 2 VMs a la vez hacen falta ~2× la memoria.
if [ "$caben" -lt 2 ] && [ "$FORCE" = 0 ]; then
  put fork_ok "saltado_por_memoria"
else
  SB="$P-sb"
  log "sandbox $SB desde $GOLDEN y fork -n 1"
  if k sandbox create -from "$GOLDEN" -name "$SB" -ttl 30m >"$OUT/raw/sandbox.txt" 2>&1 &&
     espera_dump "$SB" "$(now)" "$RESTORE_TIMEOUT" "$OUT/clone/dump-sandbox.xml" >/dev/null; then
    t0="$(now)"
    if k sandbox fork "$SB" -n 1 -json >"$OUT/raw/fork.json" 2>"$OUT/raw/fork.err"; then
      fk="$(json_fork_names "$OUT/raw/fork.json" | head -n 1)"
      if [ -n "$fk" ] && espera_dump "$fk" "$t0" "$RESTORE_TIMEOUT" "$OUT/clone/dump-fork.xml" >/dev/null; then
        put fork_ok si; put fork_s "$(secs "$t0" "$(now)")"
        ax "$fk" 20 'settings get secure android_id' >"$OUT/raw/android_id-fork.txt" 2>&1 || true
      else
        put fork_ok no
      fi
      [ -n "$fk" ] && { k sandbox rm "$fk" >/dev/null 2>&1 || k rm -f "$fk" >/dev/null 2>&1 || true; }
    else
      put fork_ok no
      log "  fork falló: $(tail -n 2 "$OUT/raw/fork.err")"
    fi
  else
    put fork_ok no
    log "  el sandbox no llegó a contestar ($(tail -n 1 "$OUT/raw/sandbox.txt"))"
  fi
  k sandbox rm "$SB" >/dev/null 2>&1 || k rm -f "$SB" >/dev/null 2>&1 || true
fi

log "fase 0 terminada"
