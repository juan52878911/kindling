#!/bin/bash
# Compila dos APK mínimos (paquetes kindling.ssaidtest.a y .b, cada uno con su
# clave de firma: el SSAID depende del paquete y de la firma) que escriben su
# ANDROID_ID en logcat (etiqueta SSAIDTEST). Mismos requisitos que
# uidump/itest/build.sh (JDK, aapt, apksigner, zipalign, keytool).
#
#   prototypes/android/ssaidtest/build.sh [DIR_SALIDA]     → DIR/ssaid-a.apk y ssaid-b.apk
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${1:-$HERE}"
SDK="${UIDUMP_SDK:-$HOME/.cache/kindling-uidump}"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
mkdir -p "$OUT"
"$HERE/../uidump/build.sh" "$W/uidump.dex" >/dev/null   # deja android-33.jar y r8 en $SDK
mkdir -p "$W/classes" "$W/dex"
javac --release 11 -Xlint:-options -cp "$SDK/android-33.jar" -d "$W/classes" "$HERE/Main.java"
java -cp "$SDK"/r8-*.jar com.android.tools.r8.D8 --release --min-api 33 --lib "$SDK/android-33.jar" \
  --output "$W/dex" "$W"/classes/kindling/ssaidtest/*.class
for v in a b; do
  pkg="kindling.ssaidtest.$v"
  mkdir -p "$W/m-$v"; sed "s/@PKG@/$pkg/g" "$HERE/AndroidManifest.xml" >"$W/m-$v/AndroidManifest.xml"
  aapt package -f -M "$W/m-$v/AndroidManifest.xml" -I "$SDK/android-33.jar" -F "$W/u-$v.apk"
  (cd "$W/dex" && aapt add "$W/u-$v.apk" classes.dex >/dev/null)
  keytool -genkeypair -keystore "$W/ks-$v.jks" -storepass android -keypass android -alias k -keyalg RSA \
    -keysize 2048 -validity 10000 -dname "CN=ssaid-$v" >/dev/null 2>&1
  zipalign -f 4 "$W/u-$v.apk" "$W/al-$v.apk"
  apksigner sign --ks "$W/ks-$v.jks" --ks-pass pass:android --key-pass pass:android \
    --out "$OUT/ssaid-$v.apk" "$W/al-$v.apk"
  echo "ssaidtest: $OUT/ssaid-$v.apk ($pkg)"
done
