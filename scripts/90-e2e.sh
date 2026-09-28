#!/usr/bin/env bash
# Prueba de extremo a extremo contra un daemon REAL.
#
# Los tests de Go cubren la lógica, pero no pueden cubrir lo que de verdad se
# rompe en este proyecto: que una microVM arranque, que su red funcione, que el
# snapshot dorado despierte y que un volumen sobreviva a la máquina. (El gateway
# y el resto de lo MCP tienen su propio e2e en kindling-mcp.) Todo eso necesita KVM, y por eso vive aquí y no en el
# CI.
#
#   ./90-e2e.sh                      contra el contexto activo de kling
#   KLING_HOST=ssh://lab ./90-e2e.sh
#   KEEP=1 ./90-e2e.sh               no limpia al terminar (para inspeccionar)
#
# Cada comprobación dice qué esperaba y qué obtuvo. Un fallo NO aborta el resto:
# saber que fallan tres cosas relacionadas vale más que enterarse de una.
set -uo pipefail

KLING="${KLING:-kling}"
VOL="${VOL:-e2e-vol}"
VOL2="${VOL2:-e2e-vol2}"
VOL3="${VOL3:-e2e-vol3}"
# Los volúmenes los monta el agente de invitado, y la imagen mínima no lo lleva:
# el daemon rechaza montarlos ahí a propósito, porque el disco se engancharía y
# nadie lo montaría. Para los bloques de volúmenes hace falta una imagen con
# agente: la de herramientas (`kling image toolchain`) lleva kling-guest.
IMGVOL="${IMGVOL:-toolchain}"
KEEP="${KEEP:-0}"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFALLO\033[0m %s\n     esperaba: %s\n     obtuvo:   %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }

# contiene busca una subcadena SIN tuberías.
#
# `algo | grep -q X` es una trampa con `set -o pipefail`: grep sale en cuanto
# encuentra la coincidencia y cierra la tubería, el productor recibe SIGPIPE y
# sale distinto de cero, y la tubería entera se da por fallida AUNQUE el texto
# estuviera. Una prueba que falla sobre una función que funciona es peor que no
# tenerla: manda a corregir lo que no está roto.
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }

need() { command -v "$1" >/dev/null || { echo "falta $1" >&2; exit 1; }; }
need "$KLING"; need curl; need python3

cleanup() {
  [ "$KEEP" = "1" ] && { echo; echo "KEEP=1: no limpio. Restos: volumen $VOL"; return; }
  echo
  echo "limpiando..."
  for m in $($KLING ps -a 2>/dev/null | awk '/e2e-/ {print $1}'); do
    $KLING rm "$m" >/dev/null 2>&1
  done
  $KLING volume rm "$VOL" >/dev/null 2>&1
  $KLING volume rm "$VOL2" >/dev/null 2>&1
  $KLING volume rm -f -snapshots "$VOL3" >/dev/null 2>&1
}
trap cleanup EXIT

# ── 1. el daemon está y tiene lo que hace falta ───────────────────────────────
step "1. Daemon"
info=$($KLING status -v 2>&1) || { echo "$info"; echo "no alcanzo el daemon"; exit 1; }
# Los espacios de la tabla son variables, así que se aplasta antes de comparar.
kvm=$(printf '%s' "$info" | tr -s ' ' | grep -i '^KVM:' || true)
contiene "$kvm" "yes" && ok "KVM disponible" || bad "KVM" "KVM: yes" "${kvm:-nada}"
contiene "$info" "irecracker" && ok "firecracker instalado" \
  || bad "firecracker" "una versión" "nada"

# ── 2. ciclo de vida: arrancar, congelar, descongelar ─────────────────────────
step "2. Ciclo de vida de una microVM"
NAME="e2e-vida-$$"
out=$($KLING run -name "$NAME" -image min 2>&1)
if contiene "$out" "booted cold"; then
  ok "arranque en frío: $(echo "$out" | grep -o '[0-9]* ms' | head -1)"
else
  bad "run" "una máquina arrancada" "$out"
fi
# El CLI dice "frozen" desde que el estado warm se muestra así al usuario.
out=$($KLING freeze "$NAME" 2>&1)
contiene "$out" "frozen" && ok "freeze -> frozen ($(echo "$out" | grep -o '[0-9]* ms' | head -1))" \
  || bad "freeze" "estado frozen" "$out"

# El thaw es LA cifra del proyecto: si sube de 200 ms, algo se rompió.
out=$($KLING thaw "$NAME" 2>&1)
ms=$(echo "$out" | grep -oE '\(([0-9]+) ms\)' | grep -oE '[0-9]+' | head -1)
if [ -n "$ms" ] && [ "$ms" -lt 200 ]; then
  ok "thaw en ${ms} ms (el objetivo del proyecto es ~30)"
elif [ -n "$ms" ]; then
  bad "thaw" "menos de 200 ms" "${ms} ms"
else
  bad "thaw" "una máquina running" "$out"
fi
$KLING rm "$NAME" >/dev/null 2>&1

# ── 3. volumen que sobrevive a la microVM ────────────────────────────────────
# Este bloque existe porque la persistencia falló de tres formas distintas, y
# ninguna daba un error: dos microVMs montando el mismo ext4, un volumen sin
# journal, y un snapshot que no recordaba con qué volumen se importó. Las tres
# terminaban en escrituras que decían "success" y luego no estaban.
step "3. Volumen persistente"
$KLING volume rm "$VOL" >/dev/null 2>&1
out=$($KLING volume create "$VOL" -size 256M 2>&1)
contiene "$out" "created" && ok "volumen creado" || bad "volume create" "creado" "$out"

# CON journal. Sin él, matar el VMM —que es como se para una microVM— deja el
# sistema de ficheros incoherente y sin nada que reproducir: el siguiente
# arranque monta sin quejarse y el primer read devuelve EBADMSG.
if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
  feats=$(ssh "${KLING_HOST#ssh://}" \
    "sudo dumpe2fs -h /var/lib/kindling/volumes/$VOL.ext4 2>/dev/null | grep -i '^Filesystem features'")
  contiene "$feats" "has_journal" && ok "formateado con journal" \
    || bad "journal" "has_journal entre las features" "${feats:-no pude leerlo}"
fi

NAME="e2e-vol-$$"
if $KLING run -name "$NAME" -image "$IMGVOL" -volume "$VOL" >/dev/null 2>&1; then
  # No se puede BORRAR mientras alguien lo usa.
  out=$($KLING volume rm "$VOL" 2>&1)
  contiene "$out" "is used by" && ok "se niega a borrar un volumen en uso" \
    || bad "volume rm en uso" "un rechazo" "$out"

  # Y tampoco se puede MONTAR dos veces. Un ext4 no admite dos escritores: cada
  # uno cachea metadatos que el otro no ve y el resultado es corrupción. Esta
  # comprobación es lo único que lo impide.
  out=$($KLING run -name "$NAME-bis" -image "$IMGVOL" -volume "$VOL" 2>&1)
  if contiene "$out" "in WRITE mode"; then
    ok "se niega a montar el mismo volumen en dos escritores"
  else
    bad "doble montaje" "un rechazo nombrando a $NAME" "$out"
    $KLING rm "$NAME-bis" >/dev/null 2>&1
  fi

  # Con un escritor dentro no entra NADIE, ni a leer: vería metadatos cambiando
  # bajo sus pies.
  out=$($KLING run -name "$NAME-ro" -image "$IMGVOL" -volume "$VOL:/x:ro" 2>&1)
  if contiene "$out" "in WRITE mode"; then
    ok "un escritor bloquea también a los lectores"
  else
    bad "lector con escritor dentro" "un rechazo" "$out"
    $KLING rm "$NAME-ro" >/dev/null 2>&1
  fi

  # Aparece a quién pertenece, que es lo que hace comprensible el rechazo.
  out=$($KLING volume ls 2>&1)
  contiene "$out" "$NAME" && ok "volume ls dice quién lo usa" \
    || bad "volume ls" "el nombre de la máquina que lo monta" "$out"

  $KLING rm "$NAME" >/dev/null 2>&1
else
  bad "run -volume" "una máquina con volumen" "no arrancó"
fi

# Liberado tras destruir la máquina, y el sistema de ficheros queda limpio: el
# daemon le pide al invitado que vacíe la caché antes de matarlo.
out=$($KLING volume ls 2>&1 | grep "$VOL " || true)
contiene "$out" "—" && ok "se libera al destruir la máquina" \
  || bad "liberación" "sin usuarios" "$out"
