#!/usr/bin/env bash
# Daemon privado kt-bench (receta de scripts/93-e2e-linux-ci.sh). Corre como root en el host del banco.
# Variables: KB (raíz del banco, /opt/kbench), y las del daemon que se quieran barrer
# (KLING_MAX_PARALLEL_BOOT, KLING_JAILER, ...): se pasan tal cual al servicio.
set -euo pipefail
KB=${KB:-/opt/kbench}
BASE=/srv/kt-bench; UNIT=kt-bench; RUN=/run/$UNIT; SOCK=$RUN/kling.sock; ROOT=$BASE/root; LIB=$BASE/lib
BINS=$KB/bins
command -v firecracker >/dev/null && { v=$(firecracker --version 2>&1); sed -n 1p <<<"$v"; }
id kindling-build >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build
id kindling >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin -G kvm kindling
mkdir -p "$BASE/bin" "$LIB" "$ROOT/images" "$RUN" "$BASE/shares"
chmod 755 "$BASE" "$ROOT"
printf '{"daemon":{"share_roots":["%s"]}}\n' "$BASE/shares" > "$BASE/config.json"
install -m755 "$BINS/kling" "$BASE/bin/kling"
install -m755 "$BINS/kling-guest" "$LIB/kling-guest"
install -m755 "$BINS/81-base-image.sh" "$BINS/lib-ext4-shrink.sh" "$LIB/"
install -d -m755 "$BASE/builders"
# Los constructores que usa el banco: base (imágenes), llm (dorados VON de `kling ai model add`)
# y chispa (`kling ai chispa deploy`). Son el mismo kling con otro subcomando.
for b in base llm chispa; do
  printf '#!/bin/sh\nexec %s builder %s "$@"\n' "$BASE/bin/kling" "$b" > "$BASE/builders/$b"; chmod 755 "$BASE/builders/$b"
done
for f in vmlinux min.ext4; do [ -f "$ROOT/images/$f" ] || cp /var/lib/kindling/images/$f "$ROOT/images/"; done
# Barrido: se reenvían al servicio las KLING_* presentes en el entorno.
EXTRA=()
for v in KLING_MAX_PARALLEL_BOOT KLING_JAILER; do
  [ -n "${!v:-}" ] && EXTRA+=(-E "$v=${!v}")
done
systemctl is-active --quiet $UNIT || systemd-run --unit "$UNIT" --collect -p KillMode=process -E "KLING_CONFIG=$BASE/config.json" \
  -E "KLING_LIB_DIR=$LIB" -E "KLING_BUILDERS_DIR=$BASE/builders" -E "KLING_SOCKET_USER=root" -E "KLING_MIN_FREE_DISK_MIB=1024" \
  "${EXTRA[@]}" "$BASE/bin/kling" daemon -root "$ROOT" -socket "$SOCK" >/dev/null
for _ in $(seq 1 60); do KLING_HOST=unix://$SOCK KLING_CONFIG=$BASE/config.json "$BASE/bin/kling" ps >/dev/null 2>&1 && break; sleep 0.5; done
# Toda la salida primero y luego las 12 líneas: con `| head -12` y pipefail, kling se llevaba un
# SIGPIPE al cerrar head y el guion salía con error justo después de un arranque bueno.
estado=$(KLING_HOST=unix://$SOCK KLING_CONFIG=$BASE/config.json "$BASE/bin/kling" status -v 2>&1)
sed -n '1,12p' <<<"$estado"
