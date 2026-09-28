#!/usr/bin/env bash
# Comprobaciones de un dorado de Postgres vivo, para correr en el lab con el
# daemon (no hay nada que probar sin microVMs).
#
#   scripts/db-golden-verify.sh [-n COPIAS] [-keep] <plantilla>
#
# Levanta N copias de la plantilla (kling run -from) y comprueba, en cada una y
# entre ellas:
#
#   1. db-post-thaw.sh: Postgres acepta conexiones y el reloj del invitado y
#      now() no se desvían del host (el /resync del núcleo se aplicó).
#   2. Conexión por red como el rol de la app, con SCRAM (contraseña por stdin).
#   3. pg_stat_activity: solo la conexión de la propia comprobación, sin
#      clientes heredados del dorado.
#   4. Escrituras en paralelo en todas las copias a la vez: cada una ve SOLO
#      lo suyo (los overlays son independientes) y los datos del seed están.
#   5. Aleatoriedad: gen_random_uuid() y random() distintos entre copias (el
#      CSPRNG se resembró y el backend no heredó estado repetido).
#   6. pg_backend_pid(): se INFORMA de los duplicados entre copias. Son normales
#      (misma tabla de PIDs en máquinas distintas) y no son un fallo.
#
# Sale con 0 solo si pasan 1 a 5. Entorno: KLING, como en db-golden.sh;
# KLING_DB_STATE, donde db-golden.sh dejó la contraseña.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

n=3
keep=0
tpl=""
while [ $# -gt 0 ]; do
  case "$1" in
    -n)    n="${2:?falta valor}"; shift 2 ;;
    -keep) keep=1; shift ;;
    -*)    echo "db-golden-verify: opción desconocida: $1" >&2; exit 2 ;;
    *)     [ -z "$tpl" ] || { echo "db-golden-verify: sobra un argumento: $1" >&2; exit 2; }
           tpl="$1"; shift ;;
  esac