if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
  st=$(ssh "${KLING_HOST#ssh://}" \
    "sudo dumpe2fs -h /var/lib/kindling/volumes/$VOL.ext4 2>/dev/null | grep -i '^Filesystem state'")
  contiene "$st" "clean" && ok "queda limpio tras matar el VMM" \
    || bad "estado del ext4" "clean" "${st:-no pude leerlo}"
fi

# Un escritor, o muchos lectores. Es lo que hace posible una biblioteca de
# paquetes compartida sin duplicarla en cada imagen.
step "3b. Volumen compartido en solo lectura"
LECTORES=0
for i in 1 2 3; do
  if $KLING run -name "e2e-lec-$i-$$" -image "$IMGVOL" -volume "$VOL:/libs:ro" >/dev/null 2>&1; then
    LECTORES=$((LECTORES+1))
  fi
done
[ "$LECTORES" = "3" ] && ok "tres microVMs lo montan a la vez en lectura" \
  || bad "lectores concurrentes" "3" "$LECTORES"

# Y con lectores dentro no entra un escritor.
out=$($KLING run -name "e2e-esc-$$" -image "$IMGVOL" -volume "$VOL" 2>&1)
if contiene "$out" "is being read"; then
  ok "los lectores bloquean al escritor"
else
  bad "escritor con lectores dentro" "un rechazo" "$out"
  $KLING rm "e2e-esc-$$" >/dev/null 2>&1
fi
for i in 1 2 3; do $KLING rm "e2e-lec-$i-$$" >/dev/null 2>&1; done

# Varios volúmenes en la misma microVM: el caso que motivó todo esto.
$KLING volume rm "$VOL2" >/dev/null 2>&1
$KLING volume create "$VOL2" -size 128M >/dev/null 2>&1
NAME="e2e-multi-$$"
if $KLING run -name "$NAME" -image "$IMGVOL" -volume "$VOL:/uno" -volume "$VOL2:/dos:ro" >/dev/null 2>&1; then
  ok "una microVM con dos volúmenes"
  # volume ls debe distinguir quién escribe de quién lee.
  out=$($KLING volume ls 2>&1)
  contiene "$out" "escritura" && ok "volume ls distingue escritor de lectores" \
    || bad "volume ls" "marca de (escritura)" "$out"
  $KLING rm "$NAME" >/dev/null 2>&1
else
  bad "dos volúmenes" "una máquina con ambos" "no arrancó"
fi

# Dos volúmenes en el mismo punto de montaje: el segundo taparía al primero.
out=$($KLING run -name "e2e-choque-$$" -image "$IMGVOL" -volume "$VOL:/x" -volume "$VOL2:/x" 2>&1)
contiene "$out" "would shadow" && ok "rechaza dos volúmenes en el mismo punto" \
  || bad "puntos de montaje repetidos" "un rechazo" "$out"
$KLING rm "e2e-choque-$$" >/dev/null 2>&1

# ── 3c. snapshots de volumen y restore ───────────────────────────────────────
# Un snapshot solo sin escritores (los lectores no cambian los bloques) y un
# restore solo sin nadie; lo anterior al restore queda en <vol>@undo. Se escribe
# de verdad dentro del invitado y se lee de vuelta: comparar ficheros del host
# no diría nada de si el ext4 restaurado monta y contiene lo que tenía.
step "3c. Snapshots de volumen y restore"
$KLING volume rm -f -snapshots "$VOL3" >/dev/null 2>&1
$KLING volume create "$VOL3" -size 128M >/dev/null 2>&1
# escribe_vol <contenido>: una microVM de usar y tirar deja el fichero en /data.
escribe_vol() {
  local n="e2e-snap-w-$$"
  $KLING run -name "$n" -image "$IMGVOL" -volume "$VOL3" -allow-exec -ttl 5m >/dev/null 2>&1 || return 1
  $KLING exec "$n" -- sh -c "echo $1 > /data/f && sync" >/dev/null 2>&1
  $KLING rm "$n" >/dev/null 2>&1
}
lee_vol() {
  local n="e2e-snap-r-$$" out
  $KLING run -name "$n" -image "$IMGVOL" -volume "$VOL3:/data:ro" -allow-exec -ttl 5m >/dev/null 2>&1 || { echo "no arrancó"; return; }
  out=$($KLING exec "$n" -- cat /data/f 2>&1)
  $KLING rm "$n" >/dev/null 2>&1
  echo "$out"
}
if escribe_vol v1; then
  out=$($KLING volume snapshot "$VOL3" antes 2>&1)
  if contiene "$out" "taken"; then
    # El modo es informativo: reflink en XFS/Btrfs, copy en ext4.
    ok "snapshot tomado: $(printf '%s' "$out" | head -1)"
  else
    bad "volume snapshot" "taken" "$out"
  fi
  out=$($KLING volume snapshot "$VOL3" undo 2>&1)
  contiene "$out" "reserved" && ok "undo está reservado" || bad "snapshot undo" "reserved" "$out"

  # Con un escritor dentro, ni snapshot ni restore; congelada sigue contando.
  n="e2e-snap-busy-$$"
  if $KLING run -name "$n" -image "$IMGVOL" -volume "$VOL3" >/dev/null 2>&1; then
    out=$($KLING volume snapshot "$VOL3" durante 2>&1)
    contiene "$out" "WRITE mode" && ok "no hay snapshot con un escritor" || bad "snapshot con escritor" "WRITE mode" "$out"
    out=$($KLING volume restore -f "$VOL3" antes 2>&1)
    contiene "$out" "is used by" && ok "no hay restore con la máquina viva" || bad "restore en uso" "is used by" "$out"
    if out=$($KLING freeze "$n" 2>&1); then
      out=$($KLING volume snapshot "$VOL3" durante 2>&1)
      contiene "$out" "WRITE mode" && ok "una congelada cuenta como escritor" || bad "snapshot con congelada" "WRITE mode" "$out"
    else
      bad "freeze de una máquina con volumen (3c)" "frozen" "$out"
    fi
    $KLING rm "$n" >/dev/null 2>&1
  else
    bad "run con volumen (3c)" "una máquina" "no arrancó"
  fi
  # Con un lector sí: no cambia los bloques.
  n="e2e-snap-ro-$$"
  if $KLING run -name "$n" -image "$IMGVOL" -volume "$VOL3:/data:ro" >/dev/null 2>&1; then
    out=$($KLING volume snapshot "$VOL3" con-lector 2>&1)
    contiene "$out" "taken" && ok "snapshot con un lector dentro" || bad "snapshot con lector" "taken" "$out"
    $KLING rm "$n" >/dev/null 2>&1
  else
    bad "run -volume :ro (3c)" "una máquina" "no arrancó"
  fi

  escribe_vol v2
  out=$($KLING volume restore -f "$VOL3" antes 2>&1)
  contiene "$out" "restored" && ok "restore" || bad "volume restore" "restored" "$out"
  out=$(lee_vol)
  [ "$out" = "v1" ] && ok "tras el restore el invitado lee v1" || bad "contenido restaurado" "v1" "$out"
  $KLING volume restore -f "$VOL3" undo >/dev/null 2>&1
  out=$(lee_vol)
  [ "$out" = "v2" ] && ok "restore de undo deshace el restore (v2)" || bad "undo" "v2" "$out"

  out=$($KLING volume snapshots "$VOL3" -q 2>&1)
  contiene "$out" "antes" && contiene "$out" "undo" && ok "volume snapshots lista antes y undo" \
    || bad "volume snapshots" "antes y undo" "$out"
  out=$($KLING volume ls 2>&1 | grep "$VOL3 " || true)
  contiene "$out" " 3 " && ok "volume ls cuenta 3 snapshots" || bad "SNAPS" "3" "$out"
  if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
    perms=$(ssh "${KLING_HOST#ssh://}" \
      "sudo stat -c '%a %U' /var/lib/kindling/volumes/snapshots /var/lib/kindling/volumes/snapshots/$VOL3 /var/lib/kindling/volumes/snapshots/$VOL3/antes.ext4 | tr '\n' ' '")
    [ "$perms" = "700 root 700 root 600 root " ] && ok "snapshots de root: 0700/0700/0600" \
      || bad "permisos de snapshots" "700 root 700 root 600 root" "${perms:-no pude leerlo}"
  fi

  out=$($KLING volume rm -f "$VOL3" 2>&1)
  contiene "$out" "-snapshots" && ok "no borra un volumen con snapshots sin -snapshots" \
    || bad "volume rm con snapshots" "un rechazo que diga -snapshots" "$out"
  out=$($KLING volume rm -f "$VOL3@con-lector" 2>&1)
  contiene "$out" "removed" && ok "volume rm vol@snap" || bad "rm vol@snap" "removed" "$out"
  out=$($KLING volume rm -f -snapshots "$VOL3" 2>&1)
  contiene "$out" "removed" && ok "volume rm -snapshots se lo lleva todo" || bad "rm -snapshots" "removed" "$out"
