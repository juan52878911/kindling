#!/usr/bin/env bash
# kling db: plantilla de Postgres CON extensiones compiladas (pg16-ext, pg17-ext).
#
#   scripts/db-image-ext.sh [-pg 16|17] [-only timescaledb,vector,...] [-keep] [<nombre>]
#   kling db golden image -ext [...]                 lo mismo desde kling db
#
# Alpine (la base de las imágenes pgNN) no tiene TimescaleDB ni pgvector para
# PG16/PG17 (solo para PG18), y su TimescaleDB es la build Apache, sin
# compresión. Este script compila las extensiones contra el Postgres de la
# imagen dentro de una microVM y deja una PLANTILLA con Postgres y las
# extensiones instaladas, de la que sale el golden:
#
#   kling db golden build -from pg16-ext -migrations init/ ... <golden>
#
# Extensiones (versiones fijas, comprobadas por commit):
#   timescaledb  2.30.2  licencia TSL: compresión, políticas, agregados continuos
#   vector       0.8.0   pgvector
#   postgis      3.6.4   sin raster (GDAL), topology ni protobuf (ST_AsMVT)
#   pg_cron      1.6.8
#   pg_partman   5.5.0
# Más las de contrib que ya trae la imagen (uuid-ossp, pgcrypto, pg_trgm...).
#
# Cómo, y por qué así:
#   - La microVM de construcción tiene salida a internet (apk y git) y un
#     volumen de 3 GiB: el overlay de 512 MiB no cabe la toolchain. La
#     toolchain va en una raíz apk dentro del volumen (apk --root) y se compila
#     en chroot. apk comprueba las firmas con las claves de la imagen.
#   - Cada fuente se clona por su etiqueta y se comprueba el commit: una
#     etiqueta movida en el origen no cuela otro código.
#   - Lleva bash, para los .sh de init al estilo Docker (golden build -init).
#   - La plantilla NO tiene red (egress none: lo heredan sus copias). Las
#     librerías que necesita PostGIS (geos, proj...) se bajan como .apk en la
#     máquina de construcción y se instalan sin red en la de la plantilla, con
#     la firma comprobada.
#   - postgresql.conf.sample precarga timescaledb (CREATE EXTENSION timescaledb
#     lo exige) y apaga su telemetría: initdb lo copia a cada golden. Para
#     precargar más (pg_cron), `golden build -preload`.
#
# Entorno: KLING = comando kling (con sus banderas). TSV, PGVECTORV... cambian
# versión y commit a la vez (ver VERSIONES abajo); no se recomienda.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)
HERE="$(cd "$(dirname "$0")" && pwd)"

die() { echo "db-image-ext: $*" >&2; exit 1; }
say() { echo "db-image-ext: $*"; }

# VERSIONES: repositorio, etiqueta y commit de cada fuente. Una función y no
# arrays asociativos: la bash de macOS (3.2) no los tiene.
fuente() {
  case "$1" in
    timescaledb) echo "https://github.com/timescale/timescaledb.git 2.30.2 b0977bf2d3a5014ee870247be09bc19d3e9f7a5f" ;;
    vector)      echo "https://github.com/pgvector/pgvector.git v0.8.0 2627c5ff775ae6d7aef0c430121ccf857842d2f2" ;;
    postgis)     echo "https://github.com/postgis/postgis.git 3.6.4 94d984bd083635c1d253db0f87cf80b32548e406" ;;
    pg_cron)     echo "https://github.com/citusdata/pg_cron.git v1.6.8 5cedfa472ccc83567aa23ec645925ed8489a7797" ;;
    pg_partman)  echo "https://github.com/pgpartman/pg_partman.git v5.5.0 f7e83b9c441c7e97066d815bbe14e02a9dc5ff94" ;;
    *) return 1 ;;
  esac
}
TODAS=timescaledb,vector,postgis,pg_cron,pg_partman

pg=16 exts=$TODAS name="" keep=0
while [ $# -gt 0 ]; do
  case "$1" in
    -pg)   pg="${2:?missing value}"; shift 2 ;;
    -only) exts="${2:?missing value}"; shift 2 ;;
    -keep) keep=1; shift ;;
    -h|-help|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*)    die "unknown option: $1" ;;
    *)     [ -z "$name" ] || die "extra argument: $1"; name="$1"; shift ;;
  esac
