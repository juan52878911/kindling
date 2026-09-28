#!/usr/bin/env bash
# Paso posterior a instanciar una copia de un dorado con Postgres vivo
# (kling run -from, kling sandbox fork, kling thaw).
#
#   scripts/db-post-thaw.sh [-max-skew S] [-restart] <máquina>
#
# El núcleo ya corrige solo, al restaurar, lo que puede (resyncGuest: reloj de
# pared y CSPRNG del kernel del invitado). Este script COMPRUEBA que se hizo y
# cubre lo que el núcleo no puede tocar, que vive dentro del proceso Postgres:
#
#   - espera a que el postmaster acepte conexiones;
#   - falla si el reloj del invitado o el que ve Postgres (now()) se desvía más
#     de S segundos del del host (por defecto 5): el resync no se aplicó;
#   - avisa si hay conexiones de cliente heredadas del dorado;
#   - con -restart, reinicia el postmaster (pg_ctl restart -m fast): es la única
#     forma de que su generador de claves de cancelación, sembrado en el dorado,
#     se resiembre. Cuesta el calor: la caché de Postgres vuelve vacía.
#
# Sale con 0 solo si todo está bien. Entorno: KLING, como en db-golden.sh.
set -euo pipefail

KLING="${KLING:-kling}"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

max_skew=5
restart=0
m=""
while [ $# -gt 0 ]; do
  case "$1" in
    -max-skew) max_skew="${2:?falta valor}"; shift 2 ;;
    -restart)  restart=1; shift ;;
    -*)        echo "db-post-thaw: opción desconocida: $1" >&2; exit 2 ;;
    *)         [ -z "$m" ] || { echo "db-post-thaw: sobra un argumento: $1" >&2; exit 2; }
               m="$1"; shift ;;
  esac
done
[ -n "$m" ] || { echo "uso: db-post-thaw.sh [-max-skew S] [-restart] <máquina>" >&2; exit 2; }
[[ "$max_skew" =~ ^[0-9]+$ ]] || { echo "db-post-thaw: -max-skew no válido" >&2; exit 2; }

pgx() { "${K[@]}" exec "$m" -- su -s /bin/sh postgres -c "$1"; }
fail=0
bad() { echo "post-thaw: FALLO: $*" >&2; fail=1; }

if [ "$restart" -eq 1 ]; then
  echo "post-thaw: reiniciando el postmaster"
  "${K[@]}" exec -timeout 2m "$m" -- sh -c \
    "su -s /bin/sh postgres -c 'pg_ctl -D /var/lib/postgresql/data -m fast -w -t 60 restart' </dev/null >/var/log/pgctl.out 2>&1 || { cat /var/log/pgctl.out; exit 1; }"
fi

ready=0
for _ in $(seq 1 30); do
  if pgx "pg_isready -q -h /run/postgresql" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" -eq 1 ] || { echo "post-thaw: FALLO: Postgres no acepta conexiones" >&2; exit 1; }

abs() { local v=$1; [ "$v" -ge 0 ] || v=$((-v)); echo "$v"; }

host_now="$(date +%s)"
guest_now="$("${K[@]}" exec "$m" -- date +%s)"
pg_now="$(pgx "psql -X -At -d postgres -c 'SELECT floor(extract(epoch FROM now()))::bigint'")"
[[ "$guest_now" =~ ^[0-9]+$ && "$pg_now" =~ ^[0-9]+$ ]] || { echo "post-thaw: FALLO: no se pudo leer el reloj" >&2; exit 1; }
sk_guest="$(abs $((guest_now - host_now)))"
sk_pg="$(abs $((pg_now - host_now)))"
echo "post-thaw: desvío del reloj frente al host: invitado ${sk_guest}s · now() de Postgres ${sk_pg}s"
[ "$sk_guest" -le "$max_skew" ] || bad "el reloj del invitado se desvía ${sk_guest}s: no se aplicó /resync (¿agente viejo? mira el log del daemon)"
[ "$sk_pg" -le "$max_skew" ] || bad "now() de Postgres se desvía ${sk_pg}s"

clients="$(pgx "psql -X -At -d postgres -c \"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()\"")"
if [ "$clients" != 0 ]; then
  echo "post-thaw: AVISO: $clients conexiones de cliente heredadas del dorado (comparten claves de cancelación con las demás copias)" >&2
fi

[ "$fail" -eq 0 ] && echo "post-thaw: ok"
exit "$fail"
