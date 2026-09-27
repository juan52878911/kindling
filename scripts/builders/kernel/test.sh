#!/usr/bin/env bash
# Pruebas de scripts/check-kernel-config.sh y de los fragmentos, sin fuentes
# del kernel, sin root y sin KVM:
#
#   scripts/builders/kernel/test.sh
#
# 1. Un .config sintético hecho SOLO con lo que piden los fragmentos tiene que
#    pasar la comprobación: si alguien añade una obligatoria al checker y no al
#    fragmento (o enciende una prohibida en el fragmento), falla aquí y no tras
#    media hora de build.
# 2. Cada regla del checker se prueba rompiendo ese .config de una en una.
# 3. Si el anfitrión expone /proc/config.gz, se le pasa el checker a título
#    informativo (no cuenta: no es nuestro kernel).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$(cd "$HERE/../.." && pwd)/check-kernel-config.sh"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT

pasa=0; fallos=0
ok()  { echo "ok   $*"; pasa=$((pasa + 1)); }
mal() { echo "FAIL $*"; fallos=$((fallos + 1)); }

# sintetico <arch> escribe el .config que resultaría si Kconfig aplicase los
# fragmentos al pie de la letra (la última línea de cada opción manda), más el
# símbolo de arquitectura, que viene de ARCH= y no del fragmento.
sintetico() {
  local arch="$1" l k
  declare -A v=()
  local orden=()
  while IFS= read -r l; do
    case "$l" in
      CONFIG_*=*)                k="${l%%=*}"; [ -n "${v[$k]+x}" ] || orden+=("$k"); v[$k]="${l#*=}" ;;
      "# CONFIG_"*" is not set") k="${l#\# }"; k="${k%% *}"; [ -n "${v[$k]+x}" ] || orden+=("$k"); v[$k]=n ;;
    esac
  done < <(cat "$HERE/config-common" "$HERE/config-$arch")
  case "$arch" in
    amd64) echo "CONFIG_X86_64=y" ;;
    arm64) echo "CONFIG_ARM64=y" ;;
  esac
  for k in "${orden[@]}"; do
    if [ "${v[$k]}" = n ]; then echo "# $k is not set"; else echo "$k=${v[$k]}"; fi
  done
}

# espera <código> <texto esperado o ""> <descripción> -- <args del checker>
espera() {
  local code="$1" texto="$2" desc="$3"; shift 4
  local out rc=0
  out="$("$CHECK" "$@" 2>&1)" || rc=$?
  if [ "$rc" -ne "$code" ]; then
    mal "$desc: exit $rc, expected $code"; echo "$out" | sed 's/^/     /'; return
  fi
  if [ -n "$texto" ] && ! grep -qF -- "$texto" <<<"$out"; then
    mal "$desc: output lacks '$texto'"; echo "$out" | sed 's/^/     /'; return
  fi
  ok "$desc"
}

# muta <base> <salida> <sed>: copia el .config aplicando una edición.
muta() { sed "$3" "$1" >"$2"; }

for arch in amd64 arm64; do
  base="$T/$arch.config"
  sintetico "$arch" >"$base"
  espera 0 "($arch): ok" "$arch: los fragmentos cumplen el checker" -- "$base"

  muta "$base" "$T/x" '/^CONFIG_VIRTIO_BLK=/d'
  espera 1 "CONFIG_VIRTIO_BLK must be y" "$arch: falta virtio-blk" -- "$T/x"

  muta "$base" "$T/x" 's/^CONFIG_OVERLAY_FS=y/CONFIG_OVERLAY_FS=m/'
  espera 1 "CONFIG_OVERLAY_FS must be y (is m)" "$arch: overlayfs como módulo no vale" -- "$T/x"

  muta "$base" "$T/x" 's/^# CONFIG_MODULES is not set/CONFIG_MODULES=y/'
  espera 1 "CONFIG_MODULES must not be set" "$arch: módulos prohibidos" -- "$T/x"

  { cat "$base"; echo "CONFIG_SERIO_I8042=y"; } >"$T/x"
  espera 1 "CONFIG_SERIO_I8042 must not be set" "$arch: i8042 prohibido" -- "$T/x"

  { cat "$base"; echo "CONFIG_IO_URING=y"; } >"$T/x"
  espera 1 "CONFIG_IO_URING must not be set" "$arch: io_uring prohibido" -- "$T/x"

  { cat "$base"; echo "CONFIG_KEXEC=y"; } >"$T/x"
  espera 1 "CONFIG_KEXEC must not be set" "$arch: kexec prohibido" -- "$T/x"

  { cat "$base"; echo "CONFIG_USB=y"; echo "CONFIG_SND=m"; } >"$T/x"
  espera 1 "options built as modules: CONFIG_SND" "$arch: USB y un =m" -- "$T/x"

  muta "$base" "$T/x" '/^CONFIG_BPF_UNPRIV_DEFAULT_OFF=/d'
  espera 1 "unprivileged BPF" "$arch: BPF sin privilegios" -- "$T/x"

  muta "$base" "$T/x" '/^CONFIG_BPF_\(SYSCALL\|UNPRIV_DEFAULT_OFF\)=/d'
  espera 0 "" "$arch: sin bpf(2) no hace falta UNPRIV_DEFAULT_OFF" -- "$T/x"

  # Varios fallos a la vez: se listan todos.
  muta "$base" "$T/x" '/^CONFIG_\(FUSE_FS\|IP_PNP\|DEVTMPFS_MOUNT\)=/d'
  espera 1 "3 problem(s)" "$arch: lista todos los fallos" -- "$T/x"