done
case "$pg" in 16|17) ;; *) die "-pg must be 16 or 17" ;; esac
[ -n "$name" ] || name="pg$pg-ext"
[[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "invalid template name: $name"
IFS=, read -r -a lista <<<"$exts"
for e in "${lista[@]}"; do
  fuente "$e" >/dev/null || die "unknown extension $e (known: $TODAS)"
done
image="pg$pg"

B="$name-build" P="$name-tpl" V="$name-vol"
TMP="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP"
  [ "$keep" -eq 1 ] && return
  "${K[@]}" rm -f "$B" >/dev/null 2>&1 || true
  "${K[@]}" rm -f "$P" >/dev/null 2>&1 || true
  "${K[@]}" volume rm -f "$V" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
T0=$(date +%s)
t() { say "[$(( $(date +%s) - T0 ))s] $*"; }

# La imagen base: la de siempre, construida con su receta si falta.
if ! "${K[@]}" image ls -q 2>/dev/null | grep -qx "$image"; then
  [ -f "$HERE/recipes/$image.recipe.json" ] || die "missing scripts/recipes/$image.recipe.json"
  t "building image $image first"
  "${K[@]}" image build "$image" -builder base -base min -grow 512 -spec "$HERE/recipes/$image.recipe.json"
fi

"${K[@]}" rm -f "$B" "$P" >/dev/null 2>&1 || true
"${K[@]}" volume rm -f "$V" >/dev/null 2>&1 || true
"${K[@]}" volume create "$V" -size 3G >/dev/null
t "build microVM from $image (internet egress, 3 GiB volume)"
"${K[@]}" run -name "$B" -image "$image" -egress internet -allow-exec -mem 3G -cpus 4 -cpu-pct 100 -volume "$V:/build" >/dev/null
for i in $(seq 1 60); do
  "${K[@]}" exec -timeout 5s "$B" -- true >/dev/null 2>&1 && break
  [ "$i" -lt 60 ] || die "the guest agent does not answer"
  sleep 1
done

# Paquetes de compilación y, solo con postgis, sus librerías de ejecución.
build_pkgs="alpine-baselayout busybox build-base git postgresql$pg postgresql$pg-dev"
# bash: los scripts de init al estilo de Docker (-init DIR con .sh) lo suelen
# pedir, y la imagen pgNN solo trae el sh de busybox.
run_pkgs="bash"
for e in "${lista[@]}"; do
  case "$e" in
    timescaledb) build_pkgs+=" cmake openssl-dev" ;;
    postgis)     build_pkgs+=" autoconf automake libtool pkgconf bison flex perl geos-dev proj-dev json-c-dev libxml2-dev pcre2-dev"
                 run_pkgs+=" geos proj json-c libxml2 pcre2" ;;
  esac
done

t "toolchain into the volume (apk --root, signatures checked)"
"${K[@]}" exec -i -timeout 15m "$B" -- sh -s <<EOF
set -eu
R=/build/root
mkdir -p \$R/etc/apk
cp -r /etc/apk/keys \$R/etc/apk/
cp /etc/apk/repositories \$R/etc/apk/
# Con reintentos: un índice que no bajó entero da "no such package" de
# paquetes que existen (visto en el laboratorio).
ok=0
for i in 1 2 3; do
  apk add --root \$R --initdb --update-cache $build_pkgs >/build/apk.log 2>&1 && { ok=1; break; }
  sleep 5
done
[ \$ok = 1 ] || { tail -20 /build/apk.log; exit 1; }
mount -t proc proc \$R/proc
mount --bind /dev \$R/dev
cp /etc/resolv.conf \$R/etc/
mkdir -p \$R/stage \$R/src /build/apks
if [ -n "$run_pkgs" ]; then
  cd /build/apks
  ok=0
  for i in 1 2 3; do apk fetch --recursive $run_pkgs >/build/fetch.log 2>&1 && { ok=1; break; }; sleep 5; done
  [ \$ok = 1 ] || { tail -20 /build/fetch.log; exit 1; }
