#!/usr/bin/env bash
# Instala en una EC2 Debian 12 todo lo del banco: kindling (Firecracker), k3s + OpenSandbox
# (runtime kubernetes) y Docker + OpenSandbox (runtime docker). Corre como root.
# Espera en /root/sube: bins/ (kling, kling-guest, kling-sandbox, 81-base-image.sh,
# lib-ext4-shrink.sh), bench2/ (bench.py, bench_k8s.py, sdk/), osbk8s/, charts/.
set -uo pipefail
exec > >(tee -a /root/setup.log) 2>&1
paso(){ echo "== $(date +%T) $*"; }
bajar(){ curl -fsSL --retry 3 -o "$2" "$1"; }
cd /root

paso apt
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq python3-venv python3-pip zstd curl jq iptables e2fsprogs xfsprogs docker.io >/dev/null
systemctl stop docker docker.socket   # Docker solo en su fase

paso firecracker
F=v1.17.0; bajar https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz fc.tgz
bajar https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz.sha256.txt fc.sha
[ "$(sha256sum fc.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 fc.sha)" ] || { echo "sha firecracker NO coincide"; exit 1; }
tar xzf fc.tgz; install -m755 release-$F-x86_64/firecracker-$F-x86_64 /usr/local/bin/firecracker
install -m755 release-$F-x86_64/jailer-$F-x86_64 /usr/local/bin/jailer

paso imagenes kindling
R=https://github.com/juan52878911/kindling/releases/download/v1.0.0-rc.2
mkdir -p /var/lib/kindling/images
bajar $R/vmlinux-amd64.zst v.zst && zstd -dqf v.zst -o /var/lib/kindling/images/vmlinux
bajar $R/min-amd64.ext4.zst m.zst && zstd -dqf m.zst -o /var/lib/kindling/images/min.ext4
mkdir -p /root/bench /root/bench2
cp -r /root/sube/bins /root/bench/; cp -r /root/sube/bench2/. /root/bench2/
cp /root/sube/kt-bench-up.sh /root/sube/kt-bench-down.sh /root/bench/; cp /root/sube/gw-up.sh /root/bench2/

paso k3s y helm
K=v1.36.4%2Bk3s1; bajar https://github.com/k3s-io/k3s/releases/download/$K/k3s k3s
bajar https://github.com/k3s-io/k3s/releases/download/$K/sha256sum-amd64.txt k3s.sha
[ "$(sha256sum k3s | cut -d' ' -f1)" = "$(awk '$2=="k3s"{print $1}' k3s.sha)" ] || { echo "sha k3s NO coincide"; exit 1; }
install -m755 k3s /usr/local/bin/k3s; ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
H=v3.17.3; bajar https://get.helm.sh/helm-$H-linux-amd64.tar.gz helm.tgz; bajar https://get.helm.sh/helm-$H-linux-amd64.tar.gz.sha256sum helm.sha
[ "$(sha256sum helm.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 helm.sha)" ] || { echo "sha helm NO coincide"; exit 1; }
tar xzf helm.tgz; install -m755 linux-amd64/helm /usr/local/bin/helm

paso venv opensandbox
python3 -m venv /root/osb && /root/osb/bin/pip install -q opensandbox-server==1.2.1 opensandbox==1.2.1
mkdir -p /root/osbk8s; cp -r /root/sube/osbk8s/. /root/osbk8s/; cp -r /root/sube/charts /root/charts
paso fin
