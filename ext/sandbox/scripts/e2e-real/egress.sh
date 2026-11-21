#!/usr/bin/env bash
set -uo pipefail
K="${KLING_BIN:-kling}"
M=pcore; pass=0; fail=0
ok(){ echo "  ok    $1"; pass=$((pass+1)); }
bad(){ echo "  FALLO $1 | esperaba: $2 | obtuvo: $3"; fail=$((fail+1)); }
# Ejecuta `egress set`; si falla, enseña la salida entera y cuenta un fallo (nunca un falso cerrado).
egress_set(){ local o rc; o=$($K egress set "$@" 2>&1); rc=$?
  [ -n "$o" ] && echo "$o" | sed 's/^/    /'
  [ $rc -eq 0 ] || { bad "egress set $*" "salida 0" "código $rc: $o"; echo "egress: $pass ok · $fail fallo(s)"; exit 1; }; }
# Abre TCP 443 con python y dice "abierto" o el error (sin depender de TLS).
sonda(){ $K exec $M -- python3 -c "
import socket,sys
try:
    socket.create_connection((sys.argv[1],443),timeout=5).close(); print('abierto')
except Exception as e: print('cerrado:',type(e).__name__)" "$1" 2>&1; }
egress_set -egress none $M
o=$(sonda example.com); [ "${o%%:*}" = cerrado ] && ok "none: example.com cerrado ($o)" || bad "none" cerrado "$o"
egress_set -egress allowlist -allow example.com $M
o=$(sonda example.com); [ "$o" = abierto ] && ok "allowlist: example.com abierto" || bad "allowlist example.com" abierto "$o"
o=$(sonda www.google.com); [ "${o%%:*}" = cerrado ] && ok "allowlist: google cerrado ($o)" || bad "allowlist google" cerrado "$o"
# conexión abierta antes de estrechar
$K exec -d $M -- python3 -c "
import socket,time
s=socket.create_connection(('example.com',443),timeout=5)
time.sleep(3)
try:
    s.sendall(b'x'*10); s.settimeout(5); print('viva', s.recv(1))
except Exception as e: print('cortada', type(e).__name__)" > /tmp/pid-viva 2>&1
sleep 1
egress_set -egress none $M
pid=$(cat /tmp/pid-viva); $K proc wait -timeout 20s $M "$pid" >/dev/null 2>&1
o=$($K proc out $M "$pid" 2>&1); echo "    conexión abierta al pasar a none: $o"
o=$(sonda example.com); [ "${o%%:*}" = cerrado ] && ok "vuelta a none: cerrado ($o)" || bad "vuelta a none" cerrado "$o"
o=$($K egress show $M 2>&1 | head -4); echo "$o" | sed 's/^/    /'
echo "egress: $pass ok · $fail fallo(s)"
