#!/usr/bin/env bash
# Fork de un sandbox para probar N variantes. `--dry-run` imprime las órdenes y no
# ejecuta nada. Usa el daemon del núcleo (`kling sandbox`), sin gateway.
# Compatible con bash 3.2 (macOS).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

seco=0
[ "${1:-}" = "--dry-run" ] && seco=1
paso() {
  echo "+ $*"
  [ "$seco" -eq 1 ] || "$@"
}

paso kling image toolchain
# 1. El sandbox base hace el trabajo caro una sola vez (aquí: un proyecto de juguete).
paso kling sandbox create -image toolchain -name base -ttl 30m
paso kling exec base -- sh -c 'mkdir -p /work && printf "FACTOR = 1\n\ndef f(x):\n    return x * FACTOR\n" > /work/f.py'

# 2. Fork: tres copias que arrancan desde el estado exacto del base, con su disco y su memoria.
echo "+ kling sandbox fork base -n 3 -q"
if [ "$seco" -eq 1 ]; then
  copias="copy-1 copy-2 copy-3"
else
  copias=$(kling sandbox fork base -n 3 -q)
fi

# 3. Una variante en cada copia; ninguna ve lo que hizo otra.
factor=2
for c in $copias; do
  paso kling exec "$c" -- python3 -c "import sys; sys.path.insert(0, '/work'); import f; f.FACTOR = $factor; print('FACTOR=$factor', f.f(10))"
  factor=$((factor + 1))
done

# 4. Limpieza.
for c in $copias; do
  paso kling sandbox rm "$c"
done
paso kling sandbox rm base
