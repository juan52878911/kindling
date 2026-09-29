#!/usr/bin/env bash
# Construye la imagen kindling "android13" para la fase 0: base glibc (Debian
# trixie) + kling-guest como PID 1 + el rootfs de Redroid 13 (arm64, 64 bits)
# + el lanzador. Corre en un Linux ARM64 como root (la VM Lima del Mac vale:
# NO hace falta virtualización anidada ni daemon de kindling dentro).
#
#   sudo prototypes/android/image/build-image.sh
#   sudo FETCH=docker prototypes/android/image/build-image.sh     # con Docker
#   sudo PHONED=0 prototypes/android/image/build-image.sh         # lanzador de bash (comparar)
#   sudo DATA_MODE=tmpfs prototypes/android/image/build-image.sh  # /data en RAM
#   sudo FETCH=local ROOTFS_TAR=redroid.tar ROOTFS_SHA256=<sha256> \
#        prototypes/android/image/build-image.sh                  # un `docker export` propio
#
# Deja en $OUT (por defecto /var/tmp/kindling-android/out) un tar con:
#   images/android-base.ext4          base glibc (la de 71-build-glibc-base.sh)
#   images/android13.layer.ext4       la capa con Android (81-base-image.sh)
#   images/android13.recipe.json      receta (la base de la capa sale de aquí)
#   SHA256SUMS, BUILDINFO
# que fase0.sh instala en la raíz PRIVADA de un daemon del Mac (README, paso 4).
#
# POR QUÉ NO `kling image build` + `kling image copy`. Tres razones, las tres
# son hallazgos para el núcleo (README):
#   1. El constructor "base" del daemon no acepta ROOTFS_DIR ni SERVICE por la
#      API (cmd/kling/builder.go, BaseSpec: solo packages y env); solo el
#      constructor "llm" los usa por dentro. Aquí se llama a 81-base-image.sh
#      directamente, con los mismos parámetros que usa el "llm".
#   2. `kling image copy` lleva SIEMPRE el kernel del daemon de origen
#      (pkg/api/blobs.go, CopyImage). El de Lima es el K1 normal, sin binder, y
#      pisaría el kernel Android en el destino.
#   3. Hace falta un daemon de kindling instalado en Lima (docs/mac-arm64.md,
#      pensado para M3+ con virtualización anidada). Este camino no necesita
#      daemon ni KVM: vale en cualquier Apple Silicon.
#
# QUÉ SE DESCARGA Y CÓMO SE VERIFICA. La imagen de Redroid se fija por DIGEST
# del manifiesto arm64 (no por etiqueta). Con FETCH=curl (por defecto) se habla
# con el registro directamente y se comprueba el sha256 de CADA pieza: el
# manifiesto contra REDROID_DIGEST, y la configuración y la capa contra los
# digests que el manifiesto declara. Con FETCH=docker, `docker pull
# repo@sha256:...` hace esa misma verificación por dentro.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PROTO="$(cd "$HERE/.." && pwd)"
REPO="$(cd "$PROTO/../.." && pwd)"

# ── lo fijado ────────────────────────────────────────────────────────────────
# redroid/redroid:13.0.0_64only-240527 (publicada 2024-05-28). Digests leídos
# el 2026-09-27 de la API de Docker Hub (hub.docker.com/v2/.../tags); el del
# manifiesto arm64 es el que manda. Para comprobarlo tú (con Docker):
#   docker buildx imagetools inspect redroid/redroid:13.0.0_64only-240527
# o, sin Docker:
#   curl -s 'https://hub.docker.com/v2/repositories/redroid/redroid/tags/13.0.0_64only-240527' \
#     | python3 -m json.tool | grep -B2 -A2 arm64
# "64only": sin bibliotecas de 32 bits. Los núcleos de Apple Silicon no
# ejecutan AArch32, así que la variante con 32 bits no aportaría nada.
REDROID_REPO="${REDROID_REPO:-redroid/redroid}"
REDROID_TAG="${REDROID_TAG:-13.0.0_64only-240527}"                # informativo
REDROID_INDEX_DIGEST="${REDROID_INDEX_DIGEST:-sha256:5a42a569ee1d7c71796c0385e906cbaa4c3e0a162a56d9f26b29bdb1befac13b}"  # informativo
# ── x86_64 ── ARCH=amd64 construye la imagen para Firecracker en x86_64
# (docs/proxmox.md): el manifiesto amd64 del MISMO índice, leído del registro
# el 2026-09-27 y comprobado (sha256 del manifiesto, config "amd64", una capa
# de 682 925 585 bytes).
ARCH="${ARCH:-arm64}"
case "$ARCH" in
  arm64)
    DEF_DIGEST=sha256:c815ac1b1d5bd0a099b74c3e3e0eeea3b32bed8d7ab1f788c20fb74e35891716
    # La única capa de esa imagen según Docker Hub (640 472 802 bytes comprimidos).
    DEF_LAYER=sha256:7ae0e9111b77895cc932d126b75e9bf16af5f9bb881e3dae79163621e8b10cb3
    UNAME_M=aarch64; ELF_MACHINE=b700 ;;          # EM_AARCH64
  amd64|x86_64)
    ARCH=amd64
    DEF_DIGEST=sha256:36d6d21bcf7e92d78eabaa6f1748e5cf0e9fb15176c0091794168be012c5c22e
    DEF_LAYER=sha256:2824b019a4a8a038e79392f80302a3ddca69fdfdd6acb8c37b16e1461b2a6168
    UNAME_M=x86_64; ELF_MACHINE=3e00 ;;           # EM_X86_64
  *) echo "error: ARCH must be arm64 or amd64" >&2; exit 1 ;;
