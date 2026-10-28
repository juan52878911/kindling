#!/usr/bin/env bash
# Batería de compatibilidad de imágenes Docker/OCI: importa cada imagen con
# `kling image import`, la arranca, hace una comprobación real de servicio
# (SQL, ping, HTTP) dentro de la máquina y la borra. Imprime una tabla con el
# resultado y los tiempos.
#
#   scripts/bateria-imagenes.sh [-k kling] [-s positivas|negativas|todas]
#                               [-o "postgres redis ..."] [-n] [-l]
#
#   -k   binario de kling (por defecto: el del PATH; el daemon es el de KLING_HOST)
#   -s   qué casos: positivas, negativas o todas (por defecto)
#   -o   solo estos casos (nombres separados por espacios)
#   -n   sin ulimits/sysctls: salta clickhouse y opensearch (daemon sin ellos, p. ej. 0.18)
#   -l   lista los casos y sale
#
# Todo lo que crea lleva el prefijo bat- (imágenes y máquinas) y es lo único que
# borra. Compatible con bash 3.2 (macOS): sin arrays asociativos.
# Positivas: el caso pasa si la comprobación responde. Negativas: pasa si
# el import o el arranque FALLAN con un mensaje que diga qué falta.
set -u

KLING=${KLING:-kling}
SUITE=todas
ONLY=
NOULIM=0
PFX=bat-
TMO=${BAT_TIMEOUT:-240}        # segundos hasta que responde la comprobación
RETRIES=${BAT_RETRIES:-2}      # intentos por caso positivo antes de darlo por rojo
PW=bateria-pw-$$

while getopts "k:s:o:nl" o; do
  case $o in
    k) KLING=$OPTARG ;;
    s) SUITE=$OPTARG ;;
    o) ONLY=$OPTARG ;;
    n) NOULIM=1 ;;
    l) LIST=1 ;;
    *) sed -n '2,16p' "$0"; exit 2 ;;
  esac
done

POS="postgres mysql mariadb mongo redis clickhouse opensearch nginx python zstd"
NEG="distroless otra-plataforma s6-overlay"
if [ "${LIST:-0}" = 1 ]; then echo "positivas: $POS"; echo "negativas: $NEG"; exit 0; fi

# arch_contraria: amd64 si el daemon es arm64 y al revés (de `kling status -v`;
# sin él, de uname -m, que es la del daemon cuando es local).
arch_contraria() {
  local a
  a=$("$KLING" status -v 2>/dev/null | awk '$1 == "arch:" {print $2; exit}')
  [ -z "$a" ] && case $(uname -m) in arm64|aarch64) a=arm64 ;; *) a=amd64 ;; esac
  case $a in arm64) echo amd64 ;; *) echo arm64 ;; esac
}

# Milisegundos: GNU date, si no perl (macOS), si no segundos.
now_ms() {
  local n
  n=$(date +%s%N 2>/dev/null)
  case $n in *N*|"") ;; *) echo $((n / 1000000)); return ;; esac
  if command -v perl >/dev/null 2>&1; then perl -MTime::HiRes=time -e 'printf "%d", time*1000'; return; fi
  echo $(($(date +%s) * 1000))
}
fmt_s() { # ms -> "1,2s"
  local ms=$1
  [ -z "$ms" ] && { echo -; return; }
  printf '%d.%ds' $((ms / 1000)) $(((ms % 1000) / 100))
}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bateria.XXXXXX")
RES="$WORK/resultados"
: > "$RES"
CREADAS=""   # máquinas e imágenes con prefijo bat-, para la limpieza final

limpiar() {
  local n
  for n in $CREADAS; do
    "$KLING" rm "$n" >/dev/null 2>&1 || true
    "$KLING" image rm "$n" >/dev/null 2>&1 || true
  done
}
trap 'limpiar; rm -rf "$WORK"' EXIT
trap 'exit 130' INT TERM