done

# PCI: prohibido en amd64 (pci=off), obligatorio en arm64 (vz).
{ cat "$T/amd64.config"; echo "CONFIG_PCI=y"; } >"$T/x"
espera 1 "CONFIG_PCI must not be set" "amd64: PCI prohibido" -- "$T/x"
muta "$T/arm64.config" "$T/x" '/^CONFIG_VIRTIO_PCI=/d'
espera 1 "CONFIG_VIRTIO_PCI must be y" "arm64: virtio-pci obligatorio para vz" -- "$T/x"

# Nombres alternativos de mitigaciones (6.9+).
muta "$T/amd64.config" "$T/x" 's/^CONFIG_RETPOLINE=y/CONFIG_MITIGATION_RETPOLINE=y/'
espera 0 "" "amd64: MITIGATION_RETPOLINE vale por RETPOLINE" -- "$T/x"
muta "$T/amd64.config" "$T/x" '/^CONFIG_RETPOLINE=/d'
espera 1 "CONFIG_RETPOLINE must be y" "amd64: sin retpoline" -- "$T/x"

# Arquitectura: se deduce, se fuerza, o se pide.
espera 0 "(arm64): ok" "--arch explícito" -- --arch arm64 "$T/arm64.config"
espera 1 "CONFIG_ARM64 must be y" "--arch que no cuadra con el .config" -- --arch arm64 "$T/amd64.config"
grep -v '^CONFIG_X86_64=' "$T/amd64.config" >"$T/x"
espera 2 "pass --arch" "sin arquitectura deducible" -- "$T/x"
espera 2 "uso:" "sin argumentos" --
espera 2 "uso:" "--arch inválido" -- --arch riscv "$T/amd64.config"
espera 2 "cannot read" "fichero inexistente" -- "$T/no-existe"

# Entrada estándar.
if out="$("$CHECK" - <"$T/amd64.config" 2>&1)"; then ok "lee de stdin"; else mal "lee de stdin"; echo "$out"; fi

# ── build.sh contra un árbol falso ───────────────────────────────────────────
# Un "kernel" cuyo Makefile escribe el .config a partir del fragmento, se deja
# CONFIG_PVH por el camino (como haría una dependencia no satisfecha) y
# "compila" un fichero de texto. Prueba la tubería de build.sh —descarga,
# sha256, extracción, deriva, checker, salidas— sin fuentes ni red.
BUILD="$HERE/build.sh"
herramientas=1
for c in curl tar xz sha256sum make gcc flex bison bc perl; do
  command -v "$c" >/dev/null || herramientas=0
done
if [ "$herramientas" = 0 ]; then
  echo "skip build.sh: faltan herramientas de build"
else
  V=6.1.999
  mkdir -p "$T/src/linux-$V" "$T/mirror/v6.x"
  cat >"$T/src/linux-$V/Makefile" <<'MK'