else
  bad "escribir en el volumen (3c)" "una máquina con exec" "no arrancó"
fi

# ── 4. reconcile no destruye máquinas vivas ──────────────────────────────────
# El caso que motivó reescribirlo: el daemon se reinicia y una microVM viva NO
# debe perder su red ni su cgroup, aunque el estado en disco vaya por detrás.
step "4. El daemon se reinicia sin llevarse las microVMs por delante"
NAME="e2e-rec-$$"
if $KLING run -name "$NAME" -image min >/dev/null 2>&1; then
  if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
    T="${KLING_HOST#ssh://}"
    ssh "$T" 'sudo systemctl restart kling' >/dev/null 2>&1
    sleep 4
    state=$($KLING ps -a 2>/dev/null | awk -v n="$NAME" '$0 ~ n {print $4}')
    [ "$state" = "running" ] && ok "sobrevive al reinicio del daemon (sigue running)" \
      || bad "reconcile" "running" "${state:-desaparecida}"
  else
    echo "  (daemon local: me salto el reinicio para no matar tu sesión)"
  fi
  $KLING rm "$NAME" >/dev/null 2>&1
else
  bad "run" "una máquina" "no arrancó"
fi

# ── 5. exec, ficheros y sandboxes ─────────────────────────────────────────────
# Lo que usa un agente de código. Tres cosas que no se ven sin KVM: que el
# agente del invitado ejecute y trocee la salida, que la puerta de exec viaje con
# el snapshot, y que un sandbox se destruya solo al vencer su TTL.
step "5. Exec, ficheros y sandboxes"
SB="e2e-sb-$$"
if $KLING sandbox create -image "$IMGVOL" -name "$SB" -ttl 5m -q >/dev/null 2>&1; then
  out=$($KLING exec "$SB" -- sh -c 'echo out; echo err >&2; exit 3' 2>&1); code=$?
  [ "$code" = "3" ] && contiene "$out" "out" && contiene "$out" "err" \
    && ok "exec: salida de los dos flujos y código de salida 3" \
    || bad "exec" "out, err y código 3" "código $code: $out"

  out=$(echo hola | $KLING exec -i "$SB" -- tr a-z A-Z 2>&1)
  [ "$out" = "HOLA" ] && ok "exec -i: stdin llega al comando" || bad "exec -i" "HOLA" "$out"

  out=$($KLING exec "$SB" -- sh -c 'wget -q -T 3 -O- http://example.com >/dev/null && echo RED || echo SIN-RED' 2>/dev/null)
  contiene "$out" "SIN-RED" && ok "sin red por defecto" || bad "egress de un sandbox" "SIN-RED" "$out"

  $KLING exec -timeout 2s "$SB" -- sleep 30 >/dev/null 2>&1; code=$?
  [ "$code" = "137" ] && ok "el plazo mata el comando (137)" || bad "exec -timeout" "137" "$code"

  # Shell interactiva. Necesita un terminal, así que se le pone uno falso. Con
  # el pty de python3 (que ya exigimos) y no con `script`: el `script -qec` de
  # util-linux no existe en el `script` BSD de macOS, desde donde se lanza este
  # e2e contra el lab, y las dos comprobaciones fallaban por la herramienta,
  # no por kling. pty.spawn devuelve el estado de waitpid del hijo.
  conpty() {
    python3 -c '
import os, pty, sys
st = pty.spawn(sys.argv[1:])
sys.exit(os.waitstatus_to_exitcode(st) if hasattr(os, "waitstatus_to_exitcode") else (st >> 8))
' "$@"
  }
  out=$(printf 'tty; stty size; exit 5\n' | conpty $KLING shell "$SB" 2>&1 | tr -d '\r')
  contiene "$out" "/dev/pts/" && ok "shell: hay un pseudoterminal de verdad dentro" \
    || bad "kling shell" "un /dev/pts/N" "$out"
  # El código de la shell remota tiene que llegar al proceso local.
  printf 'exit 5\n' | conpty $KLING shell "$SB" >/dev/null 2>&1
  code=$?
  [ "$code" = "5" ] && ok "shell: el código de salida remoto llega al local" \
    || bad "código de kling shell" "5" "$code"
  out=$($KLING shell "$SB" </dev/null 2>&1)
  contiene "$out" "needs a terminal" && ok "shell: se niega sin terminal, y lo explica" \
    || bad "kling shell sin tty" "un rechazo explicando" "$out"

  # Congelar y despertar un sandbox sobre una imagen POR CAPAS. El ciclo de
  # vida de arriba usa `min`, que es monolítica, y con jailer activo el thaw de
  # una imagen por capas fallaba enlazando una ruta que no existe. Un exec sobre
  # una máquina congelada la despierta sola.
  if $KLING freeze "$SB" >/dev/null 2>&1; then
    out=$($KLING exec "$SB" -- echo despierto 2>&1)
    [ "$out" = "despierto" ] && ok "exec despierta un sandbox congelado (imagen por capas)" \
      || bad "thaw de una imagen por capas" "despierto" "$out"
  else
    bad "freeze de un sandbox" "congelado" "falló"
  fi

  tmp=$(mktemp); echo "print(6*7)" > "$tmp"
  if $KLING cp "$tmp" "$SB:/tmp/e2e.py" >/dev/null 2>&1; then
    out=$($KLING exec "$SB" -- python3 /tmp/e2e.py 2>&1)
    [ "$out" = "42" ] && ok "cp dentro y ejecutar" || bad "cp + python3" "42" "$out"
    out=$($KLING cp "$SB:/tmp/e2e.py" - 2>&1)
    [ "$out" = "print(6*7)" ] && ok "cp fuera" || bad "cp fuera" "print(6*7)" "$out"
  else
    bad "cp" "copia" "falló"
  fi
  rm -f "$tmp"
  $KLING sandbox rm "$SB" >/dev/null 2>&1
else
  bad "sandbox create" "un sandbox sobre $IMGVOL" "no se creó"
fi

# Una máquina sin allow_exec no ejecuta nada: es lo que protege a los servicios.
NAME="e2e-noexec-$$"
if $KLING run -name "$NAME" -image "$IMGVOL" >/dev/null 2>&1; then
  out=$($KLING exec "$NAME" -- true 2>&1)
  contiene "$out" "not enabled" && ok "exec rechazado sin allow_exec" || bad "exec sin allow_exec" "un rechazo" "$out"
  $KLING rm "$NAME" >/dev/null 2>&1
fi

# La puerta viaja con el snapshot: un sandbox desde él arranca en milisegundos
# con el estado de la plantilla.
TPL="e2e-tpl-$$"; SNAP="e2e-exec-snap-$$"
if $KLING run -name "$TPL" -image "$IMGVOL" -allow-exec >/dev/null 2>&1 \
   && $KLING exec "$TPL" -- sh -c 'echo plantilla > /root/marca' >/dev/null 2>&1 \
   && $KLING save "$TPL" "$SNAP" >/dev/null 2>&1; then
  out=$($KLING sandbox create -from "$SNAP" -name "$SB-snap" -q 2>&1) \
    && out=$($KLING exec "$SB-snap" -- cat /root/marca 2>&1)
  [ "$out" = "plantilla" ] && ok "sandbox desde un snapshot con exec, con su estado" \
    || bad "sandbox -from" "plantilla" "$out"
  $KLING sandbox rm "$SB-snap" >/dev/null 2>&1
else
  bad "snapshot con exec" "run -allow-exec + commit" "falló"
fi
$KLING rm "$TPL" >/dev/null 2>&1; $KLING template rm "$SNAP" >/dev/null 2>&1

# Al vencer el TTL, un sandbox se destruye (no se congela).
if $KLING sandbox create -image "$IMGVOL" -name "$SB-ttl" -ttl 10s -q >/dev/null 2>&1; then
  sleep 25
  out=$($KLING ps -a 2>&1)
  contiene "$out" "$SB-ttl" && bad "TTL de un sandbox" "destruido" "sigue en ps -a" \
    || ok "el sandbox se destruye al vencer su TTL"