# definir <caso>: rellena ref, imgflags (import), cmd (tras --), runflags,
# mem, check (orden dentro de la máquina, con sh -c) y espera (regex de salida).
definir() {
  ref=; archive=; imgflags=; cmd=; runflags=; mem=512M; check=; espera=; arch=
  case $1 in
    postgres)
      ref=postgres:17-alpine; runflags="-e POSTGRES_PASSWORD=$PW"
      check="psql -U postgres -tAc 'select 40+2'"; espera=42 ;;
    mysql)
      ref=mysql:8.4; mem=1G; runflags="-e MYSQL_ROOT_PASSWORD=$PW"
      check="mysql -uroot -p$PW -N -e 'select 40+2'"; espera=42 ;;
    mariadb)
      ref=mariadb:11; mem=768M; runflags="-e MARIADB_ROOT_PASSWORD=$PW"
      check="mariadb -uroot -p$PW -N -e 'select 40+2'"; espera=42 ;;
    mongo)
      ref=mongo:7; mem=1G
      check="mongosh --quiet --eval 'db.runCommand({ping:1}).ok'"; espera=1 ;;
    redis)
      ref=redis:7-alpine; mem=256M; cmd="redis-server --requirepass $PW"
      # con contraseña responde PONG; sin ella, NOAUTH (la opción se aplicó de verdad)
      check="echo \"\$(redis-cli -a $PW --no-auth-warning ping) \$(redis-cli ping)\""; espera='PONG.*NOAUTH' ;;
    clickhouse)
      ref=clickhouse/clickhouse-server:24.8; mem=1G
      imgflags="-ulimit nofile=262144"
      runflags="-e CLICKHOUSE_SKIP_USER_SETUP=1"
      check="clickhouse-client -q 'select 40+2'"; espera=42 ;;
    opensearch)
      ref=opensearchproject/opensearch:2.17.1; mem=2G
      imgflags="-ulimit nofile=65536 -sysctl vm.max_map_count=262144"
      # kling no admite claves con punto en -e: los ajustes con punto van como -E tras --
      cmd="opensearch -Ediscovery.type=single-node"
      runflags="-e DISABLE_SECURITY_PLUGIN=true -e DISABLE_INSTALL_DEMO_CONFIG=true -e OPENSEARCH_JAVA_OPTS=-Xmx1g"
      check="curl -fsS 'http://127.0.0.1:9200/_cluster/health'"; espera='"status":"(green|yellow)"' ;;
    nginx)
      ref=nginx:alpine; mem=256M
      check="wget -qO- http://127.0.0.1/"; espera='Welcome to nginx' ;;
    python)
      ref=python:3.13-alpine; mem=256M; cmd="python -m http.server 8000 --bind 127.0.0.1"
      check="wget -qO- http://127.0.0.1:8000/"; espera='Directory listing' ;;
    # Negativas: espera es lo que debe decir el error para que sea claro.
    distroless)
      ref=gcr.io/distroless/static-debian12:nonroot; mem=128M
      espera='nothing to run' ;;
    zstd)
      # imagen con capas zstd (la construye docker buildx; ver preparar_zstd).
      # Es positiva: el import comprueba cada capa zstd por sha256 y la descomprime.
      ref=alpine-zstd:bateria; mem=128M; archive=1
      check="cat /hola"; espera=hola ;;
    otra-plataforma)
      # La arquitectura contraria a la del daemon: importa, pero no arranca
      # (con la del daemon, en un Mac arm64, arrancaba y el caso salía rojo).
      ref=nginx:alpine; arch=$(arch_contraria); mem=128M
      espera='amd64|arm64|platform|architecture|arch' ;;
    s6-overlay)
      ref=ghcr.io/linuxserver/nginx:latest; mem=256M
      espera='s6-overlay' ;;
  esac
}
# zstd: ningún registro popular publica capas zstd de forma estable, así que
# se construye un layout OCI con docker buildx (compression=zstd) o se usa uno
# ya hecho (BAT_ZSTD_ARCHIVE=ruta.tar). Sin ninguno de los dos, el caso se salta.
preparar_zstd() {
  [ -n "${BAT_ZSTD_ARCHIVE:-}" ] && { ZSTD_AR=$BAT_ZSTD_ARCHIVE; return 0; }
  command -v docker >/dev/null 2>&1 || return 1
  mkdir -p "$WORK/zs" && printf 'FROM alpine:3.20\nRUN echo hola > /hola\nCMD ["sleep","3600"]\n' > "$WORK/zs/Dockerfile"
  docker buildx build --platform linux/amd64 --output "type=oci,compression=zstd,force-compression=true,dest=$WORK/zstd-oci.tar" "$WORK/zs" >/dev/null 2>&1 || return 1
  ZSTD_AR=$WORK/zstd-oci.tar
}

registrar() { # caso tipo resultado import_ms listo_ms nota
  printf '%s|%s|%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" "$5" "$6" >> "$RES"
}

# una_positiva <caso>: un intento. Deja en $T_IMP, $T_LISTO y $NOTA.
una_positiva() {
  local c=$1 name="${PFX}$1" t0 t1 t2 out rc deadline
  T_IMP=; T_LISTO=; NOTA=; SALTA=0
  CREADAS="$CREADAS $name"
  "$KLING" rm "$name" >/dev/null 2>&1; "$KLING" image rm "$name" >/dev/null 2>&1
  t0=$(now_ms)
  # shellcheck disable=SC2086
  if [ -n "$archive" ]; then
    if ! preparar_zstd; then NOTA="sin docker buildx ni BAT_ZSTD_ARCHIVE"; SALTA=1; return 1; fi
    out=$("$KLING" image import -archive "$ZSTD_AR" -name "$name" -replace 2>&1); rc=$?
  elif [ -n "$cmd" ]; then
    out=$("$KLING" image import "$ref" -name "$name" -replace $imgflags -- $cmd 2>&1); rc=$?
  else
    out=$("$KLING" image import "$ref" -name "$name" -replace $imgflags 2>&1); rc=$?
  fi
  t1=$(now_ms); T_IMP=$((t1 - t0))
  if [ $rc -ne 0 ]; then NOTA="import: $(echo "$out" | tail -1 | cut -c1-140)"; return 1; fi
  # shellcheck disable=SC2086
  out=$("$KLING" run -image "$name" -name "$name" -mem $mem -allow-exec -wait-ready -ready-timeout ${TMO}s $runflags 2>&1); rc=$?
  if [ $rc -ne 0 ]; then
    NOTA="run: $(echo "$out" | tail -1 | cut -c1-140)"
    "$KLING" logs "$name" 2>&1 | tail -3 >> "$WORK/$c.log"
    return 1
  fi
  deadline=$(($(date +%s) + TMO))
  while :; do
    out=$("$KLING" exec "$name" -- sh -c "$check" 2>&1) && echo "$out" | grep -Eq "$espera" && break
    if [ "$(date +%s)" -ge $deadline ]; then
      NOTA="comprobación: $(echo "$out" | tail -1 | cut -c1-140)"
      "$KLING" logs "$name" 2>&1 | tail -5 >> "$WORK/$c.log"
      return 1
    fi
    sleep 1
  done
  t2=$(now_ms); T_LISTO=$((t2 - t1))
  return 0
}