allnoconfig:
	@{ case "$(ARCH)" in x86_64) echo CONFIG_X86_64=y;; arm64) echo CONFIG_ARM64=y;; esac; \
	   grep -E '^CONFIG_' "$(KCONFIG_ALLCONFIG)" | grep -v '^CONFIG_PVH='; \
	   [ -z "$(FAKE_EXTRA)" ] || echo "$(FAKE_EXTRA)"; } >.config
olddefconfig:
	@true
vmlinux:
	@echo "kernel falso" >vmlinux
Image:
	@mkdir -p arch/arm64/boot && echo "kernel falso" >arch/arm64/boot/Image
MK
  tar -cJf "$T/mirror/v6.x/linux-$V.tar.xz" -C "$T/src" "linux-$V"
  SHA="$(sha256sum "$T/mirror/v6.x/linux-$V.tar.xz" | cut -d' ' -f1)"
  export KERNEL_MIRROR="file://$T/mirror" CACHE="$T/cache" OUT="$T/out" KERNEL_VERSION="$V" TARGET_ARCH=amd64
  N="vmlinux-$V-kindling-amd64"

  # build <código> <texto> <desc> [VAR=valor...]
  build() {
    local code="$1" texto="$2" desc="$3"; shift 3
    local out rc=0
    out="$(env "$@" "$BUILD" 2>&1)" || rc=$?
    if [ "$rc" -ne "$code" ]; then
      mal "$desc: exit $rc, expected $code"; echo "$out" | tail -15 | sed 's/^/     /'; return
    fi
    if [ -n "$texto" ] && ! grep -qF -- "$texto" <<<"$out"; then
      mal "$desc: output lacks '$texto'"; echo "$out" | tail -15 | sed 's/^/     /'; return
    fi
    ok "$desc"
  }

  [ "$("$BUILD" --print-name)" = "$N" ] && ok "build.sh --print-name" || mal "build.sh --print-name: $("$BUILD" --print-name)"
  build 1 "unsupported TARGET_ARCH" "build.sh: arquitectura desconocida" TARGET_ARCH=riscv
  build 1 "computed: $SHA" "build.sh: sin sha256 fijado no sigue" KERNEL_SHA256=
  build 1 "sha256 mismatch" "build.sh: sha256 que no cuadra" KERNEL_SHA256=0000
  [ ! -f "$T/cache/linux-$V.tar.xz" ] && ok "build.sh: borra la descarga mala" || mal "build.sh: la descarga mala sigue en la cache"
  build 0 "CONFIG_PVH: requested y, got n" "build.sh: avisa de la deriva y sigue" KERNEL_SHA256="$SHA" CONFIG_ONLY=1
  [ -f "$T/out/$N.config" ] && ok "build.sh: deja el .config" || mal "build.sh: no dejó $N.config"
  build 1 "did not apply (STRICT_FRAGMENT=1)" "build.sh: STRICT_FRAGMENT corta" KERNEL_SHA256="$SHA" CONFIG_ONLY=1 STRICT_FRAGMENT=1
  build 1 "CONFIG_MODULES must not be set" "build.sh: el checker corta antes de compilar" KERNEL_SHA256="$SHA" FAKE_EXTRA=CONFIG_MODULES=y
  [ ! -f "$T/out/$N" ] && ok "build.sh: sin kernel si el checker falla" || mal "build.sh: dejó kernel con config mala"
  if echo '#include <libelf.h>' | gcc -E -x c - >/dev/null 2>&1; then
    build 0 "listo en" "build.sh: build completo" KERNEL_SHA256="$SHA"
    if [ -f "$T/out/$N" ] && (cd "$T/out" && sha256sum -c --quiet "$N.sha256") &&
       grep -q "^tarball_sha256=$SHA\$" "$T/out/$N.buildinfo"; then
      ok "build.sh: kernel, .sha256 y .buildinfo"
    else
      mal "build.sh: salidas incompletas"; ls -la "$T/out"
    fi
  else
    echo "skip build.sh completo: falta libelf-dev"
  fi
  unset KERNEL_MIRROR CACHE OUT KERNEL_VERSION TARGET_ARCH
fi

if [ -r /proc/config.gz ]; then
  echo "--- informativo: kernel del anfitrión ($(uname -r)), no cuenta ---"
  zcat /proc/config.gz | "$CHECK" - 2>&1 | sed 's/^/     /' || true
fi

echo "$pasa ok, $fallos FAIL"
[ "$fallos" -eq 0 ]