fi

# ── 6. carpetas compartidas ──────────────────────────────────────────────────
# Copia (un ext4 que sube el CLI), y las vivas (ro/rw, FUSE en el invitado y el
# daemon sirviendo desde el host). Lo que solo se ve con KVM: que el agente monte
# de verdad, que el kernel del invitado hable nuestro FUSE, que nada salga de la
# carpeta, y que la carpeta sobreviva a congelar y a reiniciar el daemon.
#
# Las vivas necesitan un directorio del host del daemon bajo daemon.share_roots:
# SHARE_ROOT (por defecto ~/kling-e2e-shares en ese host) tiene que estar
# permitido; si no, la sección lo dice y se salta las vivas. Salida en inglés.
step "6. Shared folders"
failE() { printf "  \033[31mFAIL\033[0m  %s\n        expected: %s\n        got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
SSHT=""
if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then SSHT="${KLING_HOST#ssh://}"; fi
# hostsh corre algo en el host del daemon: por ssh, o aquí si es local.
hostsh() { if [ -n "$SSHT" ]; then ssh "$SSHT" "$1"; else sh -c "$1"; fi; }
SH="e2e-sh-$$"
SHARE_ROOT="${SHARE_ROOT:-$(hostsh 'echo $HOME')/kling-e2e-shares}"
HD="$SHARE_ROOT/run-$$"

# copy: un directorio LOCAL con cosas que no deben pasar (enlace fuera, FIFO).
src=$(mktemp -d)
mkdir -p "$src/sub"; echo "copied" > "$src/a.txt"; head -c 1048576 /dev/urandom > "$src/sub/blob"
ln -s a.txt "$src/rel-link"; ln -s /etc/passwd "$src/abs-link"; mkfifo "$src/fifo"
out=$($KLING run -name "$SH-copy" -image "$IMGVOL" -allow-exec -share "$src:/work" 2>&1)
if contiene "$out" "booted cold"; then
  contiene "$out" "skipped abs-link" && contiene "$out" "skipped fifo" \
    && ok "copy: the CLI skips a symlink out of the tree and a FIFO, and says so" \
    || failE "copy skip notice" "skipped abs-link and fifo" "$out"
  out=$($KLING exec "$SH-copy" -- sh -c 'cat /work/a.txt /work/rel-link; wc -c < /work/sub/blob; ls /work' 2>&1)
  contiene "$out" "copied
copied
1048576" && ok "copy: contents and relative symlink inside the guest" || failE "copy contents" "copied x2, 1048576" "$out"
  contiene "$out" "abs-link" && failE "copy: absolute symlink" "not copied" "$out" || true
  out=$($KLING exec "$SH-copy" -- sh -c 'echo x > /work/new 2>&1; echo rc=$?' 2>&1)
  contiene "$out" "Read-only" && ok "copy: read-only inside" || failE "copy write" "Read-only file system" "$out"
  out=$($KLING save "$SH-copy" "$SH-snap" 2>&1)
  contiene "$out" "cannot be committed" && ok "commit of a machine with a share is refused" \
    || failE "commit with a share" "cannot be committed" "$out"
  $KLING template rm "$SH-snap" >/dev/null 2>&1
else
  failE "run -share (copy)" "a machine" "$out"
fi
$KLING rm "$SH-copy" >/dev/null 2>&1
rm -rf "$src"

roots=$($KLING status -v 2>&1 | tr -s ' ' | grep '^share roots:' || true)
allowed=0
for r in $(printf '%s' "${roots#share roots: }" | tr ',' ' '); do
  case "$SHARE_ROOT/" in "$r"/*) allowed=1;; esac
done
if [ "$allowed" = 0 ]; then
  echo "  (live shares skipped: $SHARE_ROOT is not under daemon.share_roots ($roots)."
  echo "   Allow it on the daemon host: sudo kling config set daemon.share_roots $SHARE_ROOT)"
else
  hostsh "mkdir -p $HD/ro $HD/rw && echo one > $HD/ro/f.txt && ln -s /etc $HD/rw/etc \
    && echo 'host secret' > $SHARE_ROOT/secret-$$ && ln -s ../../secret-$$ $HD/rw/up \
    && ln -s $SHARE_ROOT/secret-$$ $HD/rw/abs"

  # ro, y con egress none (el defecto): la carpeta no usa la red del invitado.
  out=$($KLING run -name "$SH-ro" -image "$IMGVOL" -allow-exec -share "$HD/ro:/src:ro" 2>&1)
  if contiene "$out" "booted cold"; then
    out=$($KLING exec "$SH-ro" -- cat /src/f.txt 2>&1)
    [ "$out" = "one" ] && ok "ro: the guest reads the host folder" || failE "ro read" "one" "$out"
    hostsh "echo two > $HD/ro/f.txt"; sleep 1.5
    out=$($KLING exec "$SH-ro" -- cat /src/f.txt 2>&1)
    [ "$out" = "two" ] && ok "ro: a change on the host shows up inside" || failE "ro host edit" "two" "$out"
    out=$($KLING exec "$SH-ro" -- sh -c 'touch /src/x 2>&1; rm /src/f.txt 2>&1; mkdir /src/d 2>&1; true' 2>&1)
    n=$(printf '%s\n' "$out" | grep -c 'Read-only' || true)
    [ "$n" = "3" ] && ok "ro: create, remove and mkdir fail with EROFS" || failE "ro writes" "3 x Read-only" "$out"
    out=$($KLING exec "$SH-ro" -- sh -c 'wget -q -T 3 -O- http://example.com >/dev/null && echo NET || echo NO-NET' 2>&1)
    contiene "$out" "NO-NET" && ok "ro: works with egress none (no guest network)" || failE "egress none" "NO-NET" "$out"
  else
    failE "run -share :ro" "a machine" "$out"
  fi
  $KLING rm "$SH-ro" >/dev/null 2>&1

  out=$($KLING run -name "$SH-rw" -image "$IMGVOL" -mem 512 -allow-exec -share "$HD/rw:/w:rw" 2>&1)
  if contiene "$out" "booted cold"; then
    $KLING exec "$SH-rw" -- sh -c 'cd /w && echo hi > f1 && mkdir -p d/e && mv f1 d/e/f2 && echo more >> d/e/f2 && cp d/e/f2 f3 && rm f3 && chmod 4755 d/e/f2' >/dev/null 2>&1
    out=$(hostsh "cat $HD/rw/d/e/f2; ls $HD/rw; stat -c %a $HD/rw/d/e/f2")
    contiene "$out" "hi
more" && ! contiene "$out" "f3" && contiene "$out" "755" && ! contiene "$out" "4755" \
      && ok "rw: create, mkdir, rename, append, unlink reach the host (setuid dropped)" \
      || failE "rw on the host" "hi/more, no f3, mode 755" "$out"

    # Nada sale de la carpeta: los enlaces del host se resuelven DENTRO del
    # invitado (su /etc, no el del host), el secreto del host no se lee, y el
    # invitado no puede crear enlaces ni nodos.
    out=$($KLING exec "$SH-rw" -- sh -c 'cat /w/up 2>&1; cat /w/abs 2>&1; head -1 /w/etc/os-release; ln -s /etc/shadow /w/l 2>&1; ln /w/d/e/f2 /w/h 2>&1; mkfifo /w/p 2>&1; true' 2>&1)
    ! contiene "$out" "host secret" && contiene "$out" "Alpine" \
      && [ "$(printf '%s\n' "$out" | grep -c 'not permitted' || true)" = "3" ] \
      && ok "rw: host symlinks resolve in the guest, no symlink/hardlink/mknod, host secret unreadable" \
      || failE "escape attempts" "no 'host secret', guest os-release, 3 x not permitted" "$out"

    # Carga tipo npm install: el paquete npm entero (miles de ficheros) se
    # extrae en la carpeta y se ejecuta desde ella.
    out=$($KLING exec -timeout 5m "$SH-rw" -- sh -c 'tar cf /tmp/npm.tar -C /usr/lib/node_modules npm && mkdir /w/nm && s=$(date +%s) && tar xf /tmp/npm.tar -C /w/nm && e=$(date +%s) && echo files=$(find /w/nm -type f | wc -l) secs=$((e-s)) && node /w/nm/npm/bin/npm-cli.js --version' 2>&1)
    hf=$(hostsh "find $HD/rw/nm -type f | wc -l" | tr -d ' ')
    contiene "$out" "files=$hf" && [ "${hf:-0}" -gt 100 ] \
      && ok "rw: extracted npm into the share ($(echo "$out" | head -1)) and ran it: $(echo "$out" | tail -1)" \
      || failE "npm-like workload" "same file count inside and on the host, npm --version" "$out / host $hf"

    # Caudal: lo acota el limitador de red del invitado (16 MiB/s por sentido).
    out=$($KLING exec -timeout 5m "$SH-rw" -- sh -c 'dd if=/dev/zero of=/w/big bs=1M count=64 conv=fsync 2>&1 | tail -1; echo 3 > /proc/sys/vm/drop_caches; dd if=/w/big of=/dev/null bs=1M 2>&1 | tail -1; rm /w/big' 2>&1)
    info_w=$(echo "$out" | sed -n 1p | grep -o '[0-9.]*MB/s' || true)
    info_r=$(echo "$out" | sed -n 2p | grep -o '[0-9.]*MB/s' || true)
    [ -n "$info_w" ] && [ -n "$info_r" ] && ok "rw: sequential write $info_w, read $info_r" \
      || failE "throughput" "two dd results" "$out"
    out=$($KLING exec -timeout 5m "$SH-rw" -- python3 -c '
import os, time
d="/w/small"; os.makedirs(d); N=500
t=time.time()
for i in range(N): open(f"{d}/f{i}","w").write("x"*1024)
c=time.time()-t; t=time.time()
for i in range(N): os.stat(f"{d}/f{i}")
s=time.time()-t; t=time.time()
for i in range(N): os.unlink(f"{d}/f{i}")
u=time.time()-t
print(f"create {N/c:.0f}/s stat {N/s:.0f}/s unlink {N/u:.0f}/s")' 2>&1)
    contiene "$out" "create" && ok "rw: small files: $out" || failE "small files" "ops/s" "$out"

    # Congelar y descongelar con un proceso escribiendo por un fichero abierto.
    $KLING exec "$SH-rw" -- sh -c 'setsid python3 -c "
import time
f=open(\"/w/log\",\"a\",buffering=1)
while True:
    f.write(str(time.time())+chr(10)); time.sleep(0.1)
" >/tmp/writer.err 2>&1 </dev/null &' >/dev/null 2>&1
    sleep 2
    $KLING freeze "$SH-rw" >/dev/null 2>&1 && $KLING thaw "$SH-rw" >/dev/null 2>&1
    n1=$(hostsh "wc -l < $HD/rw/log" | tr -d ' '); sleep 2; n2=$(hostsh "wc -l < $HD/rw/log" | tr -d ' ')
    out=$($KLING exec "$SH-rw" -- sh -c 'cat /tmp/writer.err; echo after > /w/after; cat /w/after' 2>&1)
    [ "${n2:-0}" -gt "${n1:-0}" ] && [ "$out" = "after" ] \
      && ok "rw: freeze -> thaw keeps the mount and the open file ($n1 -> $n2 lines)" \
      || failE "freeze/thaw with a live share" "the log grows, no errors" "$n1 -> $n2, $out"

    if [ -n "$SSHT" ]; then
      ssh "$SSHT" 'sudo systemctl restart kling' >/dev/null 2>&1
      sleep 5
      n1=$(hostsh "wc -l < $HD/rw/log" | tr -d ' '); sleep 2; n2=$(hostsh "wc -l < $HD/rw/log" | tr -d ' ')
      st=$($KLING inspect "$SH-rw" 2>&1 | grep '"status"' || true)
      [ "${n2:-0}" -gt "${n1:-0}" ] && contiene "$st" "attached" \
        && ok "rw: the daemon restarts and re-attaches the share" \
        || failE "re-attach after a daemon restart" "the log grows, status attached" "$n1 -> $n2, $st"
    else
      echo "  (local daemon: skipping the restart)"
    fi
  else
    failE "run -share :rw" "a machine" "$out"
  fi
  $KLING rm "$SH-rw" >/dev/null 2>&1

  # Fuera de las raíces permitidas: rechazo que dice cómo permitirlo.
  out=$($KLING run -name "$SH-bad" -image "$IMGVOL" -share "/etc:/w:rw" 2>&1)
  contiene "$out" "not under any allowed share root" && ok "a folder outside share_roots is refused" \
    || failE "share outside roots" "not under any allowed share root" "$out"
  $KLING rm "$SH-bad" >/dev/null 2>&1
  hostsh "rm -rf $HD $SHARE_ROOT/secret-$$"
fi

# ── 7. proxy de credenciales ─────────────────────────────────────────────────
# El invitado es hostil: con una clave inyectada por MMDS, un servidor
# comprometido la lee y la saca por un dominio permitido. Con el proxy la clave
# no entra: el invitado ve un marcador y el proxy lo cambia por la clave solo
# hacia su dominio. Necesita salida a internet desde el host (httpbin.org).
#
# httpbin.org/basic-auth/<usuario>/<clave> contesta 200 solo si la cabecera
# Basic trae ESA clave: es la prueba de que al proveedor le llegó la real. La
# clave va en la ruta porque así funciona httpbin, no porque el invitado la
# necesite: la sonda solo la usa para comprobar que no aparece en lo que ve.
step "7. Proxy de credenciales"
CR="e2e-cred-$$"
PASS="e2e-$(python3 -c 'import secrets; print(secrets.token_hex(12))')"
# La sonda corre DENTRO del invitado, como root. Recibe la clave real solo para
# comprobar que no la ve. Imprime una línea por comprobación.
SONDA='
import base64, json, socket, subprocess, sys, time, urllib.request, urllib.error
real = sys.argv[1]
subprocess.run(["ip", "route", "add", "169.254.169.254/32", "dev", "eth0"], capture_output=True)
t = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/latest/api/token", method="PUT",
    headers={"X-metadata-token-ttl-seconds": "60"}), timeout=4).read().decode()
store = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/",
    headers={"X-metadata-token": t, "Accept": "application/json"}), timeout=4).read().decode()
ph = json.loads(store)["env"]["E2E_KEY"]
print("MMDS", "CLAVE" if real in store else "MARCADOR")
print("PH", ph)
def get(url, h):
    try:
        r = urllib.request.urlopen(urllib.request.Request(url, headers=h), timeout=15); return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, type(e).__name__
b64 = lambda s: base64.b64encode(s.encode()).decode()
print("AUTH", get("http://httpbin.org/basic-auth/demo/" + real, {"Authorization": "Basic " + b64("demo:" + ph)})[0])
if len(sys.argv) > 2 and sys.argv[2] == "corto":
    sys.exit(0)
st, body = get("http://httpbin.org/anything", {"Authorization": "Bearer " + ph})
print("ECO", "CLAVE" if real in body else "MARCADOR")
# /anything devuelve la cabecera Basic tal cual: el base64 lleva la clave real dentro.
st, body = get("http://httpbin.org/anything", {"Authorization": "Basic " + b64("demo:" + ph)})
print("ECOBASIC", "CLAVE" if (real in body or b64("demo:" + real) in body) else ("MARCADOR" if b64("demo:" + ph) in body else "NADA"))
t0 = time.time(); st, why = get("https://httpbin.org/get", {})
print("HTTPS", st, why, int((time.time() - t0) * 1000))
s = socket.create_connection((socket.gethostbyname("httpbin.org"), 80), timeout=5)
s.sendall(b"GET / HTTP/1.1\r\nHost: evil.example.com\r\nConnection: close\r\n\r\n")
print("OTRO", s.recv(20).decode().split(" ")[1])
'
if $KLING run -image "$IMGVOL" -name "$CR" -egress allowlist -allow example.org -allow-exec -ttl 10m -on-ttl remove >/dev/null 2>&1; then
  out=$(printf '%s' "$PASS" | $KLING machine credential "$CR" -domain httpbin.org -env E2E_KEY 2>&1)
  contiene "$out" "placeholder" && ok "credential: la clave queda en el proxy" || bad "machine credential" "placeholder" "$out"

  out=$($KLING exec -timeout 90s "$CR" -- python3 -c "$SONDA" "$PASS" 2>&1)
  ph1=$(printf '%s\n' "$out" | awk '/^PH /{print $2}')
  contiene "$out" "MMDS MARCADOR" && ok "el invitado no ve la clave, solo el marcador" || bad "MMDS" "MMDS MARCADOR" "$out"
  contiene "$out" "AUTH 200" && ok "el proveedor recibe la clave real (httpbin basic-auth 200)" || bad "sustitución" "AUTH 200" "$out"
  contiene "$out" "ECO MARCADOR" && ok "el eco de la respuesta llega redactado" || bad "redacción" "ECO MARCADOR" "$out"
  contiene "$out" "ECOBASIC MARCADOR" && ok "el eco de un Basic (clave dentro del base64) también llega redactado" \
    || bad "redacción de Basic" "ECOBASIC MARCADOR" "$out"
  https=$(printf '%s\n' "$out" | awk '/^HTTPS /{print $2, $4}')
  case "$https" in
    "0 "*) ms=${https#0 }
           [ "$ms" -lt 2000 ] && ok "HTTPS directo al dominio, saltándose el proxy: rechazado en ${ms} ms" \
             || bad "salto del proxy" "un rechazo rápido (RST)" "bloqueado, pero tardó ${ms} ms" ;;
    *) bad "salto del proxy" "HTTPS 0" "$out" ;;
  esac
  contiene "$out" "OTRO 403" && ok "el proxy no sirve para otro dominio (403)" || bad "otro dominio" "OTRO 403" "$out"

  # El marcador NO es un secreto: la máquina se congela, y al despertar el
  # daemon le devuelve sus credenciales (proxy, resolver y MMDS).
  out=$($KLING freeze "$CR" 2>&1)
  contiene "$out" "frozen" && ok "una máquina con credenciales SÍ se congela (solo lleva marcadores)" \
    || bad "freeze con credenciales" "frozen" "$out"
  $KLING thaw "$CR" >/dev/null 2>&1
  out=$($KLING exec -timeout 90s "$CR" -- python3 -c "$SONDA" "$PASS" corto 2>&1)
  ph2=$(printf '%s\n' "$out" | awk '/^PH /{print $2}')
  contiene "$out" "AUTH 200" && [ "$ph1" = "$ph2" ] && ok "tras freeze/thaw la credencial sigue viva y el marcador es el mismo" \
    || bad "credencial tras thaw" "AUTH 200 con el mismo marcador" "$out"

  # Y a un reinicio del daemon: las credenciales viven cifradas en el disco del
  # host, no solo en su memoria.
  if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
    ssh "${KLING_HOST#ssh://}" 'sudo systemctl restart kling' >/dev/null 2>&1
    sleep 4
    out=$($KLING exec -timeout 90s "$CR" -- python3 -c "$SONDA" "$PASS" corto 2>&1)
    contiene "$out" "AUTH 200" && ok "tras reiniciar el daemon la credencial sigue viva" \
      || bad "credencial tras reiniciar el daemon" "AUTH 200" "$out"
  fi

  # Rotación: la misma -env con otra clave conserva el marcador (el proceso del
  # invitado ya lo tiene en su entorno) y el proveedor ve la clave nueva.
  PASS2="e2e-$(python3 -c 'import secrets; print(secrets.token_hex(12))')"
  printf '%s' "$PASS2" | $KLING machine credential "$CR" -domain httpbin.org -env E2E_KEY >/dev/null 2>&1
  out=$($KLING exec -timeout 90s "$CR" -- python3 -c "$SONDA" "$PASS2" corto 2>&1)
  ph3=$(printf '%s\n' "$out" | awk '/^PH /{print $2}')
  contiene "$out" "AUTH 200" && [ "$ph1" = "$ph3" ] && ok "rotar la clave: el proveedor acepta la nueva y el marcador no cambia" \
    || bad "rotación" "AUTH 200 con el mismo marcador" "$out"

  # Permisos por método y ruta (-allow-request): solo lo permitido lleva la
  # clave; lo demás es 403 del proxy, sin salir. La sonda usa http.client, que
  # manda la ruta tal cual (urllib tampoco la limpia, pero así queda explícito):
  # un /../ no debe servir para salirse de lo permitido. Y un stream largo
  # (/drip: 5 bytes en 150 s, uno cada 30) llega entero: antes el plazo TOTAL
  # de 120 s lo cortaba; ahora el plazo es de inactividad.
  SONDA_ALLOW='
import base64, http.client, json, sys, time, urllib.request
real = sys.argv[1]
t = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/latest/api/token", method="PUT",
    headers={"X-metadata-token-ttl-seconds": "60"}), timeout=4).read().decode()
ph = json.loads(urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/",
    headers={"X-metadata-token": t, "Accept": "application/json"}), timeout=4).read().decode())["env"]["E2E_KEY"]
basic = "Basic " + base64.b64encode(("demo:" + ph).encode()).decode()
def pedir(metodo, ruta, timeout=20):
    c = http.client.HTTPConnection("httpbin.org", 80, timeout=timeout)
    try:
        c.request(metodo, ruta, headers={"Authorization": basic})
        r = c.getresponse(); b = r.read()
        return r.status, b
    except Exception as e:
        return 0, type(e).__name__.encode()
    finally:
        c.close()
print("PERMITIDO", pedir("GET", "/basic-auth/demo/" + real)[0])
print("OTRARUTA", pedir("GET", "/anything")[0])
print("OTROMETODO", pedir("POST", "/basic-auth/demo/" + real)[0])
print("PUNTOPUNTO", pedir("GET", "/basic-auth/demo/../../anything")[0])
t0 = time.time(); st, b = pedir("GET", "/drip?duration=150&numbytes=5&delay=0&code=200", timeout=200)
print("DRIP", st, len(b), int(time.time() - t0))
'
  out=$(printf '%s' "$PASS2" | $KLING machine credential "$CR" -domain httpbin.org -env E2E_KEY \
    -allow-request 'GET /basic-auth/demo/*' -allow-request 'GET /drip' 2>&1)
  contiene "$out" "only these requests" && ok "credential -allow-request: la clave queda restringida" \
    || bad "credential -allow-request" "only these requests" "$out"
  out=$($KLING exec -timeout 240s "$CR" -- python3 -c "$SONDA_ALLOW" "$PASS2" 2>&1)
  contiene "$out" "PERMITIDO 200" && ok "Allow: la ruta permitida lleva la clave (basic-auth 200)" \
    || bad "Allow permitido" "PERMITIDO 200" "$out"
  contiene "$out" "OTRARUTA 403" && contiene "$out" "OTROMETODO 403" && ok "Allow: otra ruta u otro método, 403 del proxy" \
    || bad "Allow denegado" "OTRARUTA 403 y OTROMETODO 403" "$out"
  contiene "$out" "PUNTOPUNTO 403" && ok "Allow: /../ no sirve para salirse de lo permitido" \
    || bad "Allow con /../" "PUNTOPUNTO 403" "$out"
  drip=$(printf '%s\n' "$out" | awk '/^DRIP /{print $2, $3, $4}')
  case "$drip" in
    "200 5 "*) ok "un stream de ${drip##* } s (más que el plazo total de antes, 120 s) llega entero" ;;
    *) bad "stream largo por el proxy" "DRIP 200 5 ~150" "$out" ;;
  esac

  # Registro de auditoría (kling machine audit): una línea por petición que
  # pasó por el proxy, también las denegadas, con la ruta enmascarada y la
  # credencial usada; nunca la clave, el marcador ni la query. Todo lo de
  # arriba pasó por él, antes y después del freeze/thaw y del reinicio del
  # daemon: las líneas de la primera sonda (/anything) tienen que seguir ahí.
  out=$($KLING machine audit "$CR" -tail 0 -json 2>&1)
  contiene "$out" '"path":"/anything"' && contiene "$out" '"creds":["E2E_KEY"]' \
    && ok "audit: las peticiones de antes del freeze/thaw siguen, con la credencial usada" \
    || bad "audit tras freeze/thaw" '"path":"/anything" con "creds":["E2E_KEY"]' "$out"
  contiene "$out" '"reason":"not_allowed","denied":true' && contiene "$out" '"reason":"no_credential","denied":true' \
    && ok "audit: las denegaciones (-allow-request y otro dominio) quedan registradas" \
    || bad "audit de denegaciones" "not_allowed y no_credential con denied" "$out"
  contiene "$out" '"path":"/basic-auth/demo/:cred"' && contiene "$out" '"query":true' \
    && ok "audit: la clave en la ruta sale como :cred y de la query solo consta que la había" \
    || bad "audit enmascarado" '/basic-auth/demo/:cred y "query":true' "$out"
  if contiene "$out" "$PASS" || contiene "$out" "$PASS2" || contiene "$out" "kling-cred-" \
    || { [ -n "$ph1" ] && contiene "$out" "$ph1"; } || contiene "$out" "duration="; then
    bad "audit sin secretos" "ni clave, ni marcador, ni query" "$out"
  else
    ok "audit: ni la clave, ni el marcador, ni el contenido de la query"
  fi
  out=$($KLING machine audit "$CR" -denied -json -tail 0 2>&1)
  n_todas=$(printf '%s\n' "$out" | grep -c '"kind":"http"')
  n_den=$(printf '%s\n' "$out" | grep -c '"denied":true')
  [ "$n_todas" -gt 0 ] && [ "$n_todas" = "$n_den" ] && ok "audit -denied: solo denegadas ($n_den)" \
    || bad "audit -denied" "todas las líneas http con denied" "$n_todas líneas, $n_den denegadas"
  out=$($KLING machine audit "$CR" -tail 3 2>&1)
  contiene "$out" "METHOD" && contiene "$out" "httpbin.org" && ok "audit: la tabla" || bad "audit tabla" "cabecera y filas" "$out"
  # -f: sigue el registro como `kling logs -f` sigue la consola.
  seg=$(mktemp)
  $KLING machine audit "$CR" -f -tail 1 >"$seg" 2>&1 &
  segpid=$!
  sleep 2
  $KLING exec -timeout 30s "$CR" -- python3 -c '
import http.client
c = http.client.HTTPConnection("httpbin.org", 80, timeout=10)
c.request("GET", "/anything/e2e-follow"); print(c.getresponse().status)' >/dev/null 2>&1
  sleep 3
  kill "$segpid" 2>/dev/null; wait "$segpid" 2>/dev/null
  out=$(cat "$seg"); rm -f "$seg"
  contiene "$out" "/anything/e2e-follow" && contiene "$out" "DENIED(not_allowed)" \
    && ok "audit -f: la petición nueva aparece mientras se sigue" || bad "audit -f" "/anything/e2e-follow DENIED(not_allowed)" "$out"
  $KLING rm -f "$CR" >/dev/null 2>&1
else
  bad "run -egress allowlist" "una máquina" "no arrancó"
fi
if $KLING run -image "$IMGVOL" -name "$CR-none" -ttl 5m -on-ttl remove >/dev/null 2>&1; then
  out=$(printf 'x' | $KLING machine credential "$CR-none" -domain httpbin.org -env E2E_KEY 2>&1)
  contiene "$out" "allowlist" && ok "sin -egress allowlist, credential se niega y lo explica" \
    || bad "credential sin allowlist" "un rechazo que menciona allowlist" "$out"
  $KLING rm -f "$CR-none" >/dev/null 2>&1
fi

# 7b. Credenciales de PLANTILLA: el caso MCP. El gateway instancia réplicas del
# dorado sin nadie delante; la clave va atada a la plantilla y el daemon se la
# entrega a cada instancia al nacer (marcador propio por instancia).
step "7b. Credenciales de plantilla (lo que usa el gateway MCP)"
TC="e2e-tcred-$$"
TPLC="e2e-tpl-cred-$$"
PASS3="e2e-$(python3 -c 'import secrets; print(secrets.token_hex(12))')"
if $KLING run -image "$IMGVOL" -name "$TC" -egress allowlist -allow example.org -allow-exec >/dev/null 2>&1 \
   && $KLING save "$TC" "$TPLC" >/dev/null 2>&1; then
  out=$(printf '%s' "$PASS3" | $KLING template credential "$TPLC" -domain httpbin.org -env E2E_KEY 2>&1)
  contiene "$out" "every new instance" && ok "template credential: la clave queda atada a la plantilla" \
    || bad "template credential" "every new instance" "$out"
  out=$($KLING template inspect "$TPLC" -json 2>&1)
  contiene "$out" '"credential_domains"' && contiene "$out" "httpbin.org" && ok "la plantilla lista sus dominios con credencial" \
    || bad "template inspect" "credential_domains con httpbin.org" "$out"
  # A2: sin -egress explícito, el CLI manda vacío con -from (egressForRun en
  # cmd/kling/main.go) y el daemon hereda el allowlist de la plantilla —antes
  # el CLI mandaba siempre "none" por defecto y esto exigía repetir -egress
  # allowlist -allow a mano en cada instancia.
  if $KLING run -from "$TPLC" -name "$TC-noegress" -ttl 5m -on-ttl remove >/dev/null 2>&1; then
    out=$($KLING exec -timeout 90s "$TC-noegress" -- python3 -c "$SONDA" "$PASS3" corto 2>&1)
    contiene "$out" "AUTH 200" && ok "run -from sin -egress hereda el allowlist de la plantilla (AUTH 200)" \
      || bad "herencia de egress sin -egress" "AUTH 200" "$out"
    $KLING rm -f "$TC-noegress" >/dev/null 2>&1
  else
    bad "run -from sin -egress" "una máquina con el allowlist heredado" "no arrancó"
  fi
  # Con -egress explícito, en cambio, ese valor manda —como lo hace el
  # gateway, que siempre lo da— y el daemon no hereda nada.
  if $KLING run -from "$TPLC" -name "$TC-a" -egress allowlist -allow example.org -ttl 10m -on-ttl remove >/dev/null 2>&1; then
    out=$($KLING exec -timeout 90s "$TC-a" -- python3 -c "$SONDA" "$PASS3" corto 2>&1)
    pha=$(printf '%s\n' "$out" | awk '/^PH /{print $2}')
    contiene "$out" "MMDS MARCADOR" && contiene "$out" "AUTH 200" && ok "una instancia nace con la credencial de su plantilla (AUTH 200)" \
      || bad "instancia de plantilla con credencial" "MMDS MARCADOR y AUTH 200" "$out"
    if $KLING run -from "$TPLC" -name "$TC-b" -egress allowlist -allow example.org -ttl 10m -on-ttl remove >/dev/null 2>&1; then
      out=$($KLING exec -timeout 90s "$TC-b" -- python3 -c "$SONDA" "$PASS3" corto 2>&1)
      phb=$(printf '%s\n' "$out" | awk '/^PH /{print $2}')
      contiene "$out" "AUTH 200" && [ -n "$pha" ] && [ "$pha" != "$phb" ] && ok "la segunda instancia también, con un marcador distinto" \
        || bad "segunda instancia" "AUTH 200 con otro marcador" "$out"
      $KLING rm -f "$TC-b" >/dev/null 2>&1
    fi
    $KLING rm -f "$TC-a" >/dev/null 2>&1
  else
    bad "run -from plantilla con credenciales" "una máquina" "no arrancó"
  fi
  out=$($KLING run -from "$TPLC" -name "$TC-c" -egress internet -ttl 5m -on-ttl remove 2>&1)
  contiene "$out" "allowlist" && ok "run -from con -egress internet se niega: la plantilla tiene credenciales" \
    || bad "run -from -egress internet" "un rechazo que menciona allowlist" "$out"
  $KLING rm -f "$TC-c" >/dev/null 2>&1
  $KLING template credential "$TPLC" -clear >/dev/null 2>&1
  out=$($KLING template inspect "$TPLC" -json 2>&1)
  contiene "$out" '"credential_domains"' && bad "template credential -clear" "sin credential_domains" "$out" \
    || ok "template credential -clear las quita"
  $KLING rm -f "$TC" >/dev/null 2>&1
  $KLING template rm -f "$TPLC" >/dev/null 2>&1
else
  bad "plantilla con allowlist" "run + save" "falló"
fi

# ── 7d. proxy de credenciales de Postgres ────────────────────────────────────
# El mismo modelo con una base de datos: el invitado conecta en claro al
# dominio del servidor (que su resolver contesta con el proxy) con el marcador
# como contraseña, y el proxy entra en el servidor con la clave real por TLS
# verificado (SCRAM). Necesita un PostgreSQL con TLS, con IP PÚBLICA (el proxy
# no sale a la red privada) y un certificado válido para su nombre:
#
#   KLING_E2E_PG_URL=postgres://rol:clave@db.ejemplo.com:5432/base
#   KLING_E2E_PG_CA=/ruta/ca.pem     (opcional: si el certificado no es de una CA pública)
#
# La clave va en la URL por comodidad del que prueba; al daemon llega por stdin.
step "7d. Proxy de credenciales de Postgres"
if [ -z "${KLING_E2E_PG_URL:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_PG_URL no está (postgres://rol:clave@host:puerto/base): sin servidor que probar\n"
else
  PGC="e2e-pgcred-$$"
  read -r PG_HOST PG_PORT PG_USER PG_DB < <(python3 -c '
import sys, urllib.parse
u = urllib.parse.urlsplit(sys.argv[1])
print(u.hostname, u.port or 5432, urllib.parse.unquote(u.username or ""), (u.path or "/").lstrip("/") or urllib.parse.unquote(u.username or ""))
' "$KLING_E2E_PG_URL")
  PG_PASS=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.unquote(urllib.parse.urlsplit(sys.argv[1]).password or ""))' "$KLING_E2E_PG_URL")
  ca_args=()
  [ -n "${KLING_E2E_PG_CA:-}" ] && ca_args=(-ca-file "$KLING_E2E_PG_CA")
  # La sonda es un cliente mínimo del protocolo v3 (la imagen no trae psql):
  # SSLRequest (el proxy contesta N), arranque, contraseña en claro (el
  # marcador), una consulta; y lo mismo con un marcador falso.
  SONDA_PG='
import json, socket, struct, sys, urllib.request
host, port, user, db, real = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]
t = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/latest/api/token", method="PUT",
    headers={"X-metadata-token-ttl-seconds": "60"}), timeout=4).read().decode()
store = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/",
    headers={"X-metadata-token": t, "Accept": "application/json"}), timeout=4).read().decode()
ph = json.loads(store)["env"]["PGPASSWORD"]
print("MMDS", "CLAVE" if real in store else "MARCADOR")
def leer(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c: raise EOFError
        b += c
    return b
def msg(s):
    h = leer(s, 5); return h[:1], leer(s, struct.unpack("!I", h[1:])[0] - 4)
def conectar(pw):
    s = socket.create_connection((host, port), timeout=20)
    s.sendall(struct.pack("!II", 8, 80877103))
    ssl = leer(s, 1).decode()
    p = b"".join(k.encode() + b"\0" + v.encode() + b"\0" for k, v in (("user", user), ("database", db))) + b"\0"
    s.sendall(struct.pack("!II", 8 + len(p), 196608) + p)
    t, b = msg(s)
    if t != b"R" or b[:4] != b"\0\0\0\3": return s, ssl, "SINPASS"
    s.sendall(b"p" + struct.pack("!I", 5 + len(pw)) + pw.encode() + b"\0")
    while True:
        t, b = msg(s)
        if t == b"E":
            code = [f[1:].decode() for f in b.split(b"\0") if f[:1] == b"C"]
            return s, ssl, "ERROR " + (code[0] if code else "?")
        if t == b"Z": return s, ssl, "LISTO"
s, ssl, r = conectar(ph)
print("SSL", ssl)
print("LOGIN", r)
if r == "LISTO":
    q = b"SELECT current_user\0"
    s.sendall(b"Q" + struct.pack("!I", 4 + len(q)) + q)
    fila = ""
    while True:
        t, b = msg(s)
        if t == b"D": fila = b[6:].decode(errors="replace")
        if t in (b"Z", b"E"): break
    print("FILA", fila)
s.close()
print("FALSO", conectar("kling-cred-00000000000000000000")[2])
'
  if $KLING run -image "$IMGVOL" -name "$PGC" -egress allowlist -allow example.org -allow-exec -ttl 10m -on-ttl remove >/dev/null 2>&1; then
    out=$(printf '%s' "$PG_PASS" | $KLING machine credential "$PGC" -type postgres -domain "$PG_HOST" -port "$PG_PORT" \
      -user "$PG_USER" -database "$PG_DB" "${ca_args[@]}" -env PGPASSWORD 2>&1)
    contiene "$out" "verified TLS" && ok "credential -type postgres: la clave queda en el proxy" \
      || bad "machine credential -type postgres" "verified TLS" "$out"
    out=$($KLING exec -timeout 90s "$PGC" -- python3 -c "$SONDA_PG" "$PG_HOST" "$PG_PORT" "$PG_USER" "$PG_DB" "$PG_PASS" 2>&1)
    contiene "$out" "MMDS MARCADOR" && ok "el invitado no ve la clave, solo el marcador" || bad "MMDS (postgres)" "MMDS MARCADOR" "$out"
    contiene "$out" "SSL N" && ok "el tramo del invitado va en claro (SSLRequest -> N)" || bad "SSLRequest" "SSL N" "$out"
    contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA $PG_USER" \
      && ok "con el marcador el invitado entra y consulta como $PG_USER" || bad "login por el proxy" "LOGIN LISTO y FILA $PG_USER" "$out"
    contiene "$out" "FALSO ERROR 28P01" && ok "un marcador falso: 28P01 sin llegar al servidor" \
      || bad "marcador falso" "FALSO ERROR 28P01" "$out"
    out=$($KLING machine audit "$PGC" -tail 0 -json 2>&1)
    contiene "$out" '"kind":"postgres"' && contiene "$out" '"auth":"scram-sha-256' \
      && ok "audit: una línea por conexión, autenticada con SCRAM" || bad "audit postgres" 'kind postgres con auth scram' "$out"
    contiene "$out" '"reason":"bad_placeholder","denied":true' && ok "audit: el marcador falso queda como denegado" \
      || bad "audit postgres denegado" "bad_placeholder denied" "$out"
    if contiene "$out" "$PG_PASS" || contiene "$out" "kling-cred-" || contiene "$out" "current_user"; then
      bad "audit postgres sin secretos" "ni clave, ni marcador, ni SQL" "$out"
    else
      ok "audit: ni la clave, ni el marcador, ni el SQL"
    fi
    $KLING rm -f "$PGC" >/dev/null 2>&1
  else
    bad "run -egress allowlist (postgres)" "una máquina" "no arrancó"
  fi
fi

# ── 7c. IPv6 cerrado ──────────────────────────────────────────────────────────
# B1: defensa en profundidad, aunque el diagnóstico de A1 no encontrara fuga hoy
# en este lab (net.ipv6.conf.all.forwarding=0 en el host ya cortaba el paso
# antes de llegar a ip6tables, que está vacío). La prueba que vale es DESDE
# DENTRO del invitado, por la misma razón que la nota de la sección 3 de
# SECURITY.md: un ping desde el netns del host no cruza tap0 y no dice nada de
# lo que ve la microVM. Se comprueba en los tres modos de egress porque
# applyIPv6Barrier se aplica en los tres.
step "7c. IPv6 cerrado (defensa en profundidad, los tres modos)"
SONDA_V6='
import subprocess, sys
# Sin dirección global ni de enlace: ipv6.disable=1 en el kernel del invitado
# (arranque en frío) quita el módulo entero, y si algún día el invitado
# arrancara SIN ese parámetro (un snapshot dorado congelado antes de este
# cambio), la barrera del namespace en el host debe seguir cerrando el paso.
addrs = subprocess.run(["ip", "-6", "addr", "show"], capture_output=True, text=True).stdout
print("ADDRS", "vacio" if "inet6" not in addrs else "CON_IPV6:" + addrs.replace(chr(10), " "))
ruta = subprocess.run(["ip", "-6", "route", "show", "default"], capture_output=True, text=True).stdout.strip()
print("RUTA", "vacia" if not ruta else "CON_RUTA:" + ruta)
r = subprocess.run(["ping", "-6", "-c", "1", "-W", "2", "2606:4700:4700::1111"], capture_output=True, text=True)
print("PING6", "bloqueado" if r.returncode != 0 else "PASO:" + r.stdout.replace(chr(10), " "))
'
for modo in none internet allowlist; do
  V6="e2e-v6-$modo-$$"
  extra=""
  [ "$modo" = "allowlist" ] && extra="-allow example.org"
  if $KLING run -image "$IMGVOL" -name "$V6" -egress "$modo" $extra -allow-exec -ttl 5m -on-ttl remove >/dev/null 2>&1; then
    out=$($KLING exec -timeout 60s "$V6" -- python3 -c "$SONDA_V6" 2>&1)
    contiene "$out" "ADDRS vacio" && ok "egress=$modo: el invitado no tiene ninguna dirección IPv6" \
      || bad "IPv6 addrs ($modo)" "ADDRS vacio" "$out"
    contiene "$out" "RUTA vacia" && ok "egress=$modo: sin ruta v6 por defecto" \
      || bad "IPv6 ruta ($modo)" "RUTA vacia" "$out"
    contiene "$out" "PING6 bloqueado" && ok "egress=$modo: ping6 a un destino público no sale" \
      || bad "IPv6 ping ($modo)" "PING6 bloqueado" "$out"
    $KLING rm -f "$V6" >/dev/null 2>&1
  else
    bad "run -egress $modo (sonda IPv6)" "una máquina" "no arrancó"
  fi
done

# ── resumen ──────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fallo(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
