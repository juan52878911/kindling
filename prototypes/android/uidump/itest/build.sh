#!/bin/bash
# Compila una APK mínima con una Instrumentation (kindling.itest/.T) que toma
# UiAutomation, lee la raíz de la ventana activa y termina: sirve para probar que
# `am instrument` convive con uidump (coexist.sh). Necesita lo de build.sh más
# aapt, apksigner, zipalign y keytool (apt-get install aapt apksigner zipalign).
#
#   prototypes/android/uidump/itest/build.sh [SALIDA.apk]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${1:-$HERE/itest.apk}"
SDK="${UIDUMP_SDK:-$HOME/.cache/kindling-uidump}"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
"$HERE/../build.sh" "$W/uidump.dex" >/dev/null   # deja android-33.jar y r8 en $SDK
mkdir -p "$W/classes" "$W/dex"
javac --release 11 -Xlint:-options -cp "$SDK/android-33.jar" -d "$W/classes" "$HERE/T.java"
java -cp "$SDK"/r8-*.jar com.android.tools.r8.D8 --release --min-api 33 --lib "$SDK/android-33.jar" \
  --output "$W/dex" "$W"/classes/kindling/itest/*.class
aapt package -f -M "$HERE/AndroidManifest.xml" -I "$SDK/android-33.jar" -F "$W/unsigned.apk"
(cd "$W/dex" && aapt add "$W/unsigned.apk" classes.dex >/dev/null)
keytool -genkeypair -keystore "$W/ks.jks" -storepass android -keypass android -alias k -keyalg RSA \
  -keysize 2048 -validity 10000 -dname CN=itest >/dev/null 2>&1
zipalign -f 4 "$W/unsigned.apk" "$W/aligned.apk"
apksigner sign --ks "$W/ks.jks" --ks-pass pass:android --key-pass pass:android --out "$OUT" "$W/aligned.apk"
echo "itest: $OUT"
