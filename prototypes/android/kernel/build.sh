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

for f in "$CORE_KDIR/build.sh" "$CORE_KDIR/kernel.pin" "$CORE_KDIR/config-common" \
         "$CORE_KDIR/config-arm64" "$CORE_CHECK" "$ANDROID_FRAG" "$ANDROID_CHECK"; do
  [ -r "$f" ] || { echo "missing $f (run this from a kindling checkout)" >&2; exit 1; }
done

# vz solo existe en Apple Silicon: el kernel es arm64 siempre.
if [ -n "${TARGET_ARCH:-}" ] && [ "$TARGET_ARCH" != arm64 ]; then
  echo "TARGET_ARCH=$TARGET_ARCH: this prototype only targets arm64 (vz on Apple Silicon)" >&2
  exit 2
fi
export TARGET_ARCH=arm64

OUT="${OUT:-$HERE/out}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
export OUT

SOMBRA="$(mktemp -d "${TMPDIR:-/tmp}/kindling-android-kernel.XXXXXX")"
trap 'rm -rf "$SOMBRA"' EXIT
mkdir -p "$SOMBRA/scripts/builders/kernel"
cp "$CORE_KDIR/build.sh" "$CORE_KDIR/kernel.pin" "$CORE_KDIR/config-common" \
   "$SOMBRA/scripts/builders/kernel/"
{
  cat "$CORE_KDIR/config-arm64"
  echo
  echo "# ─── prototypes/android/kernel/config-android (añadido por su build.sh) ───"
  cat "$ANDROID_FRAG"
} >"$SOMBRA/scripts/builders/kernel/config-arm64"

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
chmod +x "$SOMBRA/scripts/check-kernel-config.sh" "$SOMBRA/scripts/builders/kernel/build.sh"

NAME="$(bash "$SOMBRA/scripts/builders/kernel/build.sh" --print-name)"
echo "==> builder del núcleo (copia) con config-android: $NAME"
# El builder decide solo si compila cruzado (TARGET_ARCH != arquitectura local).
bash "$SOMBRA/scripts/builders/kernel/build.sh"

# Renombrar a *-android para que no pise ni se confunda con el K1 normal.
NEW="$NAME-android"
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
  echo "kindling_commit=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "kernel_sha256_android=$(cut -d' ' -f1 "$OUT/$NEW.sha256")"
} >"$OUT/$NEW.buildinfo"
rm -f "$OUT/$NAME.buildinfo"

echo
echo "kernel Android listo: $OUT/$NEW"
echo "  sha256: $(cut -d' ' -f1 "$OUT/$NEW.sha256")"
echo "  llévalo al Mac junto con la imagen (README, paso 3)."
