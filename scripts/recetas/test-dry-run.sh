#!/usr/bin/env bash
# Test de las recetas: ejecuta cada --dry-run con claves FALSAS reconocibles y
# comprueba que salen comandos, que el código es 0 y que ni la salida ni los
# argumentos de ningún comando llevan la clave literal. No necesita daemon.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

FALSA_A="sk-ant-FALSA-0123456789abcdef"
FALSA_O="sk-proj-FALSA-fedcba9876543210"
fallos=0

for s in claude-code codex playwright; do
  salida=$(ANTHROPIC_API_KEY=$FALSA_A OPENAI_API_KEY=$FALSA_O ./"$s".sh --dry-run 2>&1) \
    || { echo "FALLO $s: --dry-run salió con error"; fallos=$((fallos + 1)); continue; }
  n=$(grep -c '^+ ' <<<"$salida" || true)
  if [ "$n" -lt 3 ]; then echo "FALLO $s: solo $n comandos"; fallos=$((fallos + 1)); fi
  for k in "$FALSA_A" "$FALSA_O"; do
    if grep -qF -- "$k" <<<"$salida"; then
      echo "FALLO $s: la clave falsa aparece en la salida"; fallos=$((fallos + 1))
    fi
  done
  # Ningún comando kling real lleva «-e CLAVE=» ni la clave en argv.
  if grep -E '^\+ kling .*-e [A-Z_]*(API_KEY|TOKEN|SECRET)=' <<<"$salida"; then
    echo "FALLO $s: una clave viaja por -e"; fallos=$((fallos + 1))
  fi
  echo "ok $s: $n comandos"
done

# Modo real sin clave: tiene que negarse (código 2), no seguir.
set +e
env -u ANTHROPIC_API_KEY ./claude-code.sh >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -ne 2 ]; then echo "FALLO claude-code sin clave: código $rc, esperaba 2"; fallos=$((fallos + 1)); else echo "ok sin clave: se niega"; fi

# Modo real con un kling falso: la clave llega por stdin a `machine
# credential` y no aparece en ningún argv.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/kling" <<'F'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$LOG_ARGV"
if [ "${1:-}" = machine ] && [ "${2:-}" = credential ]; then cat >"$LOG_STDIN"; fi
[ "${1:-}" = sandbox ] && [ "${2:-}" = create ] && echo sbx-falso
exit 0
F
chmod +x "$tmp/kling"
LOG_ARGV="$tmp/argv" LOG_STDIN="$tmp/stdin" KLING="$tmp/kling" ANTHROPIC_API_KEY=$FALSA_A ./claude-code.sh >/dev/null 2>&1 \
  || { echo "FALLO modo real con kling falso"; fallos=$((fallos + 1)); }
if [ "$(cat "$tmp/stdin" 2>/dev/null)" != "$FALSA_A" ]; then echo "FALLO: la clave no llegó por stdin"; fallos=$((fallos + 1)); fi
if grep -qF -- "$FALSA_A" "$tmp/argv"; then echo "FALLO: la clave está en un argv"; fallos=$((fallos + 1)); else echo "ok real: clave por stdin, $(wc -l <"$tmp/argv") llamadas sin clave en argv"; fi

[ "$fallos" -eq 0 ]
