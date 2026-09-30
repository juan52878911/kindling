#!/bin/sh
# opencode falso para los tests: FAKE_OC_MODE elige qué emite.
# Deja en $FAKE_OC_LOG los argumentos, el cwd, el contenido de su directorio y
# el principio de su stdin (por donde llega el prompt).
if [ -n "$FAKE_OC_LOG" ]; then
  {
    echo "ARGS: $*"
    echo "PWD: $(pwd)"
    echo "LS: $(ls -A | tr '\n' ' ')"
    echo "STDIN: $(head -c 200 | tr '\n' ' ')"
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
  provider)
    # el proveedor falla siempre (con control en el mensaje, que no debe llegar)
    echo "$start"
    printf '%s\n' '{"type":"error","error":{"name":"APIError","data":{"message":"upstream\u001b[31m overloaded"}}}' ;;
  provideronce)
    # falla la primera vez y responde la segunda: el reintento lo arregla
    if [ ! -e "$FAKE_OC_LOG.once" ]; then
      : > "$FAKE_OC_LOG.once"
      echo "$start"
      printf '%s\n' '{"type":"error","error":"rate limited"}'
    else
      echo "$start"
      printf '%s\n' '{"type":"text","part":{"type":"text","text":"SELECT 2"}}'
      echo "$fin"
    fi ;;
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
