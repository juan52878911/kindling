#!/usr/bin/env bash
# Construye el kernel de invitado K1 + el fragmento Android (config-android)
# para el backend vz (macOS, arm64). Fase 0: NO modifica el builder del núcleo.
#
#   prototypes/android/kernel/build.sh                         # en Linux arm64 (Lima)
#   prototypes/android/kernel/build.sh                         # en Linux amd64: cruzado,
#                                                              #   apt install gcc-aarch64-linux-gnu
#   KERNEL_SHA256=<sha256> prototypes/android/kernel/build.sh  # sin tocar kernel.pin
#   CONFIG_ONLY=1 prototypes/android/kernel/build.sh           # solo .config + comprobaciones
#   OUT=/ruta prototypes/android/kernel/build.sh               # dónde dejarlo (por defecto ./out aquí)
#
# CÓMO, SIN TOCAR EL NÚCLEO. scripts/builders/kernel/build.sh no acepta un
# fragmento extra: lee config-common y config-$TARGET_ARCH de SU directorio y
# llama a scripts/check-kernel-config.sh relativo a sí mismo (HERE/REPO). Así
# que este script monta un árbol de sombra temporal con la misma forma:
#
#   $SOMBRA/scripts/builders/kernel/build.sh      copia literal del builder
#   $SOMBRA/scripts/builders/kernel/kernel.pin    copia literal del pin
#   $SOMBRA/scripts/builders/kernel/config-common copia literal
#   $SOMBRA/scripts/builders/kernel/config-arm64  config-arm64 + config-android
#   $SOMBRA/scripts/check-kernel-config.sh        envoltorio: el comprobador del
#                                                 núcleo Y check-android-config.sh
#
# y ejecuta la copia. Todo lo demás (descarga verificada contra el sha256,
# allnoconfig + olddefconfig, aviso de deriva, compilación reproducible) es el
# builder del núcleo tal cual. Si mañana el builder acepta un fragmento extra
# (ver README, "Qué necesitaría el núcleo"), esto se reduce a una línea.
#
# El resultado lleva "-android" en el nombre para que no se confunda con el K1
# normal: vmlinux-<versión>-kindling-arm64-android (+ .config .sha256 .buildinfo).
# Es un `Image` arm64, que es lo que carga Virtualization.framework
# (docs/vz-mac-prototipo.md: "el vmlinux aarch64 ... ya es un Image").
#
# EL SHA256 DEL TARBALL. kernel.pin viene con KERNEL_SHA256 vacío a propósito
# y el builder se niega a compilar sin él. Dos formas de dárselo:
#   a) KERNEL_SHA256=... en el entorno (no toca el repo), o
#   b) rellenar scripts/builders/kernel/kernel.pin (es el paso previsto por K1).
# En los dos casos, el valor sale de la lista FIRMADA de kernel.org (README).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CORE_KDIR="$REPO/scripts/builders/kernel"
CORE_CHECK="$REPO/scripts/check-kernel-config.sh"
ANDROID_FRAG="$HERE/config-android"
ANDROID_CHECK="$HERE/check-android-config.sh"

# ── gpu ── perfil opcional (ANDROID_PROFILE=gpu): config-android-gpu detrás de
# config-android, el comprobador del núcleo sin las líneas DRM/FB (que prohíbe
# para todo invitado) y check-android-gpu-config.sh para lo que el perfil
# añade. Sale como vmlinux-...-android-gpu. Ver docs/gpu.md.
PROFILE="${ANDROID_PROFILE:-}"
GPU_FRAG="$HERE/config-android-gpu"
GPU_CHECK="$HERE/check-android-gpu-config.sh"
case "$PROFILE" in
  "") ;;
  gpu) for f in "$GPU_FRAG" "$GPU_CHECK"; do [ -r "$f" ] || { echo "missing $f" >&2; exit 1; }; done ;;
  *) echo "ANDROID_PROFILE=$PROFILE: unknown profile (only: gpu)" >&2; exit 2 ;;
