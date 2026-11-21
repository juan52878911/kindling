#!/usr/bin/env bash
# Daemon privado kt-bench en el CT 105 (receta de scripts/93-e2e-linux-ci.sh).
set -euo pipefail
BASE=/srv/kt-bench; UNIT=kt-bench; RUN=/run/$UNIT; SOCK=$RUN/kling.sock; ROOT=$BASE/root; LIB=$BASE/lib
BINS=/root/bench/bins
command -v firecracker >/dev/null && firecracker --version | head -1
id kindling-build >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build
id kindling >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin -G kvm kindling
mkdir -p "$BASE/bin" "$LIB" "$ROOT/images" "$RUN" "$BASE/shares"
chmod 755 "$BASE" "$ROOT"
printf '{"daemon":{"share_roots":["%s"]}}\n' "$BASE/shares" > "$BASE/config.json"
install -m755 "$BINS/kling" "$BASE/bin/kling"
install -m755 "$BINS/kling-guest" "$LIB/kling-guest"
install -m755 "$BINS/81-base-image.sh" "$BINS/lib-ext4-shrink.sh" "$LIB/"
install -d -m755 "$BASE/builders"
printf '#!/bin/sh\nexec %s builder base "$@"\n' "$BASE/bin/kling" > "$BASE/builders/base"; chmod 755 "$BASE/builders/base"
for f in vmlinux min.ext4; do [ -f "$ROOT/images/$f" ] || cp /var/lib/kindling/images/$f "$ROOT/images/"; done
systemctl is-active --quiet $UNIT || systemd-run --unit "$UNIT" --collect -p KillMode=process -E "KLING_CONFIG=$BASE/config.json" \
  -E "KLING_LIB_DIR=$LIB" -E "KLING_BUILDERS_DIR=$BASE/builders" -E "KLING_SOCKET_USER=root" -E "KLING_MIN_FREE_DISK_MIB=1024" \
  "$BASE/bin/kling" daemon -root "$ROOT" -socket "$SOCK" >/dev/null
for _ in $(seq 1 60); do KLING_HOST=unix://$SOCK KLING_CONFIG=$BASE/config.json "$BASE/bin/kling" ps >/dev/null 2>&1 && break; sleep 0.5; done
KLING_HOST=unix://$SOCK KLING_CONFIG=$BASE/config.json "$BASE/bin/kling" status -v 2>&1 | head -12
