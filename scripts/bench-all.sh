#!/usr/bin/env bash
# Runs the existing bench scripts in sequence and prints one summary table.
# See docs/benchmarks.md for what each number means, on what hardware it was
# first measured, and which ones are stale after the A1-A4 boot-latency work.
#
# Needs KVM (real hardware, or nested virtualization) — that is what every one
# of these scripts actually measures, so this refuses to run without it
# instead of silently producing garbage numbers.
#
#   sudo scripts/bench-all.sh
#   DEST=/opt/fc RUNS=5 sudo scripts/bench-all.sh
#
# scripts/40-bench-boot.sh (cold boot, snapshot, restore) is the only bench
# that is fully self-contained (it fetches nothing itself, but needs only
# vmlinux+rootfs.ext4 under $DEST, which scripts/30-fetch-artifacts.sh
# provides). scripts/96/97/98/99-*.sh each need a golden snapshot or a running
# machine that this script does not create for you — when the one they need
# is missing, this prints why and skips it instead of failing the whole run.
set -uo pipefail

DEST="${DEST:-/opt/fc}"
RUNS="${RUNS:-3}"
KLING="${KLING:-kling}"

# ── refuse clearly, before running anything, if KVM is not usable ─────────────
if [ ! -e /dev/kvm ]; then
  echo "bench-all: /dev/kvm does not exist — this host has no KVM (or it's not" >&2
  echo "loaded/exposed). Every one of these benchmarks boots a real microVM," >&2
  echo "so there is nothing to measure without it. Run this on the lab rig, or" >&2
  echo "inside a VM with nested virtualization enabled (see docs/mac-arm64.md" >&2
  echo "for the macOS case)." >&2
  exit 1
fi
if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
  echo "bench-all: /dev/kvm exists but is not readable/writable by $(id -un)." >&2
  echo "Run as root, or add this user to the kvm group and re-log in." >&2
  exit 1
fi

results=()   # each entry: "name|value|status"
add_result() { results+=("$1|$2|$3"); }

echo "== scripts/40-bench-boot.sh (cold boot, snapshot creation, restore) =="
if [ "$(id -u)" -ne 0 ]; then
  echo "skipped: needs root (mounts the rootfs, talks to Firecracker's socket directly)" >&2
  add_result "cold boot / snapshot / restore" "-" "skipped (not root)"
elif [ ! -f "$DEST/vmlinux" ] || [ ! -f "$DEST/rootfs.ext4" ]; then
  echo "skipped: missing $DEST/vmlinux or $DEST/rootfs.ext4 — run scripts/30-fetch-artifacts.sh first" >&2
  add_result "cold boot / snapshot / restore" "-" "skipped (no artifacts under $DEST)"
else
  out="$(DEST="$DEST" RUNS="$RUNS" bash "$(dirname "$0")/40-bench-boot.sh" 2>&1)" && rc=0 || rc=$?
  echo "$out"
  if [ "$rc" -eq 0 ]; then
    cold="$(echo "$out" | grep -oE 'cold[^0-9]*[0-9]+ ?ms' | tail -1)"
    snap="$(echo "$out" | grep -oE 'snapshot[^0-9]*[0-9]+ ?ms' | tail -1)"
    rest="$(echo "$out" | grep -oE 'restor[^0-9]*[0-9]+ ?ms' | tail -1)"
    add_result "cold boot" "${cold:-see log above}" "ran"
    add_result "snapshot creation" "${snap:-see log above}" "ran"
    add_result "restore from snapshot" "${rest:-see log above}" "ran"
  else
    add_result "cold boot / snapshot / restore" "-" "FAILED (exit $rc)"
  fi
fi

# ── the rest need a golden snapshot/machine this script does not create ──────
run_if_snapshot_exists() {
  local label="$1" snap="$2" script="$3"; shift 3
  echo "== $script ($label) =="
  if ! "$KLING" template inspect "$snap" >/dev/null 2>&1; then
    echo "skipped: no template/golden snapshot named '$snap' (see docs/benchmarks.md" >&2
    echo "for what to build first, e.g. 'kling ai model add' for a VON template)" >&2
    add_result "$label" "-" "skipped (no template '$snap')"
    return
  fi
  if out="$(bash "$(dirname "$0")/$script" "$snap" "$@" 2>&1)"; then
    echo "$out"
    add_result "$label" "ran, see log above" "ran"
  else
    rc=$?
    echo "$out" >&2
    add_result "$label" "-" "FAILED (exit $rc)"
  fi
}

run_if_snapshot_exists "VON (cold+thaw+tok/s+density)" "${VON_SNAP:-von-smol}" 96-von-bench.sh
run_if_snapshot_exists "encoder (restore+latency+density)" "${ENCODER_SNAP:-enc-e5}" 98-encoder-bench.sh

echo "== 99-thaw-bench.sh (freeze/thaw phase breakdown) =="
if [ -z "${THAW_SNAP:-}" ]; then
  echo "skipped: set THAW_SNAP=<template> to run this one (see docs/despertar.md)" >&2
  add_result "freeze/thaw phase breakdown" "-" "skipped (THAW_SNAP not set)"
elif [ "$(id -u)" -ne 0 ]; then
  echo "skipped: needs root" >&2
  add_result "freeze/thaw phase breakdown" "-" "skipped (not root)"
else
  if out="$(SNAP="$THAW_SNAP" bash "$(dirname "$0")/99-thaw-bench.sh" daemon 2>&1)"; then
    echo "$out"
    add_result "freeze/thaw phase breakdown" "ran, see log above" "ran"
  else
    rc=$?
    echo "$out" >&2
    add_result "freeze/thaw phase breakdown" "-" "FAILED (exit $rc)"
  fi
fi

# ── summary ────────────────────────────────────────────────────────────────
echo
echo "== summary =="
printf '%-40s %-30s %s\n' "BENCH" "VALUE" "STATUS"
any_failed=0
for r in "${results[@]}"; do
  IFS='|' read -r name value status <<<"$r"
  printf '%-40s %-30s %s\n' "$name" "$value" "$status"
  case "$status" in FAILED*) any_failed=1 ;; esac
done

echo
echo "Full numbers, hardware and which ones are stale after A1-A4: docs/benchmarks.md"

[ "$any_failed" -eq 0 ]
