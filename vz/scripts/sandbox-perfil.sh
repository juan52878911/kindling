#!/bin/sh
# Prueba el perfil de sandbox de kling-vz (cmd/kling-vz/kling-vz.sb) con
# sandbox-exec: qué puede tocar un kling-vz tomado por su invitado.
#
#   vz/scripts/sandbox-perfil.sh                 # el perfil del árbol
#   vz/scripts/sandbox-perfil.sh viejo.sb        # otro (p. ej. el de main)
#
# Monta una raíz de kindling falsa con dos máquinas, secrets/, dorados y
# volúmenes, abre un "reenvío" de otra máquina en 127.0.0.1 dentro del rango
# reservado y un servicio cualquiera del loopback fuera de él, y ejecuta cada
# prueba dentro del perfil con los parámetros que kling-vz le pasaría a la
# máquina m1 (lee su kernel y su dorado, escribe su overlay y su volumen). A
# las herramientas de prueba (sh, cat, perl) se les deja ejecutarse y leer el
# sistema; eso va justo tras el import de system.sb, así que las reglas del
# perfil que vienen después (las de deny del final) siguen mandando.
#
# Cada línea dice lo que pasó y lo que debe pasar; sale con 1 si algo no
# coincide. Con un perfil viejo, las diferencias son los agujeros.
set -u
AQUI=$(cd "$(dirname "$0")/.." && pwd)
PERFIL=${1:-$AQUI/cmd/kling-vz/kling-vz.sb}
FMIN=29000 FMAX=29999

T=$(mktemp -d /tmp/kling-vz-sb.XXXXXX)
T=$(cd "$T" && pwd -P)
R=$T/root
M=$R/machines/m1
trap 'kill $PIDS 2>/dev/null; rm -rf "$T"' EXIT INT TERM
PIDS=
mkdir -p "$M" "$R/machines/m2" "$R/secrets" "$R/images" "$R/volumes" \
	"$R/snapshots/suyo" "$R/snapshots/ajeno"
echo clave-maestra >"$R/secrets/snapshot.key"
echo kernel >"$R/images/vmlinux"
echo base >"$R/images/base.ext4"
echo ajeno >"$R/machines/m2/credentials.enc"
echo overlay >"$M/overlay.ext4"
echo suyo >"$R/volumes/suyo.ext4"
echo otro >"$R/volumes/otro.ext4"
echo memoria >"$R/snapshots/suyo/mem.file"
echo memoria >"$R/snapshots/ajeno/mem.file"

# Puertos libres: uno del rango de reenvíos (el agente de otra máquina) y uno
# fuera de él (un Postgres de Docker, por ejemplo).
libre() {
	perl -MIO::Socket::INET -e '
		for my $p ($ARGV[0]..$ARGV[1]) {
			my $s = IO::Socket::INET->new(LocalAddr=>"127.0.0.1", LocalPort=>$p, Listen=>1, ReuseAddr=>1) or next;
			close $s; print $p; exit 0 } exit 1' "$1" "$2"
}
escuchar() {
	perl -MIO::Socket::INET -e '
		my $s = IO::Socket::INET->new(LocalAddr=>"127.0.0.1", LocalPort=>$ARGV[0], Listen=>50, ReuseAddr=>1) or die;
		while (my $c = $s->accept) { close $c }' "$1" &
	PIDS="$PIDS $!"
}
REENVIO=$(libre $((FMIN + 500)) $FMAX)
BIND=$(libre $((FMIN + 100)) $((FMIN + 499)))
OTRO=$(libre 28000 28999)
escuchar "$REENVIO"
escuchar "$OTRO"
sleep 0.3

# El perfil con las herramientas de prueba permitidas tras el import.
awk '{ print } /^\(import "system.sb"\)/ {
	print "(allow process-exec* process-fork)"
	print "(allow file-read* (subpath \"/bin\") (subpath \"/usr\") (subpath \"/System\") (subpath \"/Library\") (subpath \"/private/var/db\") (subpath \"/dev\"))"
}' "$PERFIL" >"$T/perfil.sb"

