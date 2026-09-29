#!/usr/bin/env bash
# shellcheck disable=SC2015,SC2013  # guion de pruebas: "a && ok || ko" es a propósito
# Lo que se puede comprobar del prototipo SIN Mac, sin KVM y sin root: sintaxis,
# el linter shellcheck (si está), la lógica del informe de fase0.sh, el comprobador de
# kernel contra configuraciones sintéticas y la forma del fragmento. Con
# KSRC=<árbol de linux 6.1> comprueba además que cada símbolo de
# config-android existe en ese árbol.
#
#   prototypes/android/test-local.sh
#   KSRC=~/src/linux-6.1.140 prototypes/android/test-local.sh
#
# No sustituye a la fase 0: nada de esto arranca Android.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
fallos=0
ok() { echo "ok   $*"; }
ko() { echo "FAIL $*"; fallos=$((fallos + 1)); }

SCRIPTS=(fase0.sh test-local.sh kernel/build.sh kernel/check-android-config.sh
         image/build-image.sh image/android-launch.sh image/android-sh image/verity.sh
         test-phoned.sh
         image/kindling-phoned/ready image/kindling-phoned/post-restore.d/10-identity)

# 1. sintaxis
for s in "${SCRIPTS[@]}"; do
  if bash -n "$s"; then ok "bash -n $s"; else ko "bash -n $s"; fi
done

# 2. shellcheck
if command -v shellcheck >/dev/null 2>&1; then
  if LANG=C.UTF-8 shellcheck -x "${SCRIPTS[@]}"; then ok "shellcheck"; else ko "shellcheck"; fi
else
  echo "skip shellcheck (not installed)"
fi

# 3. fase0.sh: informe y utilidades
if ./fase0.sh --self-test >/dev/null; then ok "fase0.sh --self-test"; else ko "fase0.sh --self-test"; fi

# 4. el fragmento: formato, sin "=m", sin duplicados
frag=kernel/config-android
malas="$(grep -vE '^(#.*|CONFIG_[A-Z0-9_]+=(y|n|[0-9]+|"[^"]*")|# CONFIG_[A-Z0-9_]+ is not set|)$' "$frag" || true)"
[ -z "$malas" ] && ok "config-android: formato" || ko "config-android: líneas raras: $malas"
grep -qE '^CONFIG_[A-Z0-9_]+=m$' "$frag" && ko "config-android: tiene =m" || ok "config-android: sin =m"
dups="$(grep -oE '^(# )?CONFIG_[A-Z0-9_]+' "$frag" | sed 's/^# //' | sort | uniq -d)"
[ -z "$dups" ] && ok "config-android: sin duplicados" || ko "config-android: duplicados: $dups"

# 5. el comprobador contra configuraciones sintéticas
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
buena="$tmp/buena.config"
{
  for o in ANDROID_BINDER_IPC ANDROID_BINDERFS MEMFD_CREATE DMABUF_HEAPS DMABUF_HEAPS_SYSTEM IPV6 \
           ARM64_4K_PAGES COMPAT NAMESPACES PID_NS IPC_NS UTS_NS NET_NS CGROUPS MEMCG CPUSETS CGROUP_SCHED \
           BPF_SYSCALL CGROUP_BPF FUSE_FS PROC_FS SYSFS TMPFS DEVTMPFS UNIX98_PTYS POSIX_MQUEUE \
           EPOLL SIGNALFD TIMERFD EVENTFD FUTEX; do echo "CONFIG_$o=y"; done
  echo 'CONFIG_ANDROID_BINDER_DEVICES="binder,hwbinder,vndbinder"'
} >"$buena"
if kernel/check-android-config.sh "$buena" >/dev/null; then ok "check: config mínima pasa"; else ko "check: config mínima debería pasar"; fi
grep -v BINDERFS "$buena" >"$tmp/sin-binderfs.config"
if kernel/check-android-config.sh "$tmp/sin-binderfs.config" >/dev/null 2>&1; then ko "check: sin binderfs debería fallar"; else ok "check: sin binderfs falla"; fi
sed 's/"binder,hwbinder,vndbinder"/"binder"/' "$buena" >"$tmp/pocos.config"
if kernel/check-android-config.sh "$tmp/pocos.config" >/dev/null 2>&1; then ko "check: faltan hwbinder/vndbinder y pasa"; else ok "check: exige los tres binder"; fi
{ cat "$buena"; echo "CONFIG_FOO=m"; } >"$tmp/modulo.config"
if kernel/check-android-config.sh "$tmp/modulo.config" >/dev/null 2>&1; then ko "check: =m debería fallar"; else ok "check: =m falla"; fi
if kernel/check-android-config.sh - <"$buena" >/dev/null; then ok "check: lee de stdin"; else ko "check: stdin"; fi
# El fragmento + lo común del núcleo cubre todas las obligatorias del check
# (sin pasar por Kconfig: solo que nadie se olvidó de pedir algo).
if cat ../../scripts/builders/kernel/config-common ../../scripts/builders/kernel/config-arm64 "$frag" \
   | grep -E '^CONFIG_' | kernel/check-android-config.sh - >"$tmp/frag.out"; then
  ok "check: common+arm64+android pide todo lo obligatorio"
else
  ko "check: al fragmento le falta algo: $(grep FAIL "$tmp/frag.out" | tr '\n' ' ')"
fi

# 6. con un árbol de linux, que cada símbolo exista
if [ -n "${KSRC:-}" ]; then
  faltan=""
  for s in $(grep -oE '^(# )?CONFIG_[A-Z0-9_]+' "$frag" | sed 's/^# //; s/^CONFIG_//' | sort -u); do
    grep -rqsE "^[[:space:]]*(menu)?config $s\$" --include='Kconfig*' "$KSRC" || faltan="$faltan $s"
  done
  [ -z "$faltan" ] && ok "todos los símbolos existen en $KSRC" || ko "símbolos que no existen en $KSRC:$faltan"
else
  echo "skip símbolos (KSRC no definido)"
fi

echo
if [ "$fallos" -gt 0 ]; then echo "test-local: $fallos fallo(s)"; exit 1; fi
echo "test-local: ok"
