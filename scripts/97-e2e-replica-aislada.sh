#!/usr/bin/env bash
# e2e de la frontera de una réplica (ronda 9, decisión replica_auth): el
# puerto en que escucha una réplica de modelo lo alcanza el lado host de su
# veth y nada más de lo que se promete cerrar:
#
#   1. desde el host, por la ruta normal (origen = lado host del veth), el
#      puerto conecta (comprobación de que la prueba sirve);
#   2. desde OTRA microVM del mismo host (con red a internet, el caso más
#      abierto), el puerto no conecta;
#   3. desde el host con OTRO origen (la IP de la LAN o una de loopback
#      distinta, con --interface), el puerto no conecta.
#
# Lo que NO se promete y no se prueba: root del host, y en modo network
# cualquier proceso local que salga por el lado host del veth (eso es lo que
# replica_auth: key cubre). Solo Linux con KVM y un daemon en marcha.
#
#   KLING=kling IMGVOL=toolchain ./97-e2e-replica-aislada.sh
#
#   IMGVOL  imagen con python3 y wget (la toolchain de 90-e2e.sh)
#   PORT    puerto de la réplica (8090)
set -u

KLING="${KLING:-kling}"
IMGVOL="${IMGVOL:-toolchain}"
PORT="${PORT:-8090}"
REP="e2e-rep-$$"
OTRA="e2e-otra-$$"
pass=0
fail=0

ok()  { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad() { printf "  \033[31mFALLO\033[0m %s\n     esperaba: %s\n     obtuvo:   %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
skip() { printf "  \033[33msalto\033[0m %s\n" "$1"; }

limpiar() {
  $KLING rm "$REP" >/dev/null 2>&1
  $KLING rm "$OTRA" >/dev/null 2>&1
}
trap limpiar EXIT

if [ "$(uname -s)" != "Linux" ]; then
  echo "solo Linux: en macOS el puerto es un reenvío en 127.0.0.1 (docs/ai-gateway.md, modo network = desarrollo)"
  exit 0
fi
command -v curl >/dev/null || { echo "falta curl"; exit 2; }

# ip_de saca la IP de una máquina de `kling inspect` (JSON, campo "ip").
ip_de() {
  $KLING inspect "$1" 2>/dev/null | sed -n 's/^  "ip": "\([^"]*\)".*/\1/p' | head -1
}

echo "== réplica de prueba"
if ! $KLING run -name "$REP" -image "$IMGVOL" -allow-exec -ttl 10m >/dev/null 2>&1; then
  bad "arrancar la réplica" "kling run -image $IMGVOL" "falló"
  exit 1
fi
$KLING exec "$REP" -- sh -c "nohup python3 -m http.server $PORT >/dev/null 2>&1 &" >/dev/null 2>&1
IP=$(ip_de "$REP")
[ -n "$IP" ] || { bad "IP de la réplica" "inspect con ip" "vacía"; exit 1; }

# 1. Desde el host, ruta normal: tiene que conectar (si no, la prueba no vale).
conecta=no
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl -s -o /dev/null -m 2 "http://$IP:$PORT/"; then conecta=si; break; fi
  sleep 0.5
done
if [ "$conecta" = si ]; then ok "el host (lado host del veth) llega a $IP:$PORT"
else bad "el host llega a la réplica" "conecta" "no conecta"; exit 1; fi

# 2. Desde otra microVM del mismo host, con salida a internet.
echo "== desde otra microVM"
if $KLING run -name "$OTRA" -image "$IMGVOL" -allow-exec -egress internet -ttl 10m >/dev/null 2>&1; then
  out=$($KLING exec "$OTRA" -- sh -c "wget -q -T 3 -O /dev/null http://$IP:$PORT/ && echo CONECTA || echo NO" 2>/dev/null)
  case "$out" in
    *NO*) ok "otra microVM no llega al puerto de la réplica" ;;
    *) bad "otra microVM no llega" "NO" "$out" ;;
  esac
else
  bad "arrancar la segunda microVM" "kling run -egress internet" "falló"
fi

# 3. Desde el host con otro origen.
echo "== desde el host con otro origen"
lan=$(ip -4 -o route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
probados=0
for origen in $lan 127.0.0.2; do
  [ -n "$origen" ] || continue
  probados=$((probados+1))
  if curl -s -o /dev/null -m 3 --interface "$origen" "http://$IP:$PORT/"; then
    bad "origen $origen no llega" "no conecta" "conecta"
  else
    ok "con origen $origen no se llega al puerto de la réplica"
  fi
done
[ "$probados" -gt 0 ] || skip "no hay otro origen que probar en este host"

echo
echo "pasan $pass, fallan $fail"
[ "$fail" -eq 0 ]