fallos=0
# dentro NET LOOP -- orden...: ejecuta orden en el perfil con los parámetros de m1.
dentro() {
	net=$1 loop=$2
	shift 2
	sandbox-exec -f "$T/perfil.sb" \
		-D ROOT="$R" -D MDIR="$M" -D NET="$net" -D LOOP="$loop" -D GFX=0 \
		-D BROKER=/dev/null/kling-vz-no-broker -D FMIN=$FMIN -D FMAX=$FMAX \
		-D L0="$R/images/vmlinux" -D L1="$R/images/base.ext4" -D L2="$R/snapshots/suyo/mem.file" \
		-D L3= -D L4= -D L5= -D L6= -D L7= -D L8= -D L9= -D L10= -D L11= -D L12= -D L13= -D L14= -D L15= \
		-D E0="$M/overlay.ext4" -D E1="$R/volumes/suyo.ext4" \
		-D E2= -D E3= -D E4= -D E5= -D E6= -D E7= -D E8= -D E9= -D E10= -D E11= -D E12= -D E13= -D E14= -D E15= \
		"$@" >/dev/null 2>&1
}
# prueba DEBE descripción NET LOOP -- orden...
prueba() {
	debe=$1 que=$2
	shift 2
	if dentro "$@"; then hubo=PERMITIDO; else hubo=DENEGADO; fi
	marca="ok"
	if [ "$hubo" != "$debe" ]; then
		marca="MAL"
		fallos=$((fallos + 1))
	fi
	printf '%-4s %-10s (debe %-10s) %s\n' "$marca" "$hubo" "$debe" "$que"
}
conecta='use IO::Socket::INET; IO::Socket::INET->new(PeerAddr=>$ARGV[0], PeerPort=>$ARGV[1], Timeout=>2) or exit 1'
escucha='use IO::Socket::INET; IO::Socket::INET->new(LocalAddr=>"127.0.0.1", LocalPort=>$ARGV[0], Listen=>1, ReuseAddr=>1) or exit 1'

echo "perfil: $PERFIL"
echo "--- ficheros"
prueba DENEGADO "leer secrets/snapshot.key (clave maestra de todos los credentials.enc)" 0 0 cat "$R/secrets/snapshot.key"
prueba DENEGADO "leer machines/m2/credentials.enc (otra máquina)" 0 0 cat "$R/machines/m2/credentials.enc"
prueba DENEGADO "leer snapshots/ajeno/mem.file (un dorado que no restaura)" 0 0 cat "$R/snapshots/ajeno/mem.file"
prueba DENEGADO "escribir snapshots/ajeno/mem.file (reescribir un dorado)" 0 0 sh -c "printf x >>'$R/snapshots/ajeno/mem.file'"
prueba DENEGADO "escribir snapshots/suyo/mem.file (ni el suyo)" 0 0 sh -c "printf x >>'$R/snapshots/suyo/mem.file'"
prueba DENEGADO "crear snapshots/nuevo.file" 0 0 sh -c "printf x >'$R/snapshots/nuevo.file'"
prueba DENEGADO "escribir volumes/otro.ext4 (volumen de otra máquina)" 0 0 sh -c "printf x >>'$R/volumes/otro.ext4'"
prueba DENEGADO "escribir machines/m2/ (otra máquina)" 0 0 sh -c "printf x >'$R/machines/m2/x'"
prueba PERMITIDO "leer su kernel" 0 0 cat "$R/images/vmlinux"
prueba PERMITIDO "leer su dorado (el estado que restaura)" 0 0 cat "$R/snapshots/suyo/mem.file"
prueba PERMITIDO "escribir su overlay" 0 0 sh -c "printf x >>'$M/overlay.ext4'"
prueba PERMITIDO "escribir su volumen" 0 0 sh -c "printf x >>'$R/volumes/suyo.ext4'"
prueba PERMITIDO "crear en su directorio" 0 0 sh -c "printf x >'$M/credaudit.jsonl'"
echo "--- red"
prueba DENEGADO "egress none: 127.0.0.1:$REENVIO (reenvío de otra máquina)" 0 0 perl -e "$conecta" 127.0.0.1 "$REENVIO"
prueba DENEGADO "egress none: 0.0.0.0:$REENVIO (llega al loopback)" 0 0 perl -e "$conecta" 0.0.0.0 "$REENVIO"
prueba DENEGADO "egress none: 127.0.0.1:$OTRO (otro servicio del Mac)" 0 0 perl -e "$conecta" 127.0.0.1 "$OTRO"
prueba DENEGADO "egress internet: 127.0.0.1:$REENVIO" 1 0 perl -e "$conecta" 127.0.0.1 "$REENVIO"
prueba DENEGADO "egress internet: 127.0.0.1:$OTRO" 1 0 perl -e "$conecta" 127.0.0.1 "$OTRO"
prueba DENEGADO "allowlist: 127.0.0.1:$REENVIO" 1 1 perl -e "$conecta" 127.0.0.1 "$REENVIO"
prueba DENEGADO "allowlist: 0.0.0.0:$REENVIO" 1 1 perl -e "$conecta" 0.0.0.0 "$REENVIO"
prueba PERMITIDO "allowlist: 127.0.0.1:$OTRO (upstream -upstream del loopback)" 1 1 perl -e "$conecta" 127.0.0.1 "$OTRO"
prueba PERMITIDO "escuchar en 127.0.0.1:$BIND (su reenvío, en el rango)" 0 0 perl -e "$escucha" "$BIND"
prueba DENEGADO "escuchar en 127.0.0.1:$OTRO (fuera del rango)" 0 0 perl -e "$escucha" "$((OTRO + 1))"

echo
if [ $fallos -eq 0 ]; then
	echo "todo como debe"
	exit 0
fi
echo "$fallos pruebas no dan lo que deben"
exit 1