done
[ -n "$tpl" ] || { echo "uso: db-golden-verify.sh [-n COPIAS] [-keep] <plantilla>" >&2; exit 2; }
[[ "$n" =~ ^[0-9]+$ ]] && [ "$n" -ge 2 ] && [ "$n" -le 16 ] || { echo "db-golden-verify: -n entre 2 y 16" >&2; exit 2; }
[[ "$tpl" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || { echo "db-golden-verify: plantilla no válida" >&2; exit 2; }

state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}/$tpl"
[ -f "$state/password" ] && [ -f "$state/conn.env" ] || { echo "db-golden-verify: faltan $state/password y conn.env (¿corriste db-golden.sh build $tpl en este host?)" >&2; exit 2; }
# conn.env solo lleva PGUSER, PGDATABASE, GUEST_IP y TEMPLATE, sin secretos.
# shellcheck disable=SC1091
. "$state/conn.env"
[[ "$PGUSER" =~ ^[a-z_][a-z0-9_]{0,30}$ && "$PGDATABASE" =~ ^[a-z_][a-z0-9_]{0,30}$ && "$GUEST_IP" =~ ^[0-9.]+$ ]] || { echo "db-golden-verify: conn.env no válido" >&2; exit 2; }

tmp="$(mktemp -d)"
tag="vfy$$"
copies=()
cleanup() {
  rm -rf "$tmp"
  [ "$keep" -eq 1 ] && return 0
  local c
  for c in "${copies[@]}"; do "${K[@]}" rm -f "$c" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail=0
check() { # check <ok 0|1> <mensaje>
  if [ "$1" -eq 0 ]; then echo "  ok     $2"; else echo "  FALLO  $2"; fail=1; fi
}

# psql_app <copia> <sql>: como el rol de la app, por TCP a la IP del invitado.
# La contraseña entra por stdin y solo vive en una variable del shell del
# invitado, nunca en argv.
psql_app() {
  "${K[@]}" exec -i -timeout 5m "$1" -- sh -c \
    'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -At -v ON_ERROR_STOP=1 -h "$1" -U "$2" -d "$3" -c "$4"' \
    sh "$GUEST_IP" "$PGUSER" "$PGDATABASE" "$2" < "$state/password"
}

echo "== levantando $n copias de $tpl"
i=1
while [ "$i" -le "$n" ]; do
  c="$tag-$i"
  copies+=("$c")
  "${K[@]}" run -from "$tpl" -name "$c" >/dev/null
  i=$((i + 1))
done

echo "== 1. post-thaw (reloj y arranque)"
for c in "${copies[@]}"; do
  if KLING="$KLING" "$HERE/db-post-thaw.sh" "$c" >"$tmp/pt.$c" 2>&1; then
    check 0 "$c: $(grep 'desvío' "$tmp/pt.$c" | sed 's/^post-thaw: //')"
  else
    check 1 "$c: $(tr '\n' ' ' < "$tmp/pt.$c")"
  fi
done

echo "== 2, 3, 5, 6. conexión SCRAM, pg_stat_activity, aleatoriedad, PIDs"
q="SELECT pg_backend_pid(), gen_random_uuid(), random(), (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'), pg_postmaster_start_time()"
for c in "${copies[@]}"; do
  if psql_app "$c" "$q" >"$tmp/id.$c" 2>"$tmp/id.$c.err"; then
    check 0 "$c: conecta por red con SCRAM"
  else
    check 1 "$c: no conecta como $PGUSER por SCRAM: $(tr '\n' ' ' < "$tmp/id.$c.err")"
    : > "$tmp/id.$c"
  fi
done
t() { # t <mensaje> <comando...>: el comando decide si pasa
  local msg=$1; shift
  if "$@"; then check 0 "$msg"; else check 1 "$msg"; fi
}
for c in "${copies[@]}"; do
  pid="" uuid="" rnd="" cli="" started=""
  IFS='|' read -r pid uuid rnd cli started < "$tmp/id.$c" || true
  echo "$pid" >> "$tmp/pids"; echo "$uuid" >> "$tmp/uuids"; echo "$rnd" >> "$tmp/rnds"
  # Solo la conexión de esta comprobación: cualquier otra es heredada.
  t "$c: pg_stat_activity con '${cli}' cliente(s) (esperado: 1, este); postmaster arrancado ${started}" [ "$cli" = 1 ]
done
dup_u="$(sort "$tmp/uuids" | uniq -d | wc -l | tr -d ' ')"
dup_r="$(sort "$tmp/rnds" | uniq -d | wc -l | tr -d ' ')"
t "gen_random_uuid() distinto en las $n copias" [ "$dup_u" -eq 0 ]
t "random() distinto en las $n copias" [ "$dup_r" -eq 0 ]
dup_p="$(sort "$tmp/pids" | uniq -d | wc -l | tr -d ' ')"
echo "  info   pg_backend_pid repetidos entre copias: $dup_p de $n (normal: cada copia es una máquina distinta)"

echo "== 4. escrituras en paralelo en las $n copias"
pids=()
for c in "${copies[@]}"; do
  sql="CREATE TABLE IF NOT EXISTS verify_writes (tag text, n int); INSERT INTO verify_writes SELECT '$c', g FROM generate_series(1, 20000) g; SELECT count(*), count(DISTINCT tag), max(tag) FROM verify_writes"
  ( psql_app "$c" "$sql" >"$tmp/w.$c" 2>&1 ) &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p" || true; done
for c in "${copies[@]}"; do
  # psql -c con varias sentencias imprime solo la última: cuenta|etiquetas|etiqueta
  last="$(tail -n1 "$tmp/w.$c" || true)"
  t "$c: ve solo sus 20000 filas (salió '${last:-nada}')" [ "$last" = "20000|1|$c" ]
done
seed="$(psql_app "${copies[0]}" "SELECT CASE WHEN to_regclass('seed_events') IS NULL THEN 'sin-seed' ELSE (SELECT count(*)::text FROM seed_events) END" 2>/dev/null | tail -n1 || true)"
echo "  info   filas de seed_events en la primera copia: ${seed:-?}"

echo
if [ "$fail" -eq 0 ]; then echo "db-golden-verify: todo bien ($n copias de $tpl)"; else echo "db-golden-verify: HAY FALLOS (copias con -keep para investigar)"; fi
exit "$fail"