fi
EOF

# El script de compilación, por stdin (sin comillas que escapar en argv).
{
  echo 'set -eu'
  echo "export PATH=/usr/libexec/postgresql$pg:\$PATH PG_CONFIG=/usr/libexec/postgresql$pg/pg_config"
  echo 'fetch() { git -c advice.detachedHead=false clone -q --depth 1 --branch "$2" "$1" "/src/$4"; got=$(git -C "/src/$4" rev-parse HEAD); [ "$got" = "$3" ] || { echo "$4: tag $2 is $got, expected $3" >&2; exit 1; }; }'
  for e in "${lista[@]}"; do
    read -r repo tag commit <<<"$(fuente "$e")"
    echo "fetch $repo $tag $commit $e"
    echo "echo '== $e $tag'"
    case "$e" in
      timescaledb)
        echo 'cd /src/timescaledb && mkdir -p build && cd build'
        echo 'cmake .. -DCMAKE_BUILD_TYPE=Release -DREGRESS_CHECKS=OFF -DTAP_CHECKS=OFF -DWARNINGS_AS_ERRORS=OFF -DPG_CONFIG=$PG_CONFIG -DSEND_TELEMETRY_DEFAULT=NO -DUSE_TELEMETRY=OFF -DGENERATE_DOWNGRADE_SCRIPT=OFF >/tmp/ts.log 2>&1 || { tail -30 /tmp/ts.log; exit 1; }'
        echo 'make -j4 >>/tmp/ts.log 2>&1 && make install DESTDIR=/stage >>/tmp/ts.log 2>&1 || { tail -30 /tmp/ts.log; exit 1; }' ;;
      vector)
        # OPTFLAGS="": sin -march=native, el binario sirve en cualquier CPU.
        echo 'cd /src/vector && make OPTFLAGS="" with_llvm=no -j4 >/tmp/vector.log 2>&1 && make install with_llvm=no DESTDIR=/stage >>/tmp/vector.log 2>&1 || { tail -30 /tmp/vector.log; exit 1; }' ;;
      postgis)
        echo 'cd /src/postgis && ./autogen.sh >/tmp/postgis.log 2>&1 && ./configure --without-raster --without-topology --without-protobuf --without-gui --without-interrupt-tests --with-pgconfig=$PG_CONFIG >>/tmp/postgis.log 2>&1 || { tail -40 /tmp/postgis.log; exit 1; }'
        # Su make -j tiene carreras al generar los scripts SQL (topology.sql,
        # rtpostgis.sql: en el laboratorio salieron rotos, y un reintento en
        # serie no los rehace). En paralelo solo lo pesado en C; el resto, en
        # serie.
        echo 'make -j4 -C liblwgeom >>/tmp/postgis.log 2>&1 && make -j4 -C libpgcommon >>/tmp/postgis.log 2>&1 && make -j4 -C postgis >>/tmp/postgis.log 2>&1 && make >>/tmp/postgis.log 2>&1 && make install DESTDIR=/stage >>/tmp/postgis.log 2>&1 || { tail -40 /tmp/postgis.log; exit 1; }' ;;
      pg_cron)
        echo 'cd /src/pg_cron && make with_llvm=no -j4 >/tmp/cron.log 2>&1 && make install with_llvm=no DESTDIR=/stage >>/tmp/cron.log 2>&1 || { tail -30 /tmp/cron.log; exit 1; }' ;;
      pg_partman)
        echo 'cd /src/pg_partman && make with_llvm=no -j4 >/tmp/partman.log 2>&1 && make install with_llvm=no DESTDIR=/stage >>/tmp/partman.log 2>&1 || { tail -30 /tmp/partman.log; exit 1; }' ;;
    esac
  done
} > "$TMP/compile.sh"
"${K[@]}" cp "$TMP/compile.sh" "$B:/build/root/compile.sh" >/dev/null
t "compiling ${exts//,/, } against PostgreSQL $pg"
# Desacoplada del exec: una conexión de minutos sin salida se cayó en el
# laboratorio (read: connection timed out) a media compilación. Corre con
# setsid y deja su código en compile.rc; aquí se mira cada 10 s.
"${K[@]}" exec "$B" -- sh -c 'rm -f /build/compile.rc; setsid sh -c "chroot /build/root /bin/sh /compile.sh >/build/compile.log 2>&1; echo \$? >/build/compile.rc" </dev/null >/dev/null 2>&1 &'
visto=""
for i in $(seq 1 360); do
  sleep 10
  rc="$("${K[@]}" exec -timeout 30s "$B" -- sh -c 'cat /build/compile.rc 2>/dev/null; true' 2>/dev/null || true)"
  ult="$("${K[@]}" exec -timeout 30s "$B" -- sh -c "grep '^== ' /build/compile.log | tail -n1" 2>/dev/null || true)"
  if [ -n "$ult" ] && [ "$ult" != "$visto" ]; then t "  ${ult#== }"; visto="$ult"; fi
  [ -z "$rc" ] || break
  [ "$i" -lt 360 ] || die "the build did not finish in 60 min"
