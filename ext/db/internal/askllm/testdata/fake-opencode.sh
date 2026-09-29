#!/bin/sh
# opencode falso para los tests: FAKE_OC_MODE elige qué emite.
# Deja en $FAKE_OC_LOG los argumentos, el cwd y el contenido de su directorio.
if [ -n "$FAKE_OC_LOG" ]; then
  {
    echo "ARGS: $*"
    echo "PWD: $(pwd)"
    echo "LS: $(ls -A | tr '\n' ' ')"
    echo "PROMPT: $(head -c 200 prompt.txt 2>/dev/null | tr '\n' ' ')"
  } > "$FAKE_OC_LOG"
fi
start='{"type":"step_start","part":{"type":"step-start"}}'
fin='{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}'
case "$FAKE_OC_MODE" in
  block)
    echo "$start"
    printf '%s\n' '{"type":"text","part":{"type":"text","text":"```sql\nSELECT count(*) FROM clientes\n```"}}'
    echo "$fin" ;;
  around)
    echo "$start"
    printf '%s\n' '{"type":"text","part":{"type":"text","text":"Here you go:\n"}}'
    printf '%s\n' '{"type":"text","part":{"type":"text","text":"```sql\nSELECT 1;\n```\nHope it helps."}}'
    echo "$fin" ;;
  tool)
    echo "$start"
    printf '%s\n' '{"type":"tool_use","part":{"type":"tool","tool":"bash"}}'
    printf '%s\n' '{"type":"text","part":{"type":"text","text":"SELECT 1"}}'
    echo "$fin" ;;
  hang)
    # un nieto que también cuelga: el timeout tiene que matar el grupo entero
    sleep 300 &
    echo $! > "$FAKE_OC_PIDFILE"
    sleep 300 ;;
  notjson)
    echo "this is not json" ;;
  nofinish)
    echo "$start"
    echo '{"type":"text","part":{"type":"text","text":"SELECT 1"}}' ;;
  length)
    printf '%s\n' '{"type":"text","part":{"type":"text","text":"SELECT 1"}}'
    echo '{"type":"step_finish","part":{"reason":"length"}}' ;;
  fail)
    echo "boom: bad credentials" >&2
    exit 3 ;;
esac
