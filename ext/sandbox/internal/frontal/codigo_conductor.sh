#!/usr/bin/env bash
# Conductor de contextos de código del gateway de kindling (bash).
#
# Corre DENTRO del sandbox, con el usuario del inquilino. Mismo protocolo de
# eventos que codigo_conductor.py (stdout, stderr, error, done), pero el canal
# no es un socket Unix (bash no los habla) sino una FIFO:
#
#   conductor.sh serve FIFO     crea la FIFO y atiende peticiones, una a la vez
#   conductor.sh run FIFO PLAZO cliente de una línea: lee el código de stdin y
#                               copia a stdout las líneas JSON que recibe
#
# Petición: el cliente crea una FIFO de respuesta, la abre, y escribe en FIFO
# la línea "PLAZO FICHERO_DEL_CODIGO FIFO_DE_RESPUESTA". El estado (variables,
# funciones, directorio actual) persiste entre ejecuciones: cada una corre en
# un subshell que lo restaura de estado.sh y lo guarda al terminar. Así un
# `exit`, un plazo o una interrupción no matan el contexto.

# esc deja en REPLY el texto listo para ir entre comillas en JSON.
esc() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/\\r}
  s=${s//$'\n'/\\n}
  s=${s//[$'\001'-$'\010'$'\013'$'\014'$'\016'-$'\037'$'\177']/ }
  REPLY=$s
}

# __k_lector FLUJO: lee líneas de stdin y las manda como eventos por el fd 4.
__k_lector() {
  local l
  while IFS= read -r l || [ -n "$l" ]; do
    LC_ALL=C esc "$l"
    printf '{"type":"%s","text":"%s\\n"}\n' "$1" "$REPLY" >&4
  done
}

__k_error() { # ENAME EVALUE
  LC_ALL=C esc "$2"
  printf '{"type":"error","ename":"%s","evalue":"%s","traceback":[]}\n' "$1" "$REPLY"
}

__k_ahora_ms() {
  if [ -n "${EPOCHREALTIME:-}" ]; then
    local t=${EPOCHREALTIME/[.,]/}
    REPLY=$((t / 1000))
  else
    REPLY=$((SECONDS * 1000))
  fi
}

# __k_guardar vuelca el estado del subshell: variables, funciones y cwd.
__k_guardar() {
  { declare -p; declare -f; printf 'cd -- %q\n' "$PWD"; } > "$__k_est.nuevo" 2>/dev/null &&
    mv -f "$__k_est.nuevo" "$__k_est"
}

# __k_hijo FICHERO SALIDA ERRORES: una ejecución, en el subshell. Todo lo que
# necesita llega por argumentos posicionales (no son variables: no entran en
# el estado, que no puede pisarlos al restaurarse).
__k_hijo() {
  exec 3>&- 4>&- 5>&- 6>&-
  exec >"$2" 2>"$3"
  . "$__k_est" 2>/dev/null
  trap '__k_guardar' EXIT
  . "$1"
}

__k_servir() {
  __k_fifo=$1
  __k_dir=${__k_fifo%/*}
  __k_est=$__k_dir/estado.sh
  __k_n=0
  __k_actual=""
  __k_int=0
  trap '' PIPE
  rm -f "$__k_fifo" "$__k_dir/listo"
  (umask 077; mkfifo "$__k_fifo") || exit 1
  exec 3<>"$__k_fifo"
  : > "$__k_est"
  set -m 2>/dev/null
  # Los avisos de trabajos ("Terminated") del control de trabajos no son de nadie.
  exec 2>/dev/null
  # SIGINT (DELETE .../execution): interrumpe la ejecución en curso; sin
  # ninguna, no hace nada.
  trap '[ -n "$__k_actual" ] && { __k_int=1; kill -INT -- "-$__k_actual" 2>/dev/null; }' INT
  : > "$__k_dir/listo"
  local plazo cod sal
  while read -r -u 3 plazo cod sal; do
    __k_n=$((__k_n + 1))
    __k_una "$plazo" "$cod" "$sal"
  done
}

__k_una() { # PLAZO FICHERO SALIDA
  local plazo=$1 cod=$2 sal=$3 so=$__k_dir/so se=$__k_dir/se
  local p1 p2 h w k rc t0 t1
  exec 4>"$sal" || { rm -f "$cod"; return; }
  rm -f "$so" "$se" "$__k_dir/tm"
  mkfifo "$so" "$se"
  __k_lector stdout <"$so" &
  p1=$!
  __k_lector stderr <"$se" &
  p2=$!
  __k_ahora_ms; t0=$REPLY
  __k_int=0
  __k_hijo "$cod" "$so" "$se" &
  h=$!
  __k_actual=$h
  if [ "$plazo" != 0 ]; then
    ( sleep "$plazo"; : > "$__k_dir/tm"; kill -TERM -- "-$h" 2>/dev/null; sleep 1; kill -KILL -- "-$h" 2>/dev/null ) &
    w=$!
  fi
  wait "$h"; rc=$?
  # Una señal con trap interrumpe wait: se espera de verdad a que acabe.
  while kill -0 "$h" 2>/dev/null; do wait "$h"; rc=$?; done
  __k_actual=""
  [ -n "$w" ] && { kill -- "-$w" 2>/dev/null; wait "$w" 2>/dev/null; }
  # Algo que el código dejó en segundo plano puede retener las tuberías.
  ( sleep 2; kill "$p1" "$p2" 2>/dev/null ) &
  k=$!
  wait "$p1" "$p2" 2>/dev/null
  kill -- "-$k" 2>/dev/null; wait "$k" 2>/dev/null
  local ok=true
  if [ -e "$__k_dir/tm" ]; then
    ok=false; __k_error TimeoutError "execution timed out after ${plazo}s" >&4
  elif [ "$__k_int" = 1 ]; then
    ok=false; __k_error KeyboardInterrupt "execution interrupted" >&4
  elif [ "$rc" != 0 ]; then
    ok=false; __k_error ExitStatus "exit status $rc" >&4
  fi
  __k_ahora_ms; t1=$REPLY
  printf '{"type":"done","ok":%s,"duration_ms":%d,"execution_count":%d}\n' "$ok" $((t1 - t0)) "$__k_n" >&4
  exec 4>&-
  rm -f "$cod" "$so" "$se"
}

__k_cliente() {
  local fifo=$1 plazo=$2 dir c o l visto=0 espera limite
  dir=${fifo%/*}; c=$dir/c.$$; o=$dir/o.$$
  cat > "$c" || exit 3
  trap 'rm -f "$c" "$o"' EXIT
  limite=$((SECONDS + 5))
  until [ -e "$dir/listo" ]; do
    if [ "$SECONDS" -ge "$limite" ]; then
      __k_error ContextError "the context is not answering"
      exit 2
    fi
    sleep 0.05
  done
  (umask 077; mkfifo "$o") || exit 3
  exec 5<>"$o"
  exec 6<>"$fifo"
  printf '%s %s %s\n' "$plazo" "$c" "$o" >&6
  exec 6>&-
  espera=$((plazo + 15))
  [ "$plazo" = 0 ] && espera=86400
  while IFS= read -r -t "$espera" -u 5 l; do
    printf '%s\n' "$l"
    case $l in
      *'"type":"done"'*) visto=1; break ;;
    esac
  done
  if [ "$visto" = 0 ]; then
    __k_error ContextError "the context ended the execution without finishing it"
    exit 3
  fi
}

case "$1" in
  serve) [ $# = 2 ] && { __k_servir "$2"; exit 0; } ;;
  run) [ $# = 3 ] && { __k_cliente "$2" "$3"; exit 0; } ;;
esac
echo "usage: conductor.sh serve FIFO | run FIFO TIMEOUT" >&2
exit 64