done
if [ "$rc" != 0 ]; then
  "${K[@]}" exec "$B" -- tail -n 40 /build/compile.log >&2 || true
  die "compiling failed (exit $rc)"
fi

# Lo instalado, sin cabeceras, bitcode, documentación ni símbolos de
# depuración; ni los scripts de ACTUALIZACIÓN entre versiones (nombre--a--b.sql:
# solo sirven para ALTER EXTENSION UPDATE desde una versión que esta imagen
# nunca tuvo, y en TimescaleDB y PostGIS son decenas de MiB). Y los .apk de
# ejecución.
"${K[@]}" exec "$B" -- chroot /build/root sh -c 'cd /stage && find usr/share -path "*/extension/*--*--*.sql" -delete && rm -rf usr/share/doc usr/share/man && find usr/lib -name "*.so" -exec strip --strip-unneeded {} +'
"${K[@]}" exec "$B" -- sh -c 'cd /build/root/stage && tar czf /build/ext.tgz --exclude=usr/include --exclude="*.bc" usr && cd /build && tar czf /build/apks.tgz apks'
"${K[@]}" cp "$B:/build/ext.tgz" "$TMP/ext.tgz" >/dev/null
"${K[@]}" cp "$B:/build/apks.tgz" "$TMP/apks.tgz" >/dev/null
"${K[@]}" rm -f "$B" >/dev/null
"${K[@]}" volume rm -f "$V" >/dev/null
t "built: $(du -h "$TMP/ext.tgz" | cut -f1) of extensions, $(du -h "$TMP/apks.tgz" | cut -f1) of runtime packages"

t "template machine (no network)"
"${K[@]}" run -name "$P" -image "$image" -egress none -allow-exec -mem 512M -cpus 1 >/dev/null
for i in $(seq 1 60); do
  "${K[@]}" exec -timeout 5s "$P" -- true >/dev/null 2>&1 && break
  [ "$i" -lt 60 ] || die "the guest agent does not answer"
  sleep 1
done
# Lo de paso va a un tmpfs: en el overlay de la plantilla solo queda lo
# instalado (un fichero borrado del overlay sigue ocupando su disco).
"${K[@]}" exec "$P" -- sh -c 'mkdir -p /run/kdb && mount -t tmpfs -o size=256m tmpfs /run/kdb'
"${K[@]}" cp "$TMP/ext.tgz" "$P:/run/kdb/ext.tgz" >/dev/null
"${K[@]}" cp "$TMP/apks.tgz" "$P:/run/kdb/apks.tgz" >/dev/null
"${K[@]}" exec -i -timeout 10m "$P" -- sh -s <<INSTALAR
set -eu
cd /run/kdb
tar xzf apks.tgz
set -- apks/*.apk
if [ -e "\$1" ]; then
  # Sin red y con la firma comprobada (las claves de la imagen).
  apk add --no-network --no-cache --repositories-file /dev/null "\$@" >/run/kdb/apk.log 2>&1 || { cat /run/kdb/apk.log; exit 1; }
fi
tar xzf ext.tgz -C /
C=/usr/share/postgresql$pg/postgresql.conf.sample
if [ -f /usr/lib/postgresql$pg/timescaledb.so ]; then
  sed -i "s/^#shared_preload_libraries = ''/shared_preload_libraries = 'timescaledb'/" "\$C"
  grep -q "^shared_preload_libraries = 'timescaledb'" "\$C" || echo "shared_preload_libraries = 'timescaledb'" >> "\$C"
  echo "timescaledb.telemetry_level = off" >> "\$C"
fi
# Lo que hay, para que se pueda preguntar sin arrancar Postgres.
ls /usr/share/postgresql$pg/extension/*.control 2>/dev/null | sed 's#.*/##; s#\.control##' | sort > /usr/share/kling-db-extensions
cd /; umount /run/kdb
sync; echo 3 > /proc/sys/vm/drop_caches
INSTALAR
# La RAM que la instalación tocó, de vuelta al host antes de congelar: el
# volcado de la plantilla queda con huecos en vez de caché de ficheros.
"${K[@]}" squeeze "$P" >/dev/null 2>&1 || true