esac
# ── fin x86_64 ──
REDROID_DIGEST="${REDROID_DIGEST:-$DEF_DIGEST}"
# Si se cambia REDROID_DIGEST, vaciar esto (EXPECT_LAYER= ) o poner el nuevo.
EXPECT_LAYER="${EXPECT_LAYER-$DEF_LAYER}"

IMAGE_NAME="${IMAGE_NAME:-android13}"
BASE_NAME="${BASE_NAME:-android-base}"
WORK="${WORK:-/var/tmp/kindling-android}"
OUT="${OUT:-$WORK/out}"
FETCH="${FETCH:-curl}"
GROW="${GROW:-4096}"               # MiB de techo para la capa; se encoge al final
KLING_GUEST="${KLING_GUEST:-}"     # binario linux/$ARCH de cmd/kling-guest
# kling-phoned (prototypes/android/phoned, docs/phoned.md): 1 = el agente Go
# lanza Android, sirve la API del teléfono y aplica la identidad; 0 = el camino
# de antes (android-launch.sh + android-sh + gancho de bash), para comparar.
PHONED="${PHONED:-1}"
KLING_PHONED="${KLING_PHONED:-}"   # binario linux/$ARCH; si falta y hay Go, se compila
# adb con claves (ro.adb.secure=1): las de cada clon llegan por MMDS. Solo con
# kling-phoned, que es quien las instala.
ADB_SECURE="${ADB_SECURE:-$PHONED}"
# Pantalla encendida, sin bloqueo ni animaciones tras arrancar (lo hace
# kling-phoned; phone.sh lo hacía por kling exec). La sonda de listo lo espera.
PREP="${PREP:-$PHONED}"

