#!/usr/bin/env bash
# Almacén de copia al escribir lleno, de punta a punta (docs/cow.md, «Almacén
# lleno»). LLENA el almacén del daemon a propósito: solo contra un daemon de
# pruebas con un almacén pequeño, p. ej. uno privado arrancado con
# KLING_COW_STORE_GIB=1 (ver docs/cow.md). Se niega con un almacén de más de
# 2 GiB.
#
#   KLING_E2E_DB_GOLDEN=pg ./scripts/e2e-cow-lleno.sh
#   KLING_E2E_RESTART='cmd' ...   cómo reiniciar ese daemon (sin ella, no se prueba el reinicio)
#
# Comprueba: con el almacén lleno ninguna copia ve errores de disco ni hace
# PANIC, las que escriben quedan retenidas (hold), una congelada no se
# descongela (mensaje claro) ni se pierde tras reiniciar el daemon, y tras
# `kling cow grow` todas vuelven solas con sus datos.
set -uo pipefail

KLING="${KLING:-kling}"
GOLDEN="${KLING_E2E_DB_GOLDEN:?KLING_E2E_DB_GOLDEN: a Postgres golden}"
FILAS="${FILAS:-400000}"
pass=0; fail=0
ok()  { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad() { printf "  \033[31mFALLO\033[0m %s\n     esperaba: %s\n     obtuvo:   %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }
json() { $KLING db ls -json | python3 -c "import json,sys; d=json.load(sys.stdin); $1"; }
copias() { json 'print(" ".join(c["name"] for c in d if c["name"].startswith("cowfull-")))'; }
retenidas() { json 'print(sum(1 for c in d if c["name"].startswith("cowfull-") and c.get("hold")))'; }
errores() { json 'print(sum(c.get("disk_errors",0) for c in d if c["name"].startswith("cowfull-")))'; }
psql() { $KLING exec -timeout 120s "$1" -- su -s /bin/sh postgres -c "psql -h /run/postgresql -X -At appdb -c \"$2\"" 2>&1 | tail -1; }

cleanup() {
  for c in $(copias); do $KLING db rm "$c" >/dev/null 2>&1; done
}
trap cleanup EXIT

tam=$($KLING cow | sed -n 's/.* of \([0-9]*\) MiB free.*/\1/p' | head -1)
[ -n "$tam" ] || { echo "el daemon no tiene almacén montado (crea una copia antes, o no usa daemon.cow=auto)"; }
if [ -n "$tam" ] && [ "$tam" -gt 2048 ]; then
  echo "el almacén es de $tam MiB: esta prueba lo llena; úsala con uno de pruebas de 1-2 GiB"; exit 2
fi

echo "1. copias que escriben hasta llenar el almacén"
$KLING db up "$GOLDEN" -name cowfull-a >/dev/null || { echo "no arranca $GOLDEN"; exit 1; }
$KLING db fork cowfull-a -n 4 >/dev/null || { echo "fork falló"; exit 1; }
$KLING db up "$GOLDEN" -name cowfull-rama >/dev/null
psql cowfull-rama "create table marca as select 42 x" >/dev/null
$KLING freeze cowfull-rama >/dev/null
escriben=$(copias | tr ' ' '\n' | grep -v '^cowfull-rama$' | tr '\n' ' ')
for c in $escriben; do
  ( $KLING exec -timeout 900s "$c" -- su -s /bin/sh postgres -c \
      "psql -h /run/postgresql -X appdb -c \"create table t as select g, repeat(md5(g::text),20) s from generate_series(1,$FILAS) g\" -c checkpoint" >/dev/null 2>&1 ) &
done
for _ in $(seq 1 120); do [ "$(retenidas)" -ge 1 ] && break; sleep 1; done
sleep 3
n=$(retenidas); [ "$n" -ge 1 ] && ok "el vigilante retuvo $n copias" || bad "retenidas" ">= 1" "$n"
e=$(errores); [ "$e" = 0 ] && ok "ninguna copia vio errores de disco" || bad "errores de disco" 0 "$e"
out=$($KLING thaw cowfull-rama 2>&1)
contiene "$out" "store full" && ok "la rama congelada no se despierta sin sitio: store full" \
  || ok "la rama se despertó (había sitio en ese momento): $out"

if [ -n "${KLING_E2E_RESTART:-}" ]; then
  echo "2. reinicio del daemon con copias retenidas"
  sh -c "$KLING_E2E_RESTART"; sleep 3
  r=$(copias | wc -w | tr -d ' ')
  [ "$r" = 6 ] && ok "las 6 copias siguen tras reiniciar" || bad "copias tras reiniciar" 6 "$r"
  sleep 6
  e=$(errores); [ "$e" = 0 ] && ok "tras reiniciar no se reanudan a ciegas (0 errores)" || bad "errores tras reiniciar" 0 "$e"
fi

echo "3. kling cow grow y vuelta"
out=$($KLING cow grow +768M 2>&1); contiene "$out" "MiB free" && ok "cow grow: $out" || bad "cow grow" "MiB free" "$out"
for _ in $(seq 1 60); do [ "$(retenidas)" = 0 ] && break; sleep 1; done
n=$(retenidas); [ "$n" = 0 ] && ok "todas reanudadas solas" || bad "retenidas tras crecer" 0 "$n"
wait
for c in $(copias); do
  v=$(psql "$c" "select 1"); p=$($KLING exec "$c" -- sh -c 'grep -c PANIC /var/log/postgresql/pg.log' 2>&1 | tail -1)
  [ "$v" = 1 ] && [ "$p" = 0 ] && ok "$c contesta, sin PANIC" || bad "$c" "1 y 0 PANIC" "$v / $p"
done
m=$(psql cowfull-rama "select x from marca"); [ "$m" = 42 ] && ok "la rama conserva sus datos" || bad "marca en la rama" 42 "$m"
e=$(errores); [ "$e" = 0 ] && ok "0 errores de disco al final" || bad "errores de disco" 0 "$e"

echo; echo "$pass ok, $fail fallos"
[ "$fail" -eq 0 ]
