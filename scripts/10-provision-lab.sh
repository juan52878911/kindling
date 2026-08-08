#!/usr/bin/env bash
# Crea la VM del laboratorio en Proxmox. Ejecutar EN el host Proxmox.
#
#   VMID=300 IP=192.168.2.60/24 GW=192.168.2.1 ./10-provision-lab.sh ~/.ssh/id_ed25519.pub
#
# `--cpu host` es imprescindible: sin él no pasan las extensiones de virtualización
# al invitado y Firecracker no encuentra /dev/kvm.
set -euo pipefail

VMID="${VMID:-300}"
NAME="${NAME:-fc-lab}"
MEM="${MEM:-4096}"
BALLOON="${BALLOON:-2048}"
CORES="${CORES:-4}"
DISK="${DISK:-32G}"
STORAGE="${STORAGE:-local-lvm}"
BRIDGE="${BRIDGE:-vmbr0}"
IP="${IP:?define IP, p.ej. 192.168.2.60/24}"
GW="${GW:?define GW, p.ej. 192.168.2.1}"
CIUSER="${CIUSER:-juan}"
IMAGE="${IMAGE:-/var/lib/vz/template/iso/debian-12-genericcloud-amd64.qcow2}"
PUBKEY="${1:?uso: $0 <ruta-clave-publica-ssh>}"

command -v qm >/dev/null || { echo "esto se ejecuta en el host Proxmox" >&2; exit 1; }
[ -f "$IMAGE" ] || { echo "no encuentro la imagen cloud: $IMAGE" >&2; exit 1; }
qm status "$VMID" &>/dev/null && { echo "el VMID $VMID ya existe" >&2; exit 1; }

if [ "$(cat /sys/module/kvm_intel/parameters/nested 2>/dev/null || \
        cat /sys/module/kvm_amd/parameters/nested 2>/dev/null)" != "Y" ]; then
  echo "AVISO: la virtualización anidada no está activada en este host." >&2
  echo "  Intel: echo 'options kvm_intel nested=1' > /etc/modprobe.d/kvm.conf" >&2
  echo "  AMD:   echo 'options kvm_amd nested=1'   > /etc/modprobe.d/kvm.conf" >&2
fi

qm create "$VMID" --name "$NAME" \
  --memory "$MEM" --balloon "$BALLOON" --cores "$CORES" --cpu host \
  --net0 "virtio,bridge=$BRIDGE" \
  --scsihw virtio-scsi-single \
  --scsi0 "$STORAGE:0,import-from=$IMAGE,discard=on,ssd=1" \
  --ide2 "$STORAGE:cloudinit" \
  --boot order=scsi0 --serial0 socket --vga serial0 \
  --agent enabled=1 --ostype l26 --onboot 0 \
  --description "Laboratorio Firecracker - microVMs para herramientas MCP serverless"

qm resize "$VMID" scsi0 "$DISK"
qm set "$VMID" --ciuser "$CIUSER" --sshkeys "$PUBKEY" --ipconfig0 "ip=$IP,gw=$GW"

# El agente de invitado no es opcional en la práctica: sin él, Proxmox no puede
# distinguir memoria en uso de caché de disco, y una VM que solo tiene 600 MB
# ocupados aparece al 81% porque el invitado cacheó ficheros de snapshot.
qm set "$VMID" --cicustom "" >/dev/null 2>&1 || true
qm start "$VMID"

echo "VM $VMID ($NAME) arrancando en ${IP%%/*}"
echo
echo "Dentro de la VM, instala el agente o el panel te mentirá sobre la memoria:"
echo "  sudo apt-get install -y qemu-guest-agent && sudo systemctl enable --now qemu-guest-agent"
echo
echo "comprueba también:  ls -l /dev/kvm"
