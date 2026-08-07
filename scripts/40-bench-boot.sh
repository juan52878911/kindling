#!/usr/bin/env bash
# Mide arranque en frío, creación de snapshot y restauración. Ejecutar como root
# dentro de la VM del laboratorio, después de 30-fetch-artifacts.sh.
#
# El arranque en frío se mide con un init que apaga la microVM al terminar, y
# cronometrando el proceso entero. Medirlo con `timeout N ... | grep -q` NO funciona:
# grep sale al primer match pero Firecracker sigue escribiendo, así que acabas
# midiendo tu propio timeout. Ver docs/hallazgos.md.
set -euo pipefail

DEST="${DEST:-/opt/fc}"
RUNS="${RUNS:-3}"
MEM_MIB="${MEM_MIB:-256}"
VCPUS="${VCPUS:-1}"
cd "$DEST"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
[ -f vmlinux ] && [ -f rootfs.ext4 ] || { echo "faltan artefactos, corre 30-fetch-artifacts.sh" >&2; exit 1; }

api() { curl -s --unix-socket "$1" -X "$2" "http://localhost$3" -H 'Content-Type: application/json' -d "$4"; }
ms()  { echo $(( ($2 - $1) / 1000000 )); }

# ── init que se apaga solo, para poder cronometrar el arranque completo ────────
mkdir -p /mnt/fcbench
mount -o loop rootfs.ext4 /mnt/fcbench
printf '#!/bin/sh\necho FC_READY\n/sbin/reboot -f\n' > /mnt/fcbench/fcinit.sh
chmod +x /mnt/fcbench/fcinit.sh
umount /mnt/fcbench

BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"

cat > bench-cold.json <<JSON
{
  "boot-source": { "kernel_image_path": "$DEST/vmlinux", "boot_args": "$BOOT_ARGS init=/fcinit.sh" },
  "drives": [ { "drive_id": "rootfs", "path_on_host": "$DEST/rootfs.ext4",
                "is_root_device": true, "is_read_only": false } ],
  "machine-config": { "vcpu_count": $VCPUS, "mem_size_mib": $MEM_MIB }
}
JSON

echo "== arranque en frío =="
for i in $(seq 1 "$RUNS"); do
  s=$(date +%s%N); firecracker --no-api --config-file bench-cold.json >/dev/null 2>&1; e=$(date +%s%N)
  echo "  $i: $(ms "$s" "$e") ms"
done

# ── snapshot ──────────────────────────────────────────────────────────────────
echo "== snapshot =="
rm -f /tmp/fc-bench.sock snap.file mem.file
firecracker --api-sock /tmp/fc-bench.sock >/dev/null 2>&1 &
fcpid=$!; sleep 0.5
api /tmp/fc-bench.sock PUT /boot-source \
  "{\"kernel_image_path\":\"$DEST/vmlinux\",\"boot_args\":\"$BOOT_ARGS\"}" >/dev/null
api /tmp/fc-bench.sock PUT /drives/rootfs \
  "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$DEST/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}" >/dev/null
api /tmp/fc-bench.sock PUT /machine-config \
  "{\"vcpu_count\":$VCPUS,\"mem_size_mib\":$MEM_MIB}" >/dev/null
api /tmp/fc-bench.sock PUT /actions '{"action_type":"InstanceStart"}' >/dev/null
sleep 5   # dejar que termine el boot antes de congelar
api /tmp/fc-bench.sock PATCH /vm '{"state":"Paused"}' >/dev/null
s=$(date +%s%N)
api /tmp/fc-bench.sock PUT /snapshot/create \
  "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$DEST/snap.file\",\"mem_file_path\":\"$DEST/mem.file\"}" >/dev/null
e=$(date +%s%N)
echo "  creado en $(ms "$s" "$e") ms"
kill "$fcpid" 2>/dev/null || true; sleep 1
ls -lh snap.file mem.file | awk '{print "  " $5, $9}'

# ── restauración ──────────────────────────────────────────────────────────────
echo "== restauración =="
for i in $(seq 1 "$RUNS"); do
  rm -f /tmp/fc-restore.sock
  firecracker --api-sock /tmp/fc-restore.sock >/dev/null 2>&1 &
  rpid=$!; sleep 0.4
  s=$(date +%s%N)
  api /tmp/fc-restore.sock PUT /snapshot/load \
    "{\"snapshot_path\":\"$DEST/snap.file\",\"mem_backend\":{\"backend_path\":\"$DEST/mem.file\",\"backend_type\":\"File\"},\"resume_vm\":true}" >/dev/null
  e=$(date +%s%N)
  echo "  $i: $(ms "$s" "$e") ms"
  kill "$rpid" 2>/dev/null || true; sleep 0.5
done

echo
echo "recuerda: el estado de red y la entropía del invitado se clonan con el snapshot."
