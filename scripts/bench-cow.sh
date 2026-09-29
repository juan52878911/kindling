#!/usr/bin/env bash
# run -from with and without copy-on-write disks (docs/cow.md).
#
# Runs ON the Linux host, as root, next to the daemon (systemd unit "kling").
# For each mode in MODES it restarts the daemon with KLING_COW=<mode>, then for
# each N in COUNTS creates N instances of the golden GOLD one after another
# (`kling run -from`), timing each one and measuring how much disk they took
# (the data root plus, in store mode, the inside of kindling's XFS store), and
# removes them before the next round.
#
#   sudo GOLD=my-golden ./bench-cow.sh
#   sudo BUILD=1 ./bench-cow.sh            # builds a golden with ~200 MiB of overlay
#   sudo MODES="off auto" COUNTS="1 8 32" ./bench-cow.sh
#
# Env: GOLD (golden to instantiate; required unless BUILD=1), BUILD=1 (make a
# golden "cowb-gold" from IMAGE with FILL_MIB of random data in its overlay),
# IMAGE (default), FILL_MIB (200), MODES ("off auto"), COUNTS ("1 8 32"),
# KLING (kling binary), ROOT (/var/lib/kindling), UNIT (kling), KEEP_GOLD=1
# (do not remove the golden BUILD made).
#
# Leaves the daemon as it found it: KLING_COW is unset from the systemd
# manager environment and the unit restarted at the end.
set -uo pipefail

KLING="${KLING:-kling}"
ROOT="${ROOT:-/var/lib/kindling}"
UNIT="${UNIT:-kling}"
MODES="${MODES:-off auto}"
COUNTS="${COUNTS:-1 8 32}"
IMAGE="${IMAGE:-default}"
FILL_MIB="${FILL_MIB:-200}"
BUILD="${BUILD:-0}"
KEEP_GOLD="${KEEP_GOLD:-0}"
GOLD="${GOLD:-}"
PREFIX="cowb"

die() { echo "bench-cow: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "run it as root (it restarts the daemon)"
command -v "$KLING" >/dev/null || die "no $KLING in PATH (set KLING)"
command -v systemctl >/dev/null || die "needs systemd (it restarts the unit $UNIT with KLING_COW)"
if [ "$BUILD" != "1" ] && [ -z "$GOLD" ]; then
  die "set GOLD to a golden (kling template ls), or BUILD=1 to make one"
fi

remove_ours() {
  # Only our own machines: names start with the prefix.
  for m in $("$KLING" ps -a 2>/dev/null | awk -v p="$PREFIX-" 'index($2, p) == 1 {print $1}'); do
    "$KLING" rm -f "$m" >/dev/null 2>&1
  done
}

cleanup() {
  remove_ours
  if [ "$BUILD" = "1" ] && [ "$KEEP_GOLD" != "1" ]; then
    "$KLING" template rm "$PREFIX-gold" >/dev/null 2>&1
  fi
  systemctl unset-environment KLING_COW >/dev/null 2>&1
  systemctl restart "$UNIT" >/dev/null 2>&1
}
trap cleanup EXIT

wait_daemon() {
  for _ in $(seq 1 60); do
    "$KLING" info >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  die "the daemon did not come back after restarting $UNIT"
}

restart_with() {
  systemctl set-environment KLING_COW="$1" || die "systemctl set-environment failed"
  systemctl restart "$UNIT" || die "systemctl restart $UNIT failed"
  wait_daemon
}

# Bytes used on the filesystem of a path.
used_bytes() { df -B1 --output=used "$1" 2>/dev/null | tail -n 1 | tr -d ' '; }

# Disk used by kindling: the data root, plus the inside of the store when it
# is mounted (its image file is reserved up front, so the root does not see
# what instances write into it).
disk_used() {
  local total
  total=$(used_bytes "$ROOT")
  if mountpoint -q "$ROOT/cow" 2>/dev/null; then
    total=$(( total + $(used_bytes "$ROOT/cow") ))
  fi
  echo "$total"
}

now_ns() { date +%s%N; }

if [ "$BUILD" = "1" ]; then
  GOLD="$PREFIX-gold"
  echo "building golden $GOLD from image $IMAGE with $FILL_MIB MiB in its overlay..."
  "$KLING" template rm "$GOLD" >/dev/null 2>&1
  "$KLING" run -name "$PREFIX-src" -image "$IMAGE" >/dev/null || die "kling run -image $IMAGE failed"
  "$KLING" exec "$PREFIX-src" -- sh -c "dd if=/dev/urandom of=/var/$PREFIX.fill bs=1M count=$FILL_MIB 2>/dev/null && sync" \
    || die "could not write the fill file inside the guest"
  "$KLING" save -force "$PREFIX-src" "$GOLD" >/dev/null || die "kling save failed"
  "$KLING" rm -f "$PREFIX-src" >/dev/null 2>&1
fi
[ -f "$ROOT/snapshots/$GOLD/overlay.ext4" ] || die "no golden overlay at $ROOT/snapshots/$GOLD/overlay.ext4"
echo "golden $GOLD: overlay $(du -m --apparent-size "$ROOT/snapshots/$GOLD/overlay.ext4" | cut -f1) MiB apparent, $(du -m "$ROOT/snapshots/$GOLD/overlay.ext4" | cut -f1) MiB on disk"
echo

printf '%-6s %-9s %5s %9s %9s %9s %9s %11s\n' mode actual N first_ms mean_ms p50_ms max_ms disk_MiB
for mode in $MODES; do
  restart_with "$mode"
  actual=$("$KLING" info 2>/dev/null | awk -F': *' '/^disk clones:/ {split($2, a, " "); print a[1]}')
  remove_ours
  # One warm-up instance, not measured: in store mode it creates the store
  # (first time ever) and the golden's base copy inside it.
  "$KLING" run -from "$GOLD" -name "$PREFIX-warm" >/dev/null || die "run -from $GOLD failed ($mode)"
  "$KLING" rm -f "$PREFIX-warm" >/dev/null 2>&1
  for n in $COUNTS; do
    remove_ours
    sync
    before=$(disk_used)
    times=()
    for i in $(seq 1 "$n"); do
      t0=$(now_ns)
      "$KLING" run -from "$GOLD" -name "$PREFIX-$mode-$i" >/dev/null || die "run -from $GOLD failed ($mode, $i of $n)"
      t1=$(now_ns)
      times+=( $(( (t1 - t0) / 1000000 )) )
    done
    sync
    after=$(disk_used)
    sorted=$(printf '%s\n' "${times[@]}" | sort -n)
    mean=$(printf '%s\n' "${times[@]}" | awk '{s += $1} END {printf "%.0f", s / NR}')
    p50=$(echo "$sorted" | awk -v n="$n" 'NR == int((n + 1) / 2) {print}')
    max=$(echo "$sorted" | tail -n 1)
    printf '%-6s %-9s %5s %9s %9s %9s %9s %11s\n' "$mode" "${actual:-?}" "$n" "${times[0]}" "$mean" "$p50" "$max" \
      "$(( (after - before) / 1048576 ))"
  done
  remove_ours
done
echo
echo "first_ms is the first instance of the round; disk_MiB is what the N instances added"
echo "(data root + inside of the store). The daemon's own count: kling info (disk clones)."