esac
# ── fin gpu ──


# ── x86_64 ── arm64 (vz en Apple Silicon) por defecto; TARGET_ARCH=amd64 para
# Firecracker en x86_64 (docs/proxmox.md): config-android va detrás de
# config-amd64, más config-android-x86_64 (IA32_EMULATION, que da el COMPAT
# que Android 13 exige).
TARGET_ARCH="${TARGET_ARCH:-arm64}"
case "$TARGET_ARCH" in
  arm64) X86_FRAG="" ;;
  amd64|x86_64) TARGET_ARCH=amd64; X86_FRAG="$HERE/config-android-x86_64"
                [ -r "$X86_FRAG" ] || { echo "missing $X86_FRAG" >&2; exit 1; } ;;
  *) echo "TARGET_ARCH=$TARGET_ARCH: only arm64 (vz) or amd64 (Firecracker)" >&2; exit 2 ;;
esac
export TARGET_ARCH
# ── fin x86_64 ──
for f in "$CORE_KDIR/build.sh" "$CORE_KDIR/kernel.pin" "$CORE_KDIR/config-common" \
         "$CORE_KDIR/config-$TARGET_ARCH" "$CORE_CHECK" "$ANDROID_FRAG" "$ANDROID_CHECK"; do
  [ -r "$f" ] || { echo "missing $f (run this from a kindling checkout)" >&2; exit 1; }
done

OUT="${OUT:-$HERE/out}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
export OUT

SOMBRA="$(mktemp -d "${TMPDIR:-/tmp}/kindling-android-kernel.XXXXXX")"
trap 'rm -rf "$SOMBRA"' EXIT
mkdir -p "$SOMBRA/scripts/builders/kernel"
cp "$CORE_KDIR/build.sh" "$CORE_KDIR/kernel.pin" "$CORE_KDIR/config-common" \
   "$SOMBRA/scripts/builders/kernel/"