correr_positiva() {
  local c=$1 intento=1 name="${PFX}$1" ok=0
  definir "$c"
  while [ $intento -le "$RETRIES" ]; do
    if una_positiva "$c"; then ok=1; break; fi
    [ "$SALTA" = 1 ] && break
    echo "  [$c] intento $intento: $NOTA" >&2
    "$KLING" rm "$name" >/dev/null 2>&1
    intento=$((intento + 1))
  done
  if [ $ok = 1 ]; then registrar "$c" pos OK "$T_IMP" "$T_LISTO" "intento $intento"
  elif [ "$SALTA" = 1 ]; then registrar "$c" pos SALTADA "" "" "$NOTA"
  else registrar "$c" pos ROJO "$T_IMP" "" "$NOTA"; fi
  "$KLING" rm "$name" >/dev/null 2>&1
  "$KLING" image rm "$name" >/dev/null 2>&1
}

correr_negativa() {
  local c=$1 name="${PFX}$1" out rc t0 t1 flags=
  definir "$c"
  [ -n "$arch" ] && flags="-arch $arch"
  CREADAS="$CREADAS $name"
  t0=$(now_ms)
  # shellcheck disable=SC2086
  out=$("$KLING" image import "$ref" -name "$name" -replace $flags 2>&1); rc=$?
  if [ $rc -eq 0 ]; then
    # importó: debe fallar al arrancar
    out=$("$KLING" run -image "$name" -name "$name" -mem $mem -allow-exec -wait-ready -ready-timeout 60s 2>&1); rc=$?
    if [ $rc -eq 0 ]; then
      t1=$(now_ms)
      registrar "$c" neg ROJO "$((t1 - t0))" "" "NO falló: importó y arrancó"
      "$KLING" rm "$name" >/dev/null 2>&1; "$KLING" image rm "$name" >/dev/null 2>&1
      return
    fi
    "$KLING" logs "$name" 2>&1 | tail -3 >> "$WORK/$c.log"
    out="$out $(tail -3 "$WORK/$c.log" 2>/dev/null)"
  fi
  t1=$(now_ms)
  local msg; msg=$(echo "$out" | grep -v '^$' | tail -2 | tr '\n' ' ' | cut -c1-160)
  if echo "$out" | grep -Eiq "$espera"; then registrar "$c" neg OK "$((t1 - t0))" "" "$msg"
  else registrar "$c" neg ROJO "$((t1 - t0))" "" "mensaje poco claro: $msg"; fi
  "$KLING" rm "$name" >/dev/null 2>&1; "$KLING" image rm "$name" >/dev/null 2>&1
}

quiere() { [ -z "$ONLY" ] && return 0; case " $ONLY " in *" $1 "*) return 0 ;; esac; return 1; }

case $SUITE in positivas|todas)
  for c in $POS; do
    quiere "$c" || continue
    if [ $NOULIM = 1 ]; then case $c in clickhouse|opensearch) registrar "$c" pos SALTADA "" "" "necesita ulimits/sysctls (-n)"; continue ;; esac; fi
    echo "== $c" >&2; correr_positiva "$c"
  done ;;
esac
case $SUITE in negativas|todas)
  for c in $NEG; do
    quiere "$c" || continue
    echo "== $c (negativa)" >&2; correr_negativa "$c"
  done ;;
esac

echo
echo "kling: $("$KLING" version 2>&1 | tr '\n' ' ')"
printf '%-16s %-4s %-8s %10s %10s  %s\n' CASO TIPO RESULTADO IMPORT LISTO NOTA
while IFS='|' read -r c t r i l n; do
  printf '%-16s %-4s %-8s %10s %10s  %s\n' "$c" "$t" "$r" "$(fmt_s "$i")" "$(fmt_s "$l")" "$n"
done < "$RES"
! grep -q '|ROJO|' "$RES"
