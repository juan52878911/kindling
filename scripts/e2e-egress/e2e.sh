#!/bin/bash
# e2e del egress de Linux: la allowlist sigue funcionando, una ráfaga de
# paquetes nuevos se corta (hashlimit), la microVM no alcanza la IP del host ni
# con INPUT vaciado (un ufw/firewalld reload), y el siguiente montaje repone
# las reglas INPUT. Mide además el arranque con allowlist.
#
# Corre como root DENTRO de un netns y un mount ns propios, para no tocar el
# daemon ni la red del host:
#   sudo unshare --net --mount --pid --fork --mount-proc --propagation private \
#     scripts/e2e-egress/e2e.sh ./kling rama "$PWD/scripts/e2e-egress"
# Pide python3, ipset, iptables, firecracker y vmlinux + node22.ext4 en
# /var/lib/kindling/images. Todo va a tmpfs; al acabar, `rmdir /srv/kt-egress`.
set -u
BIN=$1; ETQ=$2; D=$3
W=/srv/kt-egress
mkdir -p $W; mount -t tmpfs tmpfs $W
mkdir -p $W/root/images
cp /var/lib/kindling/images/vmlinux /var/lib/kindling/images/node22.ext4 $W/root/images/
for m in /run/netns /run/kindling /tmp /sys/fs/cgroup /var/lib/kindling; do
  mkdir -p $m; mount -t tmpfs tmpfs $m
done
chgrp -R kindling $W/root 2>/dev/null; chmod -R g+rX $W/root

# Internet de mentira: el netns fakeinet tiene 198.51.100.1 y .3 (destinos) y
# 1.1.1.1 (el upstream DNS fijo del daemon); el "host" es 198.51.100.2.
ip link set lo up
ip netns add fakeinet
ip link add up0 type veth peer name up1 netns fakeinet
ip addr add 198.51.100.2/24 dev up0; ip link set up0 up
for a in 198.51.100.1/24 198.51.100.3/24 1.1.1.1/32; do ip -n fakeinet addr add $a dev up1; done
ip -n fakeinet link set up1 up; ip -n fakeinet link set lo up
ip route add default via 198.51.100.1
ip netns exec fakeinet python3 $D/fakeinet.py & 
python3 $D/servicio-host.py &
sleep 0.5

export KLING_JAILER=0 KLING_COW=off KLING_IMAGE_SNAPSHOTS=0 KLING_MIN_FREE_DISK_MIB=256
$BIN daemon -root $W/root -socket $W/kling.sock >$W/daemon.log 2>&1 &
export KLING_HOST=unix://$W/kling.sock
for i in $(seq 50); do $BIN status >/dev/null 2>&1 && break; sleep 0.2; done
JS=$(cat $D/invitado.js)
ex() { $BIN exec -timeout 30s "$@"; }
get() { ex $1 -- node -e "$JS" get $2 2>&1 | tail -1; }
cuenta() { python3 -c "import urllib.request;print(urllib.request.urlopen('http://198.51.100.1:8080/cuenta').read().decode())"; }
ms() { echo $(( ($(date +%s%N) - $1) / 1000000 )); }
arrancar() { # nombre egress [allow]
  local t0=$(date +%s%N) extra=""
  [ -n "${3:-}" ] && extra="-allow $3"
  if ! $BIN run -name $1 -image node22 -egress $2 $extra -allow-exec -fresh >/dev/null 2>$W/run.err; then
    echo "[$ETQ] run $1 FALLA: $(tail -2 $W/run.err)"; tail -20 $W/daemon.log; return 1
  fi
  echo "[$ETQ] arranque $2 $1: $(ms $t0) ms"
}
quitar() { $BIN rm -f $1 >/dev/null 2>&1; }

echo "== [$ETQ] allowlist =="
arrancar al allowlist permitido.test || exit 1
echo "[$ETQ] allowlist permitido.test:8080 -> $(get al http://permitido.test:8080/)"
echo "[$ETQ] allowlist IP no permitida 198.51.100.3:8080 -> $(get al http://198.51.100.3:8080/)"
echo "[$ETQ] allowlist IP del host 198.51.100.2:7070 -> $(get al http://198.51.100.2:7070/)"
cuenta >/dev/null
echo "[$ETQ] allowlist ráfaga UDP 2 s a 198.51.100.1:9999: $(ex al -- node -e "$JS" rafaga 198.51.100.1 9999 2 2>&1 | tail -1), recibidos $(cuenta)"
ns=$(ip netns list | awk '/^kl-/{print $1; exit}')
echo "[$ETQ] contador del hashlimit: $(ip netns exec $ns iptables -L FORWARD -v -x -n | awk '/limit: above/{print $1" paquetes descartados"}')"
quitar al

echo "== [$ETQ] internet + reload del cortafuegos =="
arrancar inet internet || exit 1
cuenta >/dev/null
echo "[$ETQ] internet ráfaga UDP 2 s: $(ex inet -- node -e "$JS" rafaga 198.51.100.1 9999 2 2>&1 | tail -1), recibidos $(cuenta)"
echo "[$ETQ] internet IP del host con INPUT puesto -> $(get inet http://198.51.100.2:7070/)"
iptables -F INPUT   # lo que hace un ufw/firewalld reload
echo "[$ETQ] internet IP del host con INPUT vaciado -> $(get inet http://198.51.100.2:7070/)"
quitar inet
arrancar otra none || exit 1
if iptables -C INPUT -i vh-+ -s 172.30.0.0/16 -j DROP 2>/dev/null; then r=repuesto; else r=AUSENTE; fi
echo "[$ETQ] DROP de INPUT tras el siguiente montaje: $r ($(iptables -S INPUT | grep -c vh-) reglas vh-)"
quitar otra

echo "== [$ETQ] arranque con allowlist (5 veces) =="
for i in 1 2 3 4 5; do arrancar t$i allowlist permitido.test | sed 's/.*: //' ; quitar t$i; done | tr '\n' ' '; echo
$BIN daemon stop >/dev/null 2>&1; kill %3 2>/dev/null
grep -i -E "warning|error" $W/daemon.log | grep -v "ip6tables\|sysctl" | sort | uniq -c | head -10
