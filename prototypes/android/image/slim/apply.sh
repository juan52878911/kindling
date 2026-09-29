#!/bin/bash
# Adelgaza un rootfs de Redroid 13 para un teléfono de automatización: menos
# RAM por teléfono sin tocar lo que importa (arranque, boot_completed, abrir
# una app, uiautomator dump, screencap, adb). Lo llama build-image.sh con
# SLIM=1; también vale dentro de una VM ya arrancada contra /android para
# probar (y reiniciar Android después). Ver docs/densidad.md.
#
#   apply.sh ROOTFS            aplica todo (idempotente)
#   apply.sh ROOTFS --list     solo enseña lo que haría
#
# Qué hace, cada cosa con su fichero al lado:
#   slim.prop      propiedades añadidas al final de vendor/build.prop (lo último
#                  que se carga gana; ro.config.low_ram, montículos de ART, lmkd)
#   services.txt   servicios de init que se COMENTAN (bloque entero). Un
#                  "disabled" no basta: varios se arrancan con "start X"
#                  explícito, y un "start" de un servicio que no existe solo
#                  deja una línea en dmesg.
#   apps.txt       apps del sistema que se quitan del rootfs (nunca arrancan ni
#                  ocupan caché de páginas)
#   features.xml   funciones de hardware que se declaran ausentes (Bluetooth:
#                  sin ella, system_server no lanza la pila de BT, que en
#                  Redroid muere en bucle y llena /data de tombstones)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
R="${1:?usage: apply.sh ROOTFS [--list]}"
LIST=0; [ "${2:-}" = --list ] && LIST=1
[ -x "$R/system/bin/init" ] || [ -L "$R/system/bin/init" ] || { echo "apply.sh: $R does not look like an Android rootfs" >&2; exit 1; }
MARK="kindling-slim"
# Para medir palanca a palanca (docs/densidad.md): SLIM_PARTS elige las partes
# y SLIM_PROP cambia el fichero de propiedades.
PARTS="${SLIM_PARTS:-props services apps features}"
PROPF="${SLIM_PROP:-$HERE/slim.prop}"
parte() { case " $PARTS " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# Los enlaces de Redroid son absolutos (/product -> /system/product): se
# resuelven dentro del rootfs, no en la máquina donde corre esto.
dentro() {
  local p="$R$1" t n=0
  while [ -L "$p" ] && [ $n -lt 8 ]; do
    t="$(readlink "$p")"
    case "$t" in /*) p="$R$t" ;; *) p="$(dirname "$p")/$t" ;; esac
    n=$((n + 1))
  done
  printf '%s\n' "$p"
}
limpia() { sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d' "$1"; }

VENDOR="$(dentro /vendor)"

# ── 1. propiedades ──────────────────────────────────────────────────────────
PROP="$VENDOR/build.prop"
if ! parte props; then :
elif [ $LIST = 1 ]; then
  echo "props -> $PROP:"; limpia "$PROPF" | sed 's/^/  /'
else
  # Idempotente: se quita el bloque anterior y se vuelve a poner.
  sed -i "/^# >>> $MARK/,/^# <<< $MARK/d" "$PROP"
  { echo "# >>> $MARK (prototypes/android/image/slim/slim.prop)"; limpia "$PROPF"; echo "# <<< $MARK"; } >>"$PROP"
fi

# ── 2. servicios ────────────────────────────────────────────────────────────
# Todos los .rc que lee init: los de /system, /vendor, /odm, /product,
# /system_ext y los de los APEX aplanados (/system/apex/*/etc).
rcs=()
for d in /system/etc/init /system/etc/init/hw /vendor/etc/init /odm/etc/init /product/etc/init /system_ext/etc/init; do
  dd="$(dentro "$d")"
  [ -d "$dd" ] || continue
  for f in "$dd"/*.rc; do [ -f "$f" ] && rcs+=("$f"); done
done
for f in "$R"/system/apex/*/etc/*.rc; do [ -f "$f" ] && rcs+=("$f"); done

svcs=""; parte services && svcs="$(limpia "$HERE/services.txt")"
for svc in $svcs; do
  hits=0
  for f in "${rcs[@]}"; do
    grep -q "^service $svc " "$f" || continue
    hits=$((hits + 1))
    if [ $LIST = 1 ]; then echo "service $svc <- ${f#"$R"}"; continue; fi
    # Comenta desde "service X ..." hasta la siguiente sección (línea que no
    # empieza por espacio).
    awk -v s="$svc" -v m="$MARK" '
      $0 ~ "^service " s " " { dentro = 1; print "# " m ": " $0; next }
      dentro && /^[^ \t]/     { dentro = 0 }
      dentro && NF            { print "# " m ": " $0; next }
      { print }' "$f" >"$f.slim.tmp"
    cat "$f.slim.tmp" >"$f"; rm -f "$f.slim.tmp"
  done
  [ $hits -gt 0 ] || grep -rqs "^# $MARK: service $svc " "${rcs[@]}" || echo "apply.sh: aviso: service $svc not found" >&2
done

# ── 3. apps ─────────────────────────────────────────────────────────────────
apps=""; parte apps && apps="$(limpia "$HERE/apps.txt")"
for app in $apps; do
  d="$(dentro "/$app")"
  if [ -d "$d" ]; then
    if [ $LIST = 1 ]; then echo "app $app"; else rm -rf "${d:?}"; fi
  fi
done

# ── 4. funciones de hardware ────────────────────────────────────────────────
if ! parte features; then :
elif [ $LIST = 1 ]; then
  echo "features -> /vendor/etc/permissions/$MARK.xml"
else
  install -m644 "$HERE/features.xml" "$VENDOR/etc/permissions/$MARK.xml"
fi
[ $LIST = 1 ] || echo "apply.sh: $R slimmed ($PARTS)"