# ── parches ── (patches/*.patch, -p1 sobre el árbol del kernel). El builder del
# núcleo no los admite, así que en la COPIA se mete una línea justo después de
# descomprimir el árbol. Hoy solo hay uno: dm-verity relee una vez un bloque de
# hashes que no cuadra y avisa cuando la relectura de un bloque de datos lo
# arregla (docs/verity.md). La fecha reproducible sale del Makefile, que no se
# toca. NO_PATCHES=1 los salta.
PATCHES=""
if [ "${NO_PATCHES:-0}" != 1 ]; then
  for p in "$HERE"/patches/*.patch; do [ -r "$p" ] && PATCHES="$PATCHES $p"; done
fi
if [ -n "$PATCHES" ]; then
  # shellcheck disable=SC2016  # el texto literal de la línea del builder
  ancla='SRC="$WORK/linux-${KERNEL_VERSION}"'
  grep -qxF "$ancla" "$SOMBRA/scripts/builders/kernel/build.sh" \
    || { echo "the core kernel builder changed: can't find where to apply patches/" >&2; exit 1; }
  awk -v a="$ancla" -v ps="$PATCHES" '{ print } $0 == a {
      n = split(ps, l, " ")
      for (i = 1; i <= n; i++) printf "patch -d \"$SRC\" -p1 --forward --no-backup-if-mismatch <\"%s\"\n", l[i]
    }' "$SOMBRA/scripts/builders/kernel/build.sh" >"$SOMBRA/build.sh.parcheado"
  mv "$SOMBRA/build.sh.parcheado" "$SOMBRA/scripts/builders/kernel/build.sh"
  echo "==> parches del kernel:$PATCHES"
fi
{
  cat "$CORE_KDIR/config-$TARGET_ARCH"
  echo
  echo "# ─── prototypes/android/kernel/config-android (añadido por su build.sh) ───"
  cat "$ANDROID_FRAG"
  if [ -n "$X86_FRAG" ]; then   # ── x86_64 ──
    echo
    echo "# ─── prototypes/android/kernel/config-android-x86_64 ───"
    cat "$X86_FRAG"
  fi
  if [ "$PROFILE" = gpu ]; then   # ── gpu ──
    echo
    echo "# ─── prototypes/android/kernel/config-android-gpu (perfil gpu) ───"
    cat "$GPU_FRAG"
  fi
} >"$SOMBRA/scripts/builders/kernel/config-$TARGET_ARCH"

# El comprobador de la sombra corre los dos y falla si falla cualquiera. El
# builder lo llama como "$CHECK --arch arm64 <.config>".
cat >"$SOMBRA/scripts/check-kernel-config.sh" <<EOF
#!/usr/bin/env bash
set -uo pipefail
cfg="\${@: -1}"
"$CORE_CHECK" "\$@"; rc1=\$?
"$ANDROID_CHECK" "\$cfg"; rc2=\$?
[ "\$rc1" -eq 0 ] && [ "\$rc2" -eq 0 ]
EOF
if [ "$PROFILE" = gpu ]; then   # ── gpu ──
cat >"$SOMBRA/scripts/check-kernel-config.sh" <<EOF
#!/usr/bin/env bash
set -uo pipefail
cfg="\${@: -1}"
sin="\$(mktemp)"
grep -v -E '^CONFIG_(DRM|FB)=' "\$cfg" >"\$sin"
"$CORE_CHECK" --arch $TARGET_ARCH "\$sin"; rc1=\$?
rm -f "\$sin"
"$ANDROID_CHECK" "\$cfg"; rc2=\$?
"$GPU_CHECK" "\$cfg"; rc3=\$?
[ "\$rc1" -eq 0 ] && [ "\$rc2" -eq 0 ] && [ "\$rc3" -eq 0 ]
EOF
fi
chmod +x "$SOMBRA/scripts/check-kernel-config.sh" "$SOMBRA/scripts/builders/kernel/build.sh"

NAME="$(bash "$SOMBRA/scripts/builders/kernel/build.sh" --print-name)"
echo "==> builder del núcleo (copia) con config-android: $NAME"
# El builder decide solo si compila cruzado (TARGET_ARCH != arquitectura local).
bash "$SOMBRA/scripts/builders/kernel/build.sh"

# Renombrar a *-android para que no pise ni se confunda con el K1 normal.
NEW="$NAME-android${PROFILE:+-$PROFILE}"
mv -f "$OUT/$NAME.config" "$OUT/$NEW.config"
if [ "${CONFIG_ONLY:-0}" = 1 ]; then
  echo "listo (solo configuración): $OUT/$NEW.config"
  exit 0
fi
mv -f "$OUT/$NAME" "$OUT/$NEW"
rm -f "$OUT/$NAME.sha256"
(cd "$OUT" && sha256sum "$NEW" >"$NEW.sha256")
{
  cat "$OUT/$NAME.buildinfo"
  echo "android_fragment_sha256=$(sha256sum "$ANDROID_FRAG" | cut -d' ' -f1)"
  [ -z "$X86_FRAG" ] || echo "android_x86_64_fragment_sha256=$(sha256sum "$X86_FRAG" | cut -d' ' -f1)"
  [ "$PROFILE" != gpu ] || echo "android_gpu_fragment_sha256=$(sha256sum "$GPU_FRAG" | cut -d' ' -f1)"
  for p in $PATCHES; do echo "android_patch=$(basename "$p") $(sha256sum "$p" | cut -d' ' -f1)"; done
  echo "kindling_commit=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "kernel_sha256_android=$(cut -d' ' -f1 "$OUT/$NEW.sha256")"
} >"$OUT/$NEW.buildinfo"
rm -f "$OUT/$NAME.buildinfo"

echo
echo "kernel Android listo: $OUT/$NEW"
echo "  sha256: $(cut -d' ' -f1 "$OUT/$NEW.sha256")"
echo "  llévalo al Mac junto con la imagen (README, paso 3)."