t "saving template $name"
"${K[@]}" save -replace -warm=false "$P" "$name" >/dev/null
"${K[@]}" rm -f "$P" >/dev/null 2>&1 || true

# Comprobación en una instancia de usar y tirar de la plantilla (no en ella:
# arrancar Postgres llenaría su memoria): un cluster en un tmpfs con cada
# extensión creada y la compresión de TimescaleDB, la que la build Apache no
# tiene. Si falla, la plantilla se borra.
t "checking a throwaway instance: CREATE EXTENSION of each one, and timescaledb compression"
{
  for e in "${lista[@]}"; do
    case "$e" in
      pg_cron) ;; # necesita shared_preload_libraries y cron.database_name: lo prueba -preload
      *) echo "CREATE EXTENSION IF NOT EXISTS \"$e\" CASCADE;" ;;
    esac
  done
  if [[ ",$exts," == *,timescaledb,* ]]; then
    echo "CREATE TABLE kdb_t (t timestamptz NOT NULL, v int);"
    echo "SELECT create_hypertable('kdb_t', 't');"
    echo "ALTER TABLE kdb_t SET (timescaledb.compress);"
    echo "SELECT add_compression_policy('kdb_t', INTERVAL '7 days');"
  fi
  echo "SELECT extname || ' ' || extversion FROM pg_extension ORDER BY 1;"
} > "$TMP/check.sql"
P="$name-check"
"${K[@]}" rm -f "$P" >/dev/null 2>&1 || true
"${K[@]}" run -name "$P" -from "$name" -egress none -allow-exec >/dev/null
"${K[@]}" cp "$TMP/check.sql" "$P:/tmp/check.sql" >/dev/null
ok=0
"${K[@]}" exec -timeout 5m "$P" -- sh -c '
  set -e; chmod 644 /tmp/check.sql; D=/run/kdbcheck; mkdir -p $D; mount -t tmpfs -o size=256m tmpfs $D; chown postgres $D; chmod 700 $D
  mkdir -p /run/postgresql; chown postgres /run/postgresql
  su -s /bin/sh postgres -c "initdb -D $D -E UTF8 --locale=C.UTF-8 >/tmp/kdbcheck.log 2>&1 && pg_ctl -D $D -o \"-c listen_addresses= -c dynamic_shared_memory_type=mmap\" -l /tmp/kdbcheck.pg -w start >>/tmp/kdbcheck.log 2>&1" || { cat /tmp/kdbcheck.log /tmp/kdbcheck.pg; exit 1; }
  su -s /bin/sh postgres -c "psql -X -q -At -v ON_ERROR_STOP=1 -d postgres -f /tmp/check.sql"' || ok=$?
"${K[@]}" rm -f "$P" >/dev/null 2>&1 || true
if [ "$ok" -ne 0 ]; then
  "${K[@]}" template rm -f "$name" >/dev/null 2>&1 || true
  die "the check failed: template $name removed"
fi
"${K[@]}" template ls 2>/dev/null | awk -v n="$name" '$1 == n'
t "done: kling db golden build -from $name [-extension ...] -migrations DIR <golden>"