# Lo que el lanzador lee dentro del invitado (android.conf). Resolución fija:
# la fase 0 compara tiempos de dump/screencap y una pantalla distinta los mueve.
WIDTH="${WIDTH:-720}"; HEIGHT="${HEIGHT:-1280}"; DPI="${DPI:-320}"; FPS="${FPS:-15}"
DATA_MODE="${DATA_MODE:-overlay}"  # overlay | tmpfs  (ver android-launch.sh)
DATA_SIZE="${DATA_SIZE:-2G}"       # solo con tmpfs
ANDROID_NET="${ANDROID_NET:-veth}"      # veth | isolated | shared (ver android-launch.sh)
# adb por TCP (5555), sin contraseña: solo se ve desde el Mac por los reenvíos
# de kindling (127.0.0.1). docs/telefono.md.
# más androidboot.*/ro.* para /init. stream=1 arranca uinputd (dispositivo de
# entrada "redroid vinput"). El vncserver de Redroid (androidboot.use_redroid_vnc=1)
# ya NO va por defecto: con render por software manda la pantalla en negro
# (docs/telefono.md, "Ver la pantalla"); se puede volver a pedir con
# EXTRA_ARGS='androidboot.use_redroid_stream=1 androidboot.use_redroid_vnc=1'.
EXTRA_ARGS="${EXTRA_ARGS-androidboot.use_redroid_stream=1}"
# iptables: la red veth enruta a Android con NAT (android-launch.sh).
BASE_PKGS="${BASE_PKGS:-util-linux procps iptables}"
# dm-verity sobre la capa (image/verity.sh, docs/verity.md): 1 = árbol de
# hashes + FEC pegados a la capa y la base montándola por /dev/mapper. Necesita
# el kernel con DM_VERITY (config-android) y veritysetup aquí.
VERITY="${VERITY:-0}"
[ "$VERITY" = 1 ] && BASE_PKGS="$BASE_PKGS dmsetup"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# ── 0. comprobaciones ────────────────────────────────────────────────────────
[ "$(id -u)" -eq 0 ] || die "run it as root (it mounts loop devices and chroots)"
# (KLING_ANDROID_TEST_ANYARCH=1 solo para probar el montaje en otra arquitectura:
# la imagen resultante no arrancaría en vz.)
[ "$(uname -m)" = "$UNAME_M" ] || [ "${KLING_ANDROID_TEST_ANYARCH:-0}" = 1 ] || die "ARCH=$ARCH has to be built on Linux $UNAME_M (it chroots into the base); here: $(uname -m)"
case "$FETCH" in curl|docker|local) ;; *) die "FETCH must be curl, docker or local" ;; esac
case "$DATA_MODE" in overlay|tmpfs) ;; *) die "DATA_MODE must be overlay or tmpfs" ;; esac
case "$ANDROID_NET" in veth|isolated|shared) ;; *) die "ANDROID_NET must be veth, isolated or shared" ;; esac
[[ "$REDROID_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || die "REDROID_DIGEST must be sha256:<64 hex>"
[[ "$IMAGE_NAME" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "invalid IMAGE_NAME"
[[ "$BASE_NAME" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "invalid BASE_NAME"
case "$PHONED" in 0|1) ;; *) die "PHONED must be 0 or 1" ;; esac
[ "$ADB_SECURE" = 0 ] || [ "$PHONED" = 1 ] || die "ADB_SECURE=1 needs PHONED=1 (the bash hook does not install adb keys)"

PATH="$PATH:/usr/sbin:/sbin"
falta=()
for c in curl tar sha256sum truncate mkfs.ext4 e2fsck resize2fs mount chroot od; do
  command -v "$c" >/dev/null 2>&1 || falta+=("$c")
done
case "$FETCH" in
  curl)   command -v python3 >/dev/null 2>&1 || falta+=(python3) ;;
  docker) command -v docker >/dev/null 2>&1 || falta+=(docker) ;;
  local)  [ -f "${ROOTFS_TAR:-}" ] || die "FETCH=local needs ROOTFS_TAR=<tar of the Redroid rootfs>"
          [[ "${ROOTFS_SHA256:-}" =~ ^[0-9a-f]{64}$ ]] || die "FETCH=local needs ROOTFS_SHA256=<64 hex> of that tar" ;;
esac
[ ${#falta[@]} -eq 0 ] || die "missing: ${falta[*]}  (apt-get install debootstrap e2fsprogs curl python3)"

# Los scripts del núcleo: los instalados por `make deploy` si están, si no los
# del checkout. Se ejecutan tal cual, no se copian ni se editan.
SCRIPTS=""
for d in /usr/local/lib/kindling "$REPO/scripts"; do
  if [ -f "$d/81-base-image.sh" ] && [ -f "$d/71-build-glibc-base.sh" ] && [ -f "$d/lib-ext4-shrink.sh" ] \
     && [ -f "$d/minimal-init.sh" ]; then
    SCRIPTS="$d"; break
  fi
done
[ -n "$SCRIPTS" ] || die "can't find 81-base-image.sh / 71-build-glibc-base.sh (kindling checkout or /usr/local/lib/kindling)"

# kling-guest: tiene que ser un ELF de ARCH (e_machine 0xb7 = EM_AARCH64,
# 0x3e = EM_X86_64). El nombre se queda por no tocar las llamadas.
es_elf_arm64() {
  [ -f "$1" ] || return 1
  [ "$(head -c4 "$1" | od -An -tx1 | tr -d ' \n')" = "7f454c46" ] || return 1
  [ "$(od -An -tx1 -j18 -N2 "$1" | tr -d ' \n')" = "$ELF_MACHINE" ]
}
if [ -z "$KLING_GUEST" ]; then
  for c in "$REPO/kling-guest" /usr/local/lib/kindling/kling-guest; do
    if es_elf_arm64 "$c"; then KLING_GUEST="$c"; break; fi
  done
fi
if [ -z "$KLING_GUEST" ] || ! es_elf_arm64 "$KLING_GUEST"; then die "no linux/$ARCH kling-guest found.
  Build it (on the Mac or here, with Go):  make guest GOARCH=$ARCH   (leaves ./kling-guest)
  or pass KLING_GUEST=/path/to/kling-guest"; fi

if [ "$PHONED" = 1 ]; then
  if [ -z "$KLING_PHONED" ] && es_elf_arm64 "$REPO/kling-phoned"; then KLING_PHONED="$REPO/kling-phoned"; fi
  if [ -z "$KLING_PHONED" ] && command -v go >/dev/null 2>&1; then
    KLING_PHONED="$WORK/kling-phoned"
    mkdir -p "$WORK"
    (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w" \
      -o "$KLING_PHONED" ./prototypes/android/phoned) || die "go build of kling-phoned failed"
  fi
  es_elf_arm64 "${KLING_PHONED:-/nonexistent}" || die "no linux/$ARCH kling-phoned found.
  Build it (on the Mac or here, with Go):
    CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -o kling-phoned ./prototypes/android/phoned
  or pass KLING_PHONED=/path/to/kling-phoned (or PHONED=0 for the bash launcher)"
fi

mkdir -p "$WORK/images" "$WORK/cache/blobs" "$OUT"
STAGE="$WORK/stage-$IMAGE_NAME"
TREE="$STAGE/tree"          # lo que se copia a la raíz de la imagen (ROOTFS_DIR)
ROOTFS="$TREE/android"
limpiar() { [ "${KEEP_STAGE:-0}" = 1 ] || rm -rf "$STAGE"; }
trap limpiar EXIT
rm -rf "$STAGE"
mkdir -p "$ROOTFS"

log "scripts del núcleo: $SCRIPTS"
log "kling-guest: $KLING_GUEST ($(sha256sum "$KLING_GUEST" | cut -c1-12)…)"
[ "$PHONED" = 1 ] && log "kling-phoned: $KLING_PHONED ($(sha256sum "$KLING_PHONED" | cut -c1-12)…), adb con claves: $ADB_SECURE"
# Lo que se apunta como origen en la receta y en BUILDINFO.
if [ "$FETCH" = local ]; then
  SOURCE_REF="local:sha256:$ROOTFS_SHA256"; REDROID_TAG="local"
else
  SOURCE_REF="$REDROID_REPO@$REDROID_DIGEST"
fi
log "Redroid: $SOURCE_REF ($REDROID_TAG)"

# ── 1. rootfs de Redroid, verificado ─────────────────────────────────────────
ENTRY_JSON="$STAGE/entrypoint.json"
sha_ok() { [ -f "$1" ] && [ "sha256:$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ]; }

fetch_curl() {
  local reg="https://registry-1.docker.io/v2/$REDROID_REPO" token mf
  token="$(curl -fsSL "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$REDROID_REPO:pull" \
           | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')" \
    || die "could not get a registry token (network? Docker Hub rate limit?)"
  mf="$STAGE/manifest.json"
  curl -fsSL -H "Authorization: Bearer $token" \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    -o "$mf" "$reg/manifests/$REDROID_DIGEST" || die "could not fetch manifest $REDROID_DIGEST"
  sha_ok "$mf" "$REDROID_DIGEST" || die "manifest sha256 does not match $REDROID_DIGEST"
  log "manifiesto verificado"

  # Configuración y capas que declara el manifiesto (ya verificado).
  python3 - "$mf" >"$STAGE/blobs.txt" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
if "layers" not in m:
    sys.exit("expected a single-platform image manifest, got: %s" % m.get("mediaType"))
print("config", m["config"]["digest"], m["config"].get("size", 0))
for l in m["layers"]:
    print("layer", l["digest"], l.get("size", 0), l.get("mediaType", ""))
PY
  local nlayers
  nlayers="$(grep -c '^layer ' "$STAGE/blobs.txt")"
  # Una sola capa: sin "whiteouts" que aplicar. Con más, que lo haga Docker.
  [ "$nlayers" -eq 1 ] || die "the image has $nlayers layers; FETCH=curl only handles 1 (use FETCH=docker)"

  local kind digest size mt f
  while read -r kind digest size mt; do
    f="$WORK/cache/blobs/${digest#sha256:}"
    if ! sha_ok "$f" "$digest"; then
      log "descargando $kind $digest ($((size / 1048576)) MiB)"
      curl -fL --retry 3 -H "Authorization: Bearer $token" -o "$f.part" "$reg/blobs/$digest" \
        || die "download of $digest failed"
      mv "$f.part" "$f"
      sha_ok "$f" "$digest" || { rm -f "$f"; die "sha256 mismatch for $digest (deleted; run again)"; }
    fi
    log "  $kind $digest ok"
    if [ "$kind" = config ]; then
      cp "$f" "$STAGE/config.json"
    else
      if [ -n "$EXPECT_LAYER" ] && [ "$digest" != "$EXPECT_LAYER" ]; then
        die "layer $digest is not the pinned EXPECT_LAYER $EXPECT_LAYER"
      fi
      case "$mt" in
        *gzip*|"") tar --numeric-owner --xattrs --xattrs-include='*' -xpzf "$f" -C "$ROOTFS" ;;
        *) die "unsupported layer media type: $mt" ;;
      esac
    fi
  done <"$STAGE/blobs.txt"

  python3 - "$STAGE/config.json" "$ENTRY_JSON" "$ARCH" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
if c.get("architecture") != sys.argv[3]:
    sys.exit("image architecture is %r, expected %s" % (c.get("architecture"), sys.argv[3]))
cfg = c.get("config") or {}
json.dump({"entrypoint": cfg.get("Entrypoint") or [], "cmd": cfg.get("Cmd") or []}, open(sys.argv[2], "w"))
PY
}

fetch_docker() {
  local ref="$REDROID_REPO@$REDROID_DIGEST" cid
  docker pull --platform "linux/$ARCH" "$ref"
  [ "$(docker image inspect -f '{{.Architecture}}' "$ref")" = "$ARCH" ] || die "$ref is not $ARCH"
  docker image inspect -f '{"entrypoint": {{json .Config.Entrypoint}}, "cmd": {{json .Config.Cmd}}}' "$ref" >"$ENTRY_JSON"
  cid="$(docker create --platform "linux/$ARCH" "$ref")"
  # docker export da el sistema de ficheros ya aplanado (capas + whiteouts).
  docker export "$cid" | tar --numeric-owner --xattrs --xattrs-include='*' -xpf - -C "$ROOTFS"
  docker rm "$cid" >/dev/null
  # Docker crea estos al exportar; en la microVM los monta el lanzador.
  rm -f "$ROOTFS/.dockerenv"
}

# Un tar propio (p. ej. `docker export` hecho en otra máquina): se exige su
# sha256, como a todo lo demás. Sin configuración de imagen: /init recibe los
# argumentos por defecto de Redroid (abajo).
fetch_local() {
  [ "$(sha256sum "$ROOTFS_TAR" | cut -d' ' -f1)" = "$ROOTFS_SHA256" ] || die "ROOTFS_TAR sha256 does not match ROOTFS_SHA256"
  log "rootfs local verificado: $ROOTFS_TAR"
  tar --numeric-owner --xattrs --xattrs-include='*' -xpf "$ROOTFS_TAR" -C "$ROOTFS"
  echo '{"entrypoint": [], "cmd": []}' >"$ENTRY_JSON"
}

"fetch_$FETCH"

# Argumentos de /init: el ENTRYPOINT + CMD de la imagen menos argv[0]. Se
# guardan uno por línea; el lanzador les añade los suyos.
python3 - "$ENTRY_JSON" "$STAGE/entrypoint.args" <<'PY' 2>/dev/null || true
import json, sys
e = json.load(open(sys.argv[1]))
argv = (e.get("entrypoint") or []) + (e.get("cmd") or [])
if not argv or argv[0] != "/init":
    sys.exit("unexpected entrypoint %r" % argv)
open(sys.argv[2], "w").write("".join(a + "\n" for a in argv[1:]))
PY
if [ ! -s "$STAGE/entrypoint.args" ]; then
  # Sin python3 (FETCH=docker) o entrypoint raro: el de las imágenes publicadas
  # de Redroid. Se avisa: conviene mirarlo con docker inspect.
  echo "aviso: no pude leer el ENTRYPOINT de la imagen; uso 'qemu=1 androidboot.hardware=redroid'" >&2
  printf 'qemu=1\nandroidboot.hardware=redroid\n' >"$STAGE/entrypoint.args"
fi
log "argumentos de /init de la imagen: $(tr '\n' ' ' <"$STAGE/entrypoint.args")"

# ── 2. lo que se comprueba del rootfs ────────────────────────────────────────
# /init es un enlace ABSOLUTO a /system/bin/init en la imagen publicada (visto
# en la capa sha256:7ae0e911…): hay que resolverlo dentro del rootfs, no en
# este anfitrión.
init_real="$ROOTFS/init"
if [ -L "$init_real" ]; then
  t="$(readlink "$init_real")"
  case "$t" in /*) init_real="$ROOTFS$t" ;; *) init_real="$ROOTFS/$t" ;; esac
fi
es_elf_arm64 "$init_real" || die "$init_real is not a $ARCH ELF: is this really the $ARCH image?"
PROP="$ROOTFS/system/build.prop"
[ -f "$PROP" ] || die "no /system/build.prop in the rootfs"
ver="$(sed -n 's/^ro\.build\.version\.release=//p' "$PROP" | head -1)"
# En Redroid 13 la lista va como ro.system.product.cpu.abilist y
# ro.vendor.product.cpu.abilist; init deriva ro.product.cpu.abilist al arrancar.
abis="$(cat "$PROP" "$ROOTFS/vendor/build.prop" 2>/dev/null \
        | sed -nE 's/^ro\.(system\.|vendor\.)?product\.cpu\.abilist=//p' | head -1 || true)"
log "Android $ver, ABIs: ${abis:-?}"
[ "$ver" = 13 ] || echo "aviso: se esperaba Android 13 y el build.prop dice '$ver'" >&2
case "$abis" in *armeabi*|*x86,*|x86) echo "aviso: la imagen trae ABIs de 32 bits ($abis); Apple Silicon no las ejecuta" >&2 ;; esac
napex="$( (find "$ROOTFS/system/apex" -maxdepth 1 -name '*.apex' 2>/dev/null || true) | wc -l)"
if [ "$napex" -gt 0 ]; then
  echo "aviso: $napex APEX sin aplanar en /system/apex: apexd necesitará dispositivos loop" \
       "(CONFIG_BLK_DEV_LOOP=y, ya en config-android)" >&2
else
  log "APEX aplanados (ningún .apex en /system/apex)"
fi
if grep -qs 'mount binder binder /dev/binderfs' "$ROOTFS/system/etc/init/hw/init.rc"; then
  log "init.rc monta binderfs él mismo"
else
  echo "aviso: init.rc no monta binderfs; el lanzador lo montará" >&2
fi

# ── 3. base glibc ────────────────────────────────────────────────────────────
# La misma receta que la base de VON (cmd/kling/builder_llm.go, buildLLMBase):
# Debian trixie mínimo, sin puente. util-linux por unshare/nsenter; procps
# (ps, pgrep) solo por comodidad al depurar desde `kling exec`. iptables por
# el NAT de la red veth (usa iptables-legacy: el kernel no trae nf_tables).
if [ ! -f "$WORK/images/$BASE_NAME.ext4" ]; then
  command -v debootstrap >/dev/null 2>&1 || die "building the base needs debootstrap: apt-get install debootstrap"
  log "construyendo la base $BASE_NAME (debootstrap trixie; unos minutos)"
  KLING_ROOT="$WORK" SUITE=trixie PKGS="$BASE_PKGS" BRIDGE=/nonexistent \
    bash "$SCRIPTS/71-build-glibc-base.sh" "$BASE_NAME"
else
  log "base $BASE_NAME ya construida ($WORK/images/$BASE_NAME.ext4)"
fi

# ── slim ──
# SLIM=1: teléfono de automatización con la menor RAM posible (servicios,
# apps y propiedades de slim/, medidas en docs/densidad.md).
if [ "${SLIM:-0}" = 1 ]; then
  log "SLIM=1: adelgazando el rootfs (slim/apply.sh)"
  bash "$HERE/slim/apply.sh" "$ROOTFS"
fi
# ── fin slim ──

# ── 4. lo que va en la capa ──────────────────────────────────────────────────
LIB="$TREE/usr/local/lib/kindling-android"
install -Dm755 "$PROTO/kernel/check-android-config.sh" "$LIB/check-android-config.sh"
# android-sh va en los dos caminos: es la vía con allow_exec (kling exec) para
# depurar, y la usan fase0.sh y uidump.
install -Dm755 "$HERE/android-sh"          "$TREE/usr/local/bin/android-sh"
install -Dm644 "$STAGE/entrypoint.args"    "$LIB/entrypoint.args"
# "Listo" = boot_completed y la identidad por clon, en la imagen: los ejecuta el
# agente de invitado (docs/api.md del núcleo, "Listo y ganchos tras restaurar").
if [ "$PHONED" = 1 ]; then
  install -Dm755 "$KLING_PHONED"           "$TREE/usr/local/bin/kling-phoned"
  install -Dm755 "$HERE/kindling-phoned/ready" "$TREE/etc/kindling/ready"
  install -Dm755 "$HERE/kindling-phoned/post-restore.d/10-identity" "$TREE/etc/kindling/post-restore.d/10-identity"
  SERVICE_BIN=/usr/local/bin/kling-phoned
else
  install -Dm755 "$HERE/android-launch.sh" "$LIB/android-launch.sh"
  install -Dm755 "$HERE/kindling/ready"    "$TREE/etc/kindling/ready"
  install -Dm755 "$HERE/kindling/post-restore.d/10-identity" "$TREE/etc/kindling/post-restore.d/10-identity"
  SERVICE_BIN=/usr/local/lib/kindling-android/android-launch.sh
fi
# ── uidump ───────────────────────────────────────────────────────────────────
# Servidor residente de UiAutomation (prototypes/android/uidump, docs/uidump.md):
# el XML de `uiautomator dump` en ~20 ms en vez de ~1,9 s. El dex se compila
# aquí (JDK 17; android.jar y d8 se bajan verificados por sha256); init lo
# arranca con boot_completed y `uidump` (en la VM) le habla. UIDUMP=auto lo
# salta con un aviso si no hay javac; UIDUMP=1 lo exige; UIDUMP=0 lo quita.
UIDUMP="${UIDUMP:-auto}"
if [ "$UIDUMP" != 0 ]; then
  if command -v javac >/dev/null 2>&1; then
    bash "$PROTO/uidump/build.sh" "$STAGE/kindling-uidump.dex"
    install -Dm644 "$STAGE/kindling-uidump.dex"            "$ROOTFS/system/framework/kindling-uidump.dex"
    install -Dm644 "$PROTO/uidump/kindling-uidump.rc"      "$ROOTFS/system/etc/init/kindling-uidump.rc"
    install -Dm755 "$PROTO/uidump/uidump"                  "$TREE/usr/local/bin/uidump"
  elif [ "$UIDUMP" = 1 ]; then
    die "UIDUMP=1 needs javac (apt-get install openjdk-17-jdk-headless)"
  else
    echo "aviso: sin javac la imagen sale sin uidump (apt-get install openjdk-17-jdk-headless)" >&2
  fi
fi
# ── fin uidump ───────────────────────────────────────────────────────────────
# /data en RAM = un ext4 vacío y disperso que el lanzador copia a un tmpfs y
# monta en loop. No un tmpfs a secas: PackageManager guarda el serial del
# usuario en un xattr "user.*" de /data/user/0, tmpfs no los admite hasta Linux
# 6.6, y sin él Android destruye el usuario 0 en cada arranque y system_server
# muere en bucle (medido en la fase 0). Sin journal: /data vive lo que la VM.
# Disperso, así que en la capa solo ocupan los metadatos (cp -a conserva los huecos).
if [ "$DATA_MODE" = tmpfs ]; then
  rm -f "$LIB/data.ext4"
  truncate -s "$DATA_SIZE" "$LIB/data.ext4"
  mkfs.ext4 -q -F -O ^has_journal -m 0 -E lazy_itable_init=1,nodiscard -L data "$LIB/data.ext4"
fi
cat >"$LIB/android.conf" <<EOF
# Generado por prototypes/android/image/build-image.sh. Lo leen
# android-launch.sh y android-sh (bash, "source").
ANDROID_ROOT=/android
ANDROID_WIDTH=$WIDTH
ANDROID_HEIGHT=$HEIGHT
ANDROID_DPI=$DPI
ANDROID_FPS=$FPS
ANDROID_DATA_MODE=$DATA_MODE
ANDROID_DATA_SIZE=$DATA_SIZE
ANDROID_NET=$ANDROID_NET
ANDROID_EXTRA_ARGS="$EXTRA_ARGS"
ANDROID_ADB_SECURE=$ADB_SECURE
ANDROID_PREP=$PREP
EOF
cat >"$LIB/IMAGE.txt" <<EOF
redroid=$SOURCE_REF
redroid_tag=$REDROID_TAG
redroid_index=$REDROID_INDEX_DIGEST
android_release=$ver
abilist=$abis
kindling_commit=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)
built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

# ── 5. la capa, con el motor del constructor base ────────────────────────────
# Los mismos parámetros que pasa el constructor "llm": ROOTFS_DIR se copia a la
# raíz y SERVICE se arranca (y relanza) antes de ceder el PID 1 a kling-guest.
: >"$STAGE/env"
log "montando la capa $IMAGE_NAME sobre $BASE_NAME (GROW=$GROW MiB; copiar ~2 GiB tarda)"
KLING_ROOT="$WORK" NAME="$IMAGE_NAME" BASE="$BASE_NAME" GROW="$GROW" PKGS="" \
  AGENT="$KLING_GUEST" ENV_FILE="$STAGE/env" ROOTFS_DIR="$TREE" \
  SERVICE="$SERVICE_BIN" \
  bash "$SCRIPTS/81-base-image.sh"

# ── 6. receta: lo que el daemon lee para saber la base de una capa ───────────
# (internal/machine/layer.go, recipeBase; api.ImageRecipe). Sin ella, el
# daemon supondría la base "min" (Alpine) y el invitado no arrancaría.
# guest_ipv6_stack: el núcleo arranca los invitados con ipv6.disable=1, y sin
# sockets AF_INET6 adbd no escucha en 5555 (solo abre [::]) y el IpClient del
# eth0 de Android falla en bucle borrándole la IPv4. Con esto el módulo carga
# sin direcciones v6 (ipv6.disable_ipv6=1); lo que salga lo sigue cerrando el host.
cat >"$WORK/images/$IMAGE_NAME.recipe.json" <<EOF
{
  "name": "$IMAGE_NAME",
  "base": "$BASE_NAME",
  "cmd": [],
  "grow_mb": $GROW,
  "cpu_pct_per_vcpu": 100,
  "guest_ipv6_stack": true,
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "builder": "prototype-android",
  "spec": {"redroid": "$SOURCE_REF", "tag": "$REDROID_TAG", "arch": "$ARCH",
           "data_mode": "$DATA_MODE", "net": "$ANDROID_NET",
           "phoned": $([ "$PHONED" = 1 ] && echo true || echo false), "adb_secure": $([ "$ADB_SECURE" = 1 ] && echo true || echo false),
           "display": "${WIDTH}x${HEIGHT}@${DPI}dpi"}
}
EOF

# ── 6b. dm-verity (VERITY=1) ─────────────────────────────────────────────────
# Toca la capa (árbol pegado detrás), la base (dmsetup, init y tabla) y la
# receta (raíz del hash). Va antes del paquete para que SHA256SUMS lo cubra.
if [ "$VERITY" = 1 ]; then
  bash "$HERE/verity.sh" "$WORK/images" "$IMAGE_NAME" "$BASE_NAME"
fi

# ── 7. paquete para el Mac ───────────────────────────────────────────────────
STAMP="$(date -u +%Y%m%d-%H%M%S)"
PKG="$OUT/android-fase0-$STAMP"
mkdir -p "$PKG/images"
cp "$LIB/IMAGE.txt" "$PKG/BUILDINFO"
# El árbol ya está dentro de la capa: fuera antes de empaquetar, y el paquete
# enlaza (no copia) las imágenes si puede. En una VM Lima con el disco justo, el
# árbol más dos copias de la capa no cabían.
limpiar
for f in "$WORK/images/$BASE_NAME.ext4" "$WORK/images/$IMAGE_NAME.layer.ext4" \
         "$WORK/images/$IMAGE_NAME.recipe.json"; do
  ln -f "$f" "$PKG/images/" 2>/dev/null || cp --sparse=always "$f" "$PKG/images/"
done
{
  echo "image_name=$IMAGE_NAME"
  echo "base_name=$BASE_NAME"
  echo "kling_guest_sha256=$(sha256sum "$KLING_GUEST" | cut -d' ' -f1)"
  [ "$PHONED" = 1 ] && echo "kling_phoned_sha256=$(sha256sum "$KLING_PHONED" | cut -d' ' -f1)"
  echo "arch=$ARCH"
  echo "data_mode=$DATA_MODE net=$ANDROID_NET display=${WIDTH}x${HEIGHT}@${DPI}"
} >>"$PKG/BUILDINFO"
(cd "$PKG" && sha256sum images/* >SHA256SUMS)
tar -C "$OUT" -cSf "$PKG.tar" "$(basename "$PKG")"
(cd "$OUT" && sha256sum "$(basename "$PKG").tar" >"$(basename "$PKG").tar.sha256")
rm -rf "$PKG"

echo
log "listo: $PKG.tar"
echo "  $(cut -d' ' -f1 "$PKG.tar.sha256")  (sha256 del tar)"
echo "  capa:  $(du -h "$WORK/images/$IMAGE_NAME.layer.ext4" | cut -f1)   base: $(du -h "$WORK/images/$BASE_NAME.ext4" | cut -f1)"
echo
echo "Llévalo al Mac (desde el Mac, con la VM Lima llamada p. ej. kindling-arm):"
echo "  limactl copy kindling-arm:$PKG.tar ~/Downloads/"
echo "  limactl copy kindling-arm:$PKG.tar.sha256 ~/Downloads/"
echo "  cd ~/Downloads && shasum -a 256 -c $(basename "$PKG").tar.sha256"
echo "y pásaselo a fase0.sh con -bundle ~/Downloads/$(basename "$PKG").tar"
