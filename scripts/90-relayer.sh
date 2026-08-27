#!/usr/bin/env bash
# relayer.sh — reconstruye UN servicio de producción como capa sobre una base.
#
#   relayer.sh <servicio> <base> <paquete-npm> <nº herramientas esperadas> [flags de import...]
#
# El orden importa y es lo que hace esto reversible: la imagen monolítica no se
# borra, se APARTA. Mientras está apartada, el resolutor de kindling ve la capa
# (la monolítica tiene precedencia si están las dos). Si algo sale mal, se
# devuelve a su sitio y el servicio vuelve a ser exactamente el que era.
#
# El comando del servidor NO se pasa: se lee del /entrypoint de la imagen vieja,
# que es lo único que sobrevivió de cómo se construyó. Esa es la razón de ser de
# las recetas, y ninguno de estos servicios las tiene.
set -euo pipefail

SVC="${1:?falta el servicio}"; BASE="${2:?falta la base}"
PKG="${3:?falta el paquete npm}"; WANT="${4:?faltan las herramientas esperadas}"
shift 4

IMG=/var/lib/kindling/images
MONO="$IMG/$SVC.ext4"
[ -f "$MONO" ] || { echo "$SVC: no es monolítica, ¿ya está por capas?" >&2; exit 1; }

# El comando, tal cual lo ejecuta hoy el invitado.
CMD=$(sudo debugfs -R "cat /entrypoint" "$MONO" 2>/dev/null \
  | sed -n 's|^exec /usr/local/bin/kling-bridge -listen :8080 -- ||p')
[ -n "$CMD" ] || { echo "$SVC: no pude leer el comando de su entrypoint" >&2; exit 1; }
echo "== $SVC: $PKG -> [$CMD] sobre base '$BASE'"

# El JSON se arma con python: el comando puede llevar espacios (varios argumentos)
# y meterlo a mano en una cadena es cómo se cuela una comilla suelta.
REQ=$(python3 -c '
import json,sys
svc,base,pkg,cmd = sys.argv[1:5]
print(json.dumps({"name":svc,"base":base,"packages":["nodejs","npm"],
                  "npm":[pkg],"cmd":cmd.split()}))' "$SVC" "$BASE" "$PKG" "$CMD")

sudo sh -c "sync; echo 3 > /proc/sys/vm/drop_caches"; sleep 2
OUT=$(curl -s --unix-socket /run/kling.sock -X POST http://d/images \
        -H "Content-Type: application/json" -d "$REQ")
if ! printf '%s' "$OUT" | grep -q "capa sobre base"; then
  echo "$SVC: la construcción falló" >&2
  printf '%s' "$OUT" | tail -c 600 >&2
  exit 1
fi
printf '%s\n' "  $(printf '%s' "$OUT" | grep -o 'capa sobre base[^\\]*')"

sudo mv "$MONO" "$MONO.old"
restaurar() { sudo mv -f "$MONO.old" "$MONO" 2>/dev/null || true; }

sudo sh -c "sync; echo 3 > /proc/sys/vm/drop_caches"; sleep 2
if ! kling mcp import "$SVC" -image "$SVC" -force "$@" 2>&1 | tee /tmp/import.$SVC | grep -q "imported with"; then
  echo "$SVC: la importación falló, devuelvo la monolítica" >&2
  tail -5 /tmp/import.$SVC >&2
  restaurar
  exit 1
fi

GOT=$(sudo python3 -c "
import json; print(len(json.load(open('/var/lib/kindling/snapshots/$SVC/meta.json')).get('tools',[])))")
if [ "$GOT" != "$WANT" ]; then
  echo "$SVC: catálogo distinto ($GOT herramientas, esperaba $WANT); devuelvo la monolítica" >&2
  restaurar
  exit 1
fi

sudo rm -f "$MONO.old"
echo "  ✓ $SVC por capas, $GOT herramienta(s), monolítica retirada"
