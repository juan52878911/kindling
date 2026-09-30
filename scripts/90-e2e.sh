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

# rechazo_clave dice si la salida de psql es un rechazo de la CLAVE (28P01, del
# servidor o del proxy de credenciales). Las pruebas de "esta clave no entra" se
# daban por buenas con cualquier fallo —host inalcanzable, puerto cerrado, psql
# roto—, y así no podían fallar: ahora solo vale un rechazo de verdad.
rechazo_clave() { contiene "$1" "password authentication failed" || contiene "$1" "refused the credential"; }

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

# El host del daemon: por ssh, o este mismo si el daemon es local (socket unix).
# Las comprobaciones que miran ficheros del daemon usan SU raíz (la que dice
# status -v; un daemon privado no vive en /var/lib/kindling) y sudo solo si no
# se es root allí. Con la raíz y sudo fijos, en un host sin sudo o con otra
# raíz "no existe" salía verde (el registro borrado, la ruta vieja vacía)
# aunque no se hubiera mirado nada.
SSHT=""
if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then SSHT="${KLING_HOST#ssh://}"; fi
# hostsh corre algo en el host del daemon: por ssh, o aquí si es local.
hostsh() { if [ -n "$SSHT" ]; then ssh "$SSHT" "$1"; else sh -c "$1"; fi; }
# HOSTFS: se puede mirar el disco del daemon (por ssh, o local con socket unix).
HOSTFS=0
{ [ -n "$SSHT" ] || [[ "${KLING_HOST:-}" == unix://* ]]; } && HOSTFS=1
KROOT=$(printf '%s\n' "$info" | awk '$1 == "root:" {print $2; exit}')
KROOT="${KROOT:-/var/lib/kindling}"
HSUDO="sudo"
[ "$HOSTFS" = 1 ] && [ "$(hostsh 'id -u' 2>/dev/null)" = 0 ] && HSUDO=""

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
if [ "$HOSTFS" = 1 ]; then
  feats=$(hostsh "$HSUDO dumpe2fs -h $KROOT/volumes/$VOL.ext4 2>/dev/null | grep -i '^Filesystem features'")
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
if [ "$HOSTFS" = 1 ]; then
  st=$(hostsh "$HSUDO dumpe2fs -h $KROOT/volumes/$VOL.ext4 2>/dev/null | grep -i '^Filesystem state'")
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
  if [ "$HOSTFS" = 1 ]; then
    perms=$(hostsh "$HSUDO stat -c '%a %U' $KROOT/volumes/snapshots $KROOT/volumes/snapshots/$VOL3 $KROOT/volumes/snapshots/$VOL3/antes.ext4 | tr '\n' ' '")
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
  # El registro vive fuera del directorio de la máquina (que es del VMM), en
  # <root>/audit, 0700 de root; rm lo borra (#79).
  crid=$($KLING inspect "$CR" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
  # "no" solo si test contesta de verdad (sale 1); un sudo o un test que no
  # corren daban "no" y la comprobación no podía fallar.
  out=$(hostsh "$HSUDO stat -c '%a %U' $KROOT/audit $KROOT/audit/$crid.jsonl 2>&1 | tr '\n' ' '")
  vieja=$(hostsh "$HSUDO test -e $KROOT/machines/$crid/credaudit.jsonl; echo \$?")
  case "$vieja" in 0) vieja=si;; 1) vieja=no;; *) vieja="no pude mirarlo ($vieja)";; esac
  [ "$out" = "700 root 600 root " ] && [ "$vieja" = no ] \
    && ok "audit: en <root>/audit (0700 y 0600, de root), no en el directorio del VMM" \
    || bad "sitio del registro" "700 root 600 root y nada en machines/<id>" "$out vieja=$vieja"
  $KLING rm -f "$CR" >/dev/null 2>&1
  [ "$(hostsh "$HSUDO test -e $KROOT/audit/$crid.jsonl; echo \$?")" = 1 ] \
    && ok "audit: rm borra el registro de la máquina" || bad "audit tras rm" "borrado" "sigue"
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
# verificado (SCRAM). Sin upstream necesita un PostgreSQL con TLS, con IP
# PÚBLICA (sin upstream el proxy no sale a la red privada) y un certificado
# válido para su nombre:
#
#   KLING_E2E_PG_URL=postgres://rol:clave@db.ejemplo.com:5432/base
#   KLING_E2E_PG_CA=/ruta/ca.pem     (opcional: si el certificado no es de una CA pública)
#
# Con un upstream fijado (docs/postgres.md) vale una base de datos del propio
# host o de la LAN; el host de la URL es entonces solo el nombre que usa el
# invitado (cualquier nombre exacto, p. ej. pg.kindling.test):
#
#   KLING_E2E_PG_UPSTREAM=127.0.0.1:55432     (-upstream: a dónde marca el proxy)
#   KLING_E2E_PG_TLS=disable                  (opcional, -upstream-tls disable: sin TLS, solo SCRAM-SHA-256)
#   KLING_E2E_PG_SERVERNAME=pg.lan            (opcional, -tls-server-name: el nombre del certificado)
#
# Un Docker sin TLS en el host del daemon:
#   docker run -d --name pge2e -p 127.0.0.1:55432:5432 -e POSTGRES_USER=kling \
#     -e POSTGRES_PASSWORD=clave-e2e -e POSTGRES_HOST_AUTH_METHOD=scram-sha-256 postgres:17
#   KLING_E2E_PG_URL=postgres://kling:clave-e2e@pg.kindling.test:5432/kling \
#   KLING_E2E_PG_UPSTREAM=127.0.0.1:55432 KLING_E2E_PG_TLS=disable ./scripts/90-e2e.sh
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
  # Upstream fijado, modo TLS y nombre del certificado: lo que la CLI dice del
  # destino y el método que debe quedar en la auditoría dependen de ellos.
  PG_MODO="over verified TLS"; PG_AUTH='"auth":"scram-sha-256'
  if [ -n "${KLING_E2E_PG_SERVERNAME:-}" ]; then
    ca_args+=(-tls-server-name "$KLING_E2E_PG_SERVERNAME"); PG_MODO="over TLS verified as"
  fi
  if [ -n "${KLING_E2E_PG_UPSTREAM:-}" ]; then
    ca_args+=(-upstream "$KLING_E2E_PG_UPSTREAM")
  fi
  if [ -n "${KLING_E2E_PG_TLS:-}" ]; then
    ca_args+=(-upstream-tls "$KLING_E2E_PG_TLS")
    [ "$KLING_E2E_PG_TLS" = disable ] && { PG_MODO="without TLS"; PG_AUTH='"auth":"scram-sha-256"'; }
  fi
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
    contiene "$out" "$PG_MODO" && ok "credential -type postgres: la clave queda en el proxy ($PG_MODO)" \
      || bad "machine credential -type postgres" "$PG_MODO" "$out"
    # -database es obligatoria (o -any-database): sin ella el CLI rechaza antes de leer la clave
    out=$(printf '%s' "$PG_PASS" | $KLING machine credential "$PGC" -type postgres -domain "$PG_HOST" -port "$PG_PORT" \
      -user "$PG_USER" -env PGPASSWORD2 2>&1) && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "needs -database"; } && ok "credential -type postgres sin -database: rechazada" \
      || bad "credential -type postgres sin -database" "error 'needs -database'" "$out"
    out=$($KLING exec -timeout 90s "$PGC" -- python3 -c "$SONDA_PG" "$PG_HOST" "$PG_PORT" "$PG_USER" "$PG_DB" "$PG_PASS" 2>&1)
    contiene "$out" "MMDS MARCADOR" && ok "el invitado no ve la clave, solo el marcador" || bad "MMDS (postgres)" "MMDS MARCADOR" "$out"
    contiene "$out" "SSL N" && ok "el tramo del invitado va en claro (SSLRequest -> N)" || bad "SSLRequest" "SSL N" "$out"
    contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA $PG_USER" \
      && ok "con el marcador el invitado entra y consulta como $PG_USER" || bad "login por el proxy" "LOGIN LISTO y FILA $PG_USER" "$out"
    contiene "$out" "FALSO ERROR 28P01" && ok "un marcador falso: 28P01 sin llegar al servidor" \
      || bad "marcador falso" "FALSO ERROR 28P01" "$out"
    out=$($KLING machine audit "$PGC" -tail 0 -json 2>&1)
    contiene "$out" '"kind":"postgres"' && contiene "$out" "$PG_AUTH" \
      && ok "audit: una línea por conexión, autenticada con SCRAM" || bad "audit postgres" "kind postgres con $PG_AUTH" "$out"
    if [ -n "${KLING_E2E_PG_UPSTREAM:-}" ]; then
      contiene "$out" '"upstream":"' && ok "audit: la línea dice a qué upstream marcó el proxy" \
        || bad "audit postgres upstream" '"upstream":"…"' "$out"
    fi
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
import errno, os, socket
# Todo en Python y leyendo /proc, sin ip ni ping: antes un `ip` o un `ping` que
# fallara por cualquier otra razón (no instalado, sin permiso, otra sintaxis)
# daba "vacío" y "bloqueado", y la prueba no podía fallar.
#
# Sin dirección ni ruta: ipv6.disable=1 en el kernel del invitado (arranque en
# frío) quita el módulo entero, y entonces /proc/net/if_inet6 no existe. Si
# algún día el invitado arrancara SIN ese parámetro (un snapshot dorado
# congelado antes de este cambio), la barrera del namespace en el host debe
# seguir cerrando el paso: lo dice la conexión.
def leer(p):
    try:
        with open(p) as f:
            return f.read()
    except FileNotFoundError:
        return ""
addrs = leer("/proc/net/if_inet6").strip()
print("ADDRS", "vacio" if not addrs else "CON_IPV6:" + addrs.replace(chr(10), " "))
defecto = [l for l in leer("/proc/net/ipv6_route").splitlines()
           if l.split()[:2] == ["0" * 32, "00"] and l.split()[-1] != "lo"]
print("RUTA", "vacia" if not defecto else "CON_RUTA:" + " | ".join(defecto))
# Una conexión TCP de verdad a un destino público. Solo cuentan como bloqueo los
# errores de red; cualquier otra cosa (un RST, una excepción rara) no.
BLOQUEO = {errno.EAFNOSUPPORT, errno.ENETUNREACH, errno.EHOSTUNREACH, errno.EADDRNOTAVAIL, errno.EACCES, errno.EPERM}
def conectar(fam, dst):
    try:
        s = socket.socket(fam, socket.SOCK_STREAM)
    except OSError as e:
        return "bloqueado:" + errno.errorcode.get(e.errno, "?") if e.errno in BLOQUEO else "ERROR:" + repr(e)
    s.settimeout(4)
    try:
        s.connect(dst)
        return "PASO"
    except socket.timeout:
        return "bloqueado:timeout"
    except OSError as e:
        return "bloqueado:" + errno.errorcode.get(e.errno, "?") if e.errno in BLOQUEO else "ERROR:" + repr(e)
    finally:
        s.close()
print("TCP6", conectar(socket.AF_INET6, ("2606:4700:4700::1111", 443)))
# El control: la misma conexión por IPv4. Con egress=internet TIENE que pasar;
# si no, la sonda no distingue un bloqueo de una red rota y lo anterior no vale.
print("TCP4", conectar(socket.AF_INET, ("1.1.1.1", 443)))
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
    contiene "$out" "TCP6 bloqueado" && ok "egress=$modo: una conexión IPv6 a un destino público no sale" \
      || bad "IPv6 conexión ($modo)" "TCP6 bloqueado:<errno de red>" "$out"
    if [ "$modo" = internet ]; then
      contiene "$out" "TCP4 PASO" && ok "egress=internet: la misma conexión por IPv4 sí sale (la sonda distingue)" \
        || bad "control IPv4 (internet)" "TCP4 PASO" "$out"
    else
      contiene "$out" "TCP4 bloqueado" && ok "egress=$modo: tampoco sale por IPv4" \
        || bad "control IPv4 ($modo)" "TCP4 bloqueado" "$out"
    fi
    $KLING rm -f "$V6" >/dev/null 2>&1
  else
    bad "run -egress $modo (sonda IPv6)" "una máquina" "no arrancó"
  fi
done

# ── 7e. kling db ─────────────────────────────────────────────────────────────
# Bases Postgres desechables (ext/db). Necesita la plantilla dorada con Postgres
# (kling db golden build ... pg) y el plugin kling-db instalado. Sin
# KLING_E2E_DB_GOLDEN se salta, y lo dice.
#
#   KLING_E2E_DB_GOLDEN=pg ./scripts/90-e2e.sh
#   KLING_E2E_DB_GOLDEN_PASSWORD=...   (opcional) la clave de la plantilla: con ella se
#                                      prueba que NO entra en una copia
#
# Las claves de las copias se guardan en un KLING_DB_STATE propio de la prueba.
# Toda la salida de kling db se acumula en un fichero y al final se busca en él
# cada clave (la de cada copia y la de la plantilla): tiene que salir 0 veces.
step "7e. kling db"
if [ -z "${KLING_E2E_DB_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_DB_GOLDEN no está (nombre de la plantilla Postgres): sin plantilla que probar\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado (cd ext/db && go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db)\n"
else
  # Sin psql en el host, las pruebas de claves desde el host (las que dicen que
  # la de la plantilla, la vieja tras rotate o la del rol ro NO entran) se
  # saltaban y la sección salía verde sin haberlas hecho.
  need psql
  DBG="$KLING_E2E_DB_GOLDEN"
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  # El estado de la plantilla (su verificador) vive en el directorio real de kling db: sin
  # copiarlo aquí, doctor no puede comprobar que las copias rotaron la clave.
  DBREAL="$HOME/.local/state/kling-db/$KLING_E2E_DB_GOLDEN"
  if [ -d "$DBREAL" ]; then mkdir -p "$KLING_DB_STATE" && chmod 700 "$KLING_DB_STATE" && cp -a "$DBREAL" "$KLING_DB_STATE/"; fi
  DBLOG="$DBTMP/salida.log"; : > "$DBLOG"
  DBU="e2e-db-$$"
  # dbk ejecuta kling db, acumula stdout+stderr en DBLOG y lo devuelve.
  dbk() { local o rc; o=$($KLING db "$@" 2>&1 </dev/null); rc=$?; printf '%s\n' "$o" >> "$DBLOG"; printf '%s\n' "$o"; return $rc; }
  # dbsql corre SQL DENTRO de la copia, por el socket, como superusuario local.
  dbsql() { $KLING exec -timeout 60s "$1" -- su -s /bin/sh postgres -c "psql -X -At -h /run/postgresql appdb -c \"$2\"" 2>&1; }
  # dbpw: la clave de la copia, leída del fichero del host (nunca se imprime).
  dbpw() { local id; id=$($KLING inspect "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'); cat "$KLING_DB_STATE/copies/$id/password" 2>/dev/null; }
  # dbhost: host, puerto, usuario y base de una copia (de connect -dsn, sin la clave).
  dbhost() { $KLING db connect "$1" -dsn 2>/dev/null </dev/null | python3 -c '
import sys, urllib.parse
u = urllib.parse.urlsplit(sys.stdin.read().strip())
print(u.hostname, u.port, urllib.parse.unquote(u.username or ""), u.path.lstrip("/"))'; }
  # dbhostsql: SQL desde el host con una clave dada (por entorno, no por argv).
  dbhostsql() { local pw="$1" h p u d; read -r h p u d < <(dbhost "$2"); PGPASSWORD="$pw" PGSSLMODE=disable PGCONNECT_TIMEOUT=10 psql -X -At -h "$h" -p "$p" -U "$u" -d "$d" -c "$3" 2>&1; }

  out=$(dbk up "$DBG" -name "$DBU")
  contiene "$out" "ready" && ok "kling db up: copia lista" || bad "db up" "ready" "$out"
  out=$(dbsql "$DBU" "SELECT 1")
  [ "$out" = "1" ] && ok "SELECT 1 dentro de la copia, por el socket" || bad "SELECT 1 por socket" "1" "$out"

  # La clave de la copia no es la de la plantilla.
  PW1=$(dbpw "$DBU")
  [ -n "$PW1" ] && ok "la copia tiene su clave en el host" || bad "clave de la copia" "un fichero con la clave" "nada"
  TODAS="$PW1"
  {
    out=$(dbhostsql "$PW1" "$DBU" "SELECT 1")
    [ "$out" = "1" ] && ok "desde el host entra con la clave de la copia" || bad "conexión del host" "1" "$out"
    if [ -n "${KLING_E2E_DB_GOLDEN_PASSWORD:-}" ]; then
      [ "$PW1" != "$KLING_E2E_DB_GOLDEN_PASSWORD" ] && ok "la clave de la copia es distinta de la de la plantilla" \
        || bad "rotación" "clave de la copia distinta de la de la plantilla" "iguales"
      out=$(dbhostsql "$KLING_E2E_DB_GOLDEN_PASSWORD" "$DBU" "SELECT 1")
      rechazo_clave "$out" && ok "la clave de la plantilla NO entra en la copia" \
        || bad "clave de la plantilla" "password authentication failed" "$out"
    else
      printf "  \033[33mskip\033[0m  KLING_E2E_DB_GOLDEN_PASSWORD no está: sin la prueba de la clave de la plantilla desde el host\n"
    fi
    # connect -dsn: el DSN funciona tal cual (su salida no va a DBLOG: lleva la clave a propósito).
    dsn=$($KLING db connect "$DBU" -dsn 2>/dev/null </dev/null)
    # El DSN se descompone en variables PG*: la clave va por entorno, nunca en el argv de psql.
    out=$(eval "$(E2E_DSN="$dsn" python3 -c 'import os, shlex, urllib.parse as u
d = u.urlsplit(os.environ["E2E_DSN"])
for k, v in (("PGHOST", d.hostname), ("PGPORT", d.port), ("PGUSER", u.unquote(d.username or "")),
             ("PGPASSWORD", u.unquote(d.password or "")), ("PGDATABASE", d.path.lstrip("/"))):
    print("export %s=%s" % (k, shlex.quote(str(v or ""))))')"; PGCONNECT_TIMEOUT=10 psql -X -At -c "SELECT 1" 2>&1)
    [ "$out" = "1" ] && ok "connect -dsn: el DSN funciona" || bad "connect -dsn" "1" "$(printf '%s' "$out" | tr -d '\n' | head -c 200)"
    dsn=""
  }

  # fork -n 4: cuatro claves distintas y escrituras aisladas.
  dbsql "$DBU" "CREATE TABLE e2e_marca(v text); INSERT INTO e2e_marca VALUES ('origen')" >/dev/null
  out=$(dbk fork "$DBU" -n 4)
  copias=$(printf '%s\n' "$out" | awk '/  ready  / {print $1}')
  nc=$(printf '%s\n' "$copias" | grep -c . || true)
  [ "$nc" = "4" ] && ok "fork -n 4: cuatro copias listas" || bad "fork -n 4" "4 copias" "$out"
  distintas=1; i=0
  for c in $copias; do
    i=$((i+1))
    pw=$(dbpw "$c")
    case " $TODAS " in *" $pw "*) distintas=0;; esac
    [ -n "$pw" ] || distintas=0
    TODAS="$TODAS $pw"
    dbsql "$c" "INSERT INTO e2e_marca VALUES ('copia-$i')" >/dev/null
  done
  # Sin copias los dos bucles no dan ni una vuelta y "distintas" y "aisladas" se
  # quedaban en 1: el fork fallido salía como aislado. Se exigen las cuatro.
  [ "$nc" = "4" ] || distintas=0
  [ "$distintas" = 1 ] && ok "fork: cuatro claves distintas entre sí y de la del origen" \
    || bad "claves del fork" "todas distintas y no vacías" "alguna repetida o vacía"
  aisladas=1; i=0
  for c in $copias; do
    i=$((i+1))
    filas=$(dbsql "$c" "SELECT string_agg(v, ',' ORDER BY v) FROM e2e_marca")
    [ "$filas" = "copia-$i,origen" ] || { aisladas=0; echo "     $c ve: $filas"; }
  done
  [ "$nc" = "4" ] || aisladas=0
  filas=$(dbsql "$DBU" "SELECT string_agg(v, ',') FROM e2e_marca")
  { [ "$aisladas" = 1 ] && [ "$filas" = "origen" ]; } && ok "cada copia ve solo sus escrituras (y el origen no ve ninguna)" \
    || bad "aislamiento del fork" "copia-N,origen en cada una; origen solo 'origen'" "origen ve: $filas"
  for c in $copias; do dbk rm "$c" >/dev/null 2>&1; done

  # doctor: copia limpia = 0 problemas; con un superusuario de login añadido, >0.
  out=$(dbk doctor "$DBU"); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "; 0 problem(s)"; } && ok "doctor de una copia limpia: 0 problemas" \
    || bad "doctor limpio" "0 problem(s), salida 0" "rc=$rc $(printf '%s' "$out" | tail -3)"
  dbsql "$DBU" "CREATE ROLE e2e_super LOGIN SUPERUSER" >/dev/null
  out=$(dbk doctor "$DBU"); rc=$?
  { [ "$rc" != 0 ] && ! contiene "$out" "; 0 problem(s)"; } && ok "doctor con un superusuario de login añadido: hay problemas" \
    || bad "doctor con superusuario" "problemas > 0" "rc=$rc $(printf '%s' "$out" | tail -3)"
  dbsql "$DBU" "DROP ROLE e2e_super" >/dev/null

  # tenant-check: una política "tipo AuraCRM" (sin inquilino deja ver todo) falla con
  # salida != 0 y nombra la rama IS NULL; corregida, pasa. Nunca imprime valores de inquilino.
  dbsql "$DBU" "CREATE TABLE e2e_tc(id serial PRIMARY KEY, tenant_id text NOT NULL);
    INSERT INTO e2e_tc(tenant_id) VALUES ('e2e-inquilino-uno'), ('e2e-inquilino-uno'), ('e2e-inquilino-dos');
    GRANT SELECT, INSERT, UPDATE ON e2e_tc TO app; GRANT USAGE ON SEQUENCE e2e_tc_id_seq TO app;
    ALTER TABLE e2e_tc ENABLE ROW LEVEL SECURITY;
    CREATE POLICY e2e_tc_p ON e2e_tc USING (current_setting('app.tenant_id', true) IS NULL
      OR current_setting('app.tenant_id', true) = '' OR tenant_id = current_setting('app.tenant_id', true))" >/dev/null
  out=$(dbk tenant-check "$DBU"); rc=$?
  { [ "$rc" != 0 ] && contiene "$out" "fail-open in USING" && contiene "$out" "e2e_tc"; } \
    && ok "tenant-check: la política fail-open falla (salida $rc) y la señala" \
    || bad "tenant-check fail-open" "salida != 0 y 'fail-open in USING'" "rc=$rc $(printf '%s' "$out" | tail -5)"
  contiene "$out" "e2e-inquilino" && bad "tenant-check sin datos" "sin valores de inquilino" "$(printf '%s' "$out" | grep e2e-inquilino | head -2)"
  out=$(dbk tenant-check "$DBU" -json); rc=$?
  { [ "$rc" != 0 ] && contiene "$out" '"pass": false'; } && ok "tenant-check -json: pass false" \
    || bad "tenant-check -json" '"pass": false' "rc=$rc $(printf '%s' "$out" | tail -3)"
  dbsql "$DBU" "DROP POLICY e2e_tc_p ON e2e_tc;
    CREATE POLICY e2e_tc_p ON e2e_tc USING (tenant_id = current_setting('app.tenant_id', true))" >/dev/null
  out=$(dbk tenant-check "$DBU"); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "0 failed check(s), 0 error(s)"; } && ok "tenant-check: con la política corregida pasa" \
    || bad "tenant-check correcta" "salida 0" "rc=$rc $(printf '%s' "$out" | tail -5)"
  n=$(dbsql "$DBU" "SELECT count(*) FROM e2e_tc")
  [ "$n" = 3 ] && ok "tenant-check no cambió los datos (las escrituras se deshacen)" || bad "datos tras tenant-check" 3 "$n"
  dbsql "$DBU" "DROP TABLE e2e_tc" >/dev/null

  # audit: muestra conexiones y no lleva ni la clave ni SQL.
  dbhostsql "$PW1" "$DBU" "SELECT 424242" >/dev/null
  out=$(dbk audit "$DBU" -since 1h)
  contiene "$out" "connect" && ok "audit muestra conexiones" || bad "audit" "eventos de conexión" "$out"
  if contiene "$out" "424242" || contiene "$out" "SELECT" || contiene "$out" "e2e_marca"; then
    bad "audit sin SQL" "ni SQL ni valores" "$out"
  else
    ok "audit no contiene SQL"
  fi

  # reset: los datos vuelven a ser los de la plantilla.
  dbsql "$DBU" "CREATE TABLE e2e_sucia(x int)" >/dev/null
  out=$(dbk reset "$DBU")
  contiene "$out" "ready" || bad "db reset" "ready" "$out"
  out=$(dbsql "$DBU" "SELECT count(*) FROM pg_tables WHERE tablename IN ('e2e_sucia','e2e_marca')")
  [ "$out" = "0" ] && ok "reset devuelve los datos de la plantilla" || bad "reset" "0 tablas de la prueba" "$out"
  PW2=$(dbpw "$DBU")
  { [ -n "$PW2" ] && [ "$PW2" != "$PW1" ]; } && ok "reset: otra copia, otra clave" || bad "clave tras reset" "distinta" "igual o vacía"
  TODAS="$TODAS $PW2 ${KLING_E2E_DB_GOLDEN_PASSWORD:-}"

  # ── kling db: rol de solo lectura, rotate, snapshot/undo, rehearse, golden -template, ask ──
  # dbid: id de la máquina de una copia. dbrpw: la clave de un rol extra (fichero del host, nunca impresa).
  dbid() { $KLING inspect "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'; }
  dbrpw() { cat "$KLING_DB_STATE/copies/$(dbid "$1")/$2.password" 2>/dev/null; }
  # dbrosql: SQL desde el host como el rol $1 con la clave $2 sobre la copia $3 (clave por entorno).
  dbrosql() { local h p u d; read -r h p u d < <(dbhost "$3"); PGPASSWORD="$2" PGSSLMODE=disable PGCONNECT_TIMEOUT=10 psql -X -At -h "$h" -p "$p" -U "$1" -d "$d" -c "$4" 2>&1; }
  APPROLE=$(dbhost "$DBU" | awk '{print $3}')

  # role -ro: el rol lee, y INSERT, DELETE, COPY TO PROGRAM y SET ROLE fallan con él.
  dbsql "$DBU" "CREATE TABLE e2e_ro(v text); INSERT INTO e2e_ro VALUES ('a'),('b'); ALTER TABLE e2e_ro OWNER TO $APPROLE" >/dev/null
  out=$(dbk role "$DBU" -ro -name e2e_agent); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "read-only"; } && ok "role -ro: rol de solo lectura creado" || bad "role -ro" "rol creado" "rc=$rc $out"
  ROPW=$(dbrpw "$DBU" e2e_agent)
  [ -n "$ROPW" ] && TODAS="$TODAS $ROPW"
  if [ -z "$ROPW" ]; then
    bad "clave del rol ro" "un fichero con la clave en el host" "nada"
  else
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "SELECT count(*) FROM e2e_ro")
    [ "$out" = "2" ] && ok "el rol ro lee (SELECT)" || bad "SELECT del rol ro" "2" "$out"
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "INSERT INTO e2e_ro VALUES ('x')")
    contiene "$out" "ERROR" && ok "el rol ro no puede INSERT" || bad "INSERT del rol ro" "ERROR" "$out"
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "DELETE FROM e2e_ro")
    contiene "$out" "ERROR" && ok "el rol ro no puede DELETE" || bad "DELETE del rol ro" "ERROR" "$out"
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "COPY (SELECT 1) TO PROGRAM 'id'")
    contiene "$out" "ERROR" && ok "el rol ro no puede COPY TO PROGRAM" || bad "COPY TO PROGRAM del rol ro" "ERROR" "$out"
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "SET ROLE postgres")
    contiene "$out" "ERROR" && ok "el rol ro no puede SET ROLE" || bad "SET ROLE del rol ro" "ERROR" "$out"
    # Ni siquiera apagando la bandera de solo lectura: el rol no tiene el privilegio.
    out=$(dbrosql e2e_agent "$ROPW" "$DBU" "SET default_transaction_read_only = off; INSERT INTO e2e_ro VALUES ('y')")
    contiene "$out" "ERROR" && ok "el rol ro no escribe ni apagando default_transaction_read_only" || bad "escritura con la bandera apagada" "ERROR" "$out"
    out=$(dbsql "$DBU" "SELECT count(*) FROM e2e_ro")
    [ "$out" = "2" ] && ok "las filas siguen intactas tras los intentos del rol ro" || bad "datos tras el rol ro" "2" "$out"
  fi
  # Un fork no hereda el rol ro: la hija nace sin él, y su clave no entra en la hija.
  out=$(dbk fork "$DBU" -n 1); hija=$(printf '%s\n' "$out" | awk '/  ready  / {print $1}' | head -1)
  if [ -z "$hija" ]; then
    bad "fork con rol ro" "una copia hija lista" "$out"
  else
    out=$(dbsql "$hija" "SELECT count(*) FROM pg_roles WHERE shobj_description(oid, 'pg_authid') = 'kling-db:ro'")
    [ "$out" = "0" ] && ok "fork: la hija no hereda los roles de kling db role" || bad "roles ro en la hija" "0" "$out"
    if [ -n "$ROPW" ]; then
      # En la hija el rol no existe y el fork le quita su línea de pg_hba.conf:
      # el servidor la rechaza antes de mirar la clave ("no pg_hba.conf entry"
      # para ese usuario). También es un rechazo del servidor, no una red rota.
      out=$(dbrosql e2e_agent "$ROPW" "$hija" "SELECT 1")
      { rechazo_clave "$out" || { contiene "$out" "no pg_hba.conf entry for host" && contiene "$out" 'user "e2e_agent"'; }; } \
        && ok "fork: la clave del rol ro del origen no entra en la hija" \
        || bad "clave del rol ro en la hija" "password authentication failed o no pg_hba.conf entry para e2e_agent" "$out"
    fi
    TODAS="$TODAS $(dbpw "$hija")"
    dbk rm "$hija" >/dev/null 2>&1
  fi
  dbk role "$DBU" -ro -name e2e_agent -rm >/dev/null 2>&1
  out=$(dbsql "$DBU" "SELECT count(*) FROM pg_roles WHERE rolname = 'e2e_agent'")
  [ "$out" = "0" ] && ok "role -rm: el rol desaparece" || bad "role -rm" "0" "$out"

  # rotate: la clave vieja deja de valer y la nueva entra.
  {
    ROT_OLD=$(dbpw "$DBU")
    out=$(dbk rotate "$DBU"); rc=$?
    ROT_NEW=$(dbpw "$DBU")
    TODAS="$TODAS $ROT_OLD $ROT_NEW"
    { [ "$rc" = 0 ] && [ -n "$ROT_NEW" ] && [ "$ROT_NEW" != "$ROT_OLD" ]; } && ok "rotate: clave nueva distinta de la vieja" || bad "rotate" "clave nueva distinta" "rc=$rc $out"
    out=$(dbhostsql "$ROT_OLD" "$DBU" "SELECT 1")
    rechazo_clave "$out" && ok "rotate: la clave vieja ya no entra" \
      || bad "clave vieja tras rotate" "password authentication failed" "$out"
    out=$(dbhostsql "$ROT_NEW" "$DBU" "SELECT 1")
    [ "$out" = "1" ] && ok "rotate: la clave nueva entra" || bad "clave nueva tras rotate" "1" "$out"
  }

  # snapshot + undo: los datos vuelven a los del punto.
  dbsql "$DBU" "CREATE TABLE e2e_snap(v text); INSERT INTO e2e_snap VALUES ('punto')" >/dev/null
  out=$(dbk snapshot "$DBU" e2e-punto); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "snapshot of"; } && ok "snapshot: punto de restauración creado" || bad "snapshot" "creado" "rc=$rc $out"
  dbsql "$DBU" "INSERT INTO e2e_snap VALUES ('despues'); CREATE TABLE e2e_tras(x int)" >/dev/null
  out=$(dbk snapshots "$DBU")
  contiene "$out" "e2e-punto" && ok "snapshots: lista el punto" || bad "snapshots" "e2e-punto" "$out"
  out=$(dbk undo "$DBU" e2e-punto); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "ready"; } && ok "undo: copia lista" || bad "undo" "ready" "rc=$rc $out"
  out=$(dbsql "$DBU" "SELECT string_agg(v, ',') FROM e2e_snap")
  [ "$out" = "punto" ] && ok "undo: vuelven los datos del punto" || bad "datos tras undo" "punto" "$out"
  out=$(dbsql "$DBU" "SELECT count(*) FROM pg_tables WHERE tablename = 'e2e_tras'")
  [ "$out" = "0" ] && ok "undo: lo posterior al punto ya no está" || bad "undo" "sin e2e_tras" "$out"
  TODAS="$TODAS $(dbpw "$DBU")"
  dbk snapshot -rm "$DBU" e2e-punto >/dev/null 2>&1
  # Tras undo, la copia nació del punto y kling no deja borrarlo con ella viva:
  # se recoge por nombre al final de la sección (ver dbsnap_limpiar).

  # rehearse: una migración que añade una columna, y otra que se bloquea (lock_timeout).
  # El origen no se toca: el ensayo va en una copia desechable.
  dbsql "$DBU" "CREATE TABLE e2e_rh(id int); INSERT INTO e2e_rh VALUES (1); ALTER TABLE e2e_rh OWNER TO $APPROLE" >/dev/null
  RHDIR=$(mktemp -d "$DBTMP/rh.XXXXXX"); RHBLK=$(mktemp -d "$DBTMP/rhb.XXXXXX")
  printf 'ALTER TABLE e2e_rh ADD COLUMN extra text;\n' > "$RHDIR/001_add_col.sql"
  out=$(dbk rehearse "$DBU" -migrations "$RHDIR"); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" "001_add_col.sql" && contiene "$out" ": OK"; } && ok "rehearse: la migración que añade una columna pasa" || bad "rehearse ok" "OK, salida 0" "rc=$rc $(printf '%s' "$out" | tail -4)"
  out=$(dbsql "$DBU" "SELECT count(*) FROM information_schema.columns WHERE table_name = 'e2e_rh' AND column_name = 'extra'")
  [ "$out" = "0" ] && ok "rehearse no toca el origen" || bad "rehearse" "origen sin la columna" "$out"
  # Otra sesión retiene el lock (fuera de la migración, en segundo plano) y la migración lo pide.
  cat > "$RHBLK/001_bloqueo.sql" <<'SQL'
\! sh -c 'psql -X -q -d appdb -c "LOCK TABLE e2e_rh IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(20)" >/dev/null 2>&1 &'
\! sleep 1
ALTER TABLE e2e_rh ADD COLUMN otra int;
SQL
  out=$(dbk rehearse "$DBU" -migrations "$RHBLK" -lock-timeout 1s); rc=$?
  { [ "$rc" != 0 ] && contiene "$out" "would block" && contiene "$out" ": FAILED"; } && ok "rehearse: la migración que se bloquea falla por lock_timeout (would block)" || bad "rehearse bloqueo" "FAILED con would block, salida != 0" "rc=$rc $(printf '%s' "$out" | tail -4)"
  rm -rf "$RHDIR" "$RHBLK"

  # golden -template crm-demo: se construye y se consulta. kling db no busca
  # db-golden.sh en el directorio actual (a propósito); fuera de una
  # instalación con el script al lado del plugin, se le da el de este checkout.
  export KLING_DB_GOLDEN_SCRIPT="${KLING_DB_GOLDEN_SCRIPT:-$(cd "$(dirname "$0")" && pwd)/db-golden.sh}"
  GTN="e2e-crm-$$"
  out=$(dbk golden build -template crm-demo "$GTN"); rc=$?
  if [ "$rc" != 0 ]; then
    bad "golden build -template crm-demo" "plantilla construida" "rc=$rc $(printf '%s' "$out" | tail -4)"
  else
    ok "golden build -template crm-demo: plantilla construida"
    out=$(dbk up "$GTN" -name "$GTN-c")
    contiene "$out" "ready" && ok "up de la golden crm-demo: lista" || bad "up crm-demo" "ready" "$out"
    out=$(dbsql "$GTN-c" "SELECT count(*) > 0 FROM customers")
    [ "$out" = "t" ] && ok "crm-demo: hay clientes que consultar" || bad "consulta crm-demo" "t" "$out"
    TODAS="$TODAS $(dbpw "$GTN-c")"
    dbk rm "$GTN-c" >/dev/null 2>&1
  fi
  $KLING template rm "$GTN" >/dev/null 2>&1

  # ask con el "modelo" de pruebas (KLING_DB_ASK_FAKE: la respuesta sale de un fichero, sin red).
  # No salta ningún control: la SQL pasa por sqlguard, el rol de solo lectura y READ ONLY.
  ASKF=$(mktemp)
  printf '%s\n' '```sql' 'SELECT count(*) AS n FROM e2e_ro' '```' > "$ASKF"
  out=$(KLING_DB_ASK_FAKE="$ASKF" dbk ask "$DBU" "how many rows does e2e_ro have?" -yes -json); rc=$?
  { [ "$rc" = 0 ] && contiene "$out" '"role": "kling_db_ro"' && contiene "$out" '"2"'; } \
    && ok "ask (modelo de pruebas): responde con el rol de solo lectura" || bad "ask fake" "JSON con el rol y 2" "rc=$rc $(printf '%s' "$out" | tail -4)"
  printf '%s\n' "INSERT INTO e2e_ro VALUES ('z')" > "$ASKF"
  out=$(KLING_DB_ASK_FAKE="$ASKF" dbk ask "$DBU" "add a row" -yes -json); rc=$?
  [ "$rc" != 0 ] && ok "ask: un INSERT del modelo se rechaza" || bad "ask INSERT" "error" "rc=$rc $out"
  printf '%s\n' "SELECT 1; DELETE FROM e2e_ro" > "$ASKF"
  out=$(KLING_DB_ASK_FAKE="$ASKF" dbk ask "$DBU" "delete everything" -yes -json); rc=$?
  [ "$rc" != 0 ] && ok "ask: dos sentencias del modelo se rechazan" || bad "ask 2 sentencias" "error" "rc=$rc $out"
  rm -f "$ASKF"
  out=$(dbsql "$DBU" "SELECT count(*) FROM e2e_ro")
  [ "$out" = "2" ] && ok "ask (modelo de pruebas) no modificó los datos" || bad "datos tras ask fake" "2" "$out"

  # ask con un proveedor real: KLING_E2E_ASK_PROVIDER=opencode|anthropic (o, sin él, con ANTHROPIC_API_KEY).
  ASKP="${KLING_E2E_ASK_PROVIDER:-}"
  [ -z "$ASKP" ] && [ -n "${ANTHROPIC_API_KEY:-}" ] && ASKP=anthropic
  if [ -z "$ASKP" ]; then
    printf "  \033[33mskip\033[0m  ask real: ni KLING_E2E_ASK_PROVIDER ni ANTHROPIC_API_KEY (con opencode instalado: KLING_E2E_ASK_PROVIDER=opencode)\n"
  elif [ "$ASKP" = opencode ] && [ ! -x "$HOME/.opencode/bin/opencode" ] && ! command -v opencode >/dev/null; then
    printf "  \033[33mskip\033[0m  ask real: KLING_E2E_ASK_PROVIDER=opencode pero no hay opencode\n"
  else
    out=$(dbk ask "$DBU" "how many rows does the table e2e_ro have?" -provider "$ASKP" -yes -json); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" '"role"'; } && ok "ask ($ASKP): responde con un rol de solo lectura" || bad "ask $ASKP" "JSON con el rol" "rc=$rc $(printf '%s' "$out" | tail -3)"
    out=$(dbsql "$DBU" "SELECT count(*) FROM e2e_ro")
    [ "$out" = "2" ] && ok "ask ($ASKP) no modificó los datos" || bad "datos tras ask" "2" "$out"
  fi
  dbk rm "$DBU" >/dev/null 2>&1
  # Los puntos de restauración de esta prueba (dbsnap-*-e2e-punto) se quedaban:
  # con la copia ya borrada, se quitan por nombre.
  for t in $($KLING template ls 2>/dev/null | awk '$1 ~ /^dbsnap-.*-e2e-punto$/ {print $1}'); do
    $KLING template rm -f "$t" >/dev/null 2>&1
  done

  # Ninguna clave en ninguna salida de kling db (ni en la de audit, ni en la de doctor).
  fugas=0
  for pw in $TODAS; do
    grep -qF -- "$pw" "$DBLOG" && fugas=$((fugas+1))
  done
  [ "$fugas" = 0 ] && ok "ninguna clave aparece en la salida de kling db (0 coincidencias)" \
    || bad "fuga de claves" 0 "$fugas"
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7e (clone). kling db clone: copia de producción enmascarada ──────────────
# Un golden hecho de una base "de producción" creada aquí mismo, con datos
# personales falsos pero con forma de reales. Comprueba que una columna
# sospechosa sin regla bloquea, que un superusuario y una regla que no cabe
# no dejan golden, y que el golden bueno no contiene ningún valor original
# pero conserva el join por correo, los NULL y lo que no es sospechoso.
# Necesita un Postgres con SCRAM al que lleguen ESTE shell (psql) y el daemon
# (el proxy marca al host:puerto de la URL): córrelo en el host del daemon,
# p. ej. con
#
#   docker run -d --name kpg -p 127.0.0.1:55432:5432 -e POSTGRES_USER=kling \
#     -e POSTGRES_PASSWORD=clave-e2e -e POSTGRES_HOST_AUTH_METHOD=scram-sha-256 postgres:16
#   KLING_E2E_CLONE_ADMIN_URL=postgres://kling:clave-e2e@127.0.0.1:55432/kling ./scripts/90-e2e.sh
#
# La URL de administración (un superusuario) crea y borra la base y el rol de
# solo lectura de la prueba; a kling db clone solo le llega el rol de solo
# lectura, con su clave por PGPASSWORD. Imagen: KLING_E2E_CLONE_IMAGE (pg16).
step "7e (clone). kling db clone"
if [ -z "${KLING_E2E_CLONE_ADMIN_URL:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_CLONE_ADMIN_URL no está (postgres://superusuario:clave@host:puerto/base, con SCRAM): sin origen que clonar\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado\n"
else
  need psql
  CLTMP=$(mktemp -d); export KLING_DB_STATE="$CLTMP/state"
  CLLOG="$CLTMP/salida.log"; : > "$CLLOG"
  CLDB="e2eclone$$"; CLRO="e2eclone_ro$$"; CLG="e2e-clone-$$"; CLC="e2e-clonecp-$$"
  CLPW=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
  CLSCRIPT="$(cd "$(dirname "$0")" && pwd)/db-golden.sh"
  CLIMG="${KLING_E2E_CLONE_IMAGE:-pg16}"
  # La URL de administración se parte en variables: su clave nunca va en argv.
  eval "$(E2E_DSN="$KLING_E2E_CLONE_ADMIN_URL" python3 -c 'import os, shlex, urllib.parse as u
d = u.urlsplit(os.environ["E2E_DSN"])
for k, v in (("CLA_HOST", d.hostname), ("CLA_PORT", d.port or 5432), ("CLA_USER", u.unquote(d.username or "")),
             ("CLA_PW", u.unquote(d.password or "")), ("CLA_DB", d.path.lstrip("/") or u.unquote(d.username or ""))):
    print("%s=%s" % (k, shlex.quote(str(v or ""))))')"
  # adm <base> <sql>: SQL como superusuario del origen.
  adm() { PGHOST="$CLA_HOST" PGPORT="$CLA_PORT" PGUSER="$CLA_USER" PGPASSWORD="$CLA_PW" PGDATABASE="$1" PGSSLMODE=disable \
    PGCONNECT_TIMEOUT=10 psql -X -q -At -v ON_ERROR_STOP=1 -c "$2" 2>&1; }
  # clk <usuario> <clave> <reglas> [flags...]: kling db clone contra la base de la prueba.
  clk() { local u="$1" pw="$2" r="$3"; shift 3
    local o rc; o=$(PGPASSWORD="$pw" $KLING db clone "postgres://$u@$CLA_HOST:$CLA_PORT/$CLDB?sslmode=disable" \
      -mask "$r" -golden "$CLG" -script "$CLSCRIPT" -image "$CLIMG" "$@" 2>&1 </dev/null); rc=$?
    printf '%s\n' "$o" >> "$CLLOG"; printf '%s\n' "$o"; return $rc; }
  # sin_restos: ni golden ni máquina de construcción.
  sin_restos() {
    ! $KLING template inspect "$CLG" -json >/dev/null 2>&1 && ! $KLING ps -a 2>/dev/null | grep -qF -- "$CLG-clone-"
  }
  # Los valores "de producción": ninguno puede aparecer en la salida ni en el golden.
  ORIG="ana.garcia@correo-real.es luis.perez@correo-real.es marta.ruiz@correo-real.es García Pérez Ruiz +34600111222 +34600333444 4111111111111111 5500000000000004"
  out=$(adm "$CLA_DB" "CREATE DATABASE $CLDB" && adm "$CLA_DB" "CREATE ROLE $CLRO LOGIN PASSWORD '$CLPW'" && adm "$CLDB" "
    CREATE TABLE users (id int PRIMARY KEY, email text UNIQUE NOT NULL, full_name text, phone text, tier text);
    CREATE TABLE orders (id int PRIMARY KEY, customer_email text REFERENCES users(email), card text, total numeric);
    INSERT INTO users VALUES (1, 'ana.garcia@correo-real.es', 'Ana García', '+34600111222', 'vip'),
      (2, 'luis.perez@correo-real.es', 'Luis Pérez', '+34600333444', NULL), (3, 'marta.ruiz@correo-real.es', 'Marta Ruiz', NULL, 'x');
    INSERT INTO orders VALUES (10, 'ana.garcia@correo-real.es', '4111111111111111', 10),
      (11, 'ana.garcia@correo-real.es', '4111111111111111', 20), (12, 'luis.perez@correo-real.es', '5500000000000004', 30);
    GRANT USAGE ON SCHEMA public TO $CLRO; GRANT SELECT ON ALL TABLES IN SCHEMA public TO $CLRO;")
  if [ -n "$out" ]; then
    bad "origen de clone" "base y rol creados" "$out"
  else
    printf '%s\n' "users.email: email" "users.full_name: name" "orders.customer_email: email" "orders.card: card" > "$CLTMP/incompletas.yaml"
    cp "$CLTMP/incompletas.yaml" "$CLTMP/reglas.yaml"; echo "users.phone: phone" >> "$CLTMP/reglas.yaml"
    cp "$CLTMP/reglas.yaml" "$CLTMP/rota.yaml"; echo "users.id: email" >> "$CLTMP/rota.yaml"

    out=$(clk "$CLRO" "$CLPW" "$CLTMP/incompletas.yaml"); rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "public.users.phone" && sin_restos; } \
      && ok "clone: una columna sospechosa sin regla bloquea, sin golden ni máquina" || bad "clone bloqueo" "error con users.phone y sin restos" "rc=$rc $(printf '%s' "$out" | tail -3)"
    out=$(clk "$CLA_USER" "$CLA_PW" "$CLTMP/reglas.yaml"); rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "superuser" && sin_restos; } \
      && ok "clone: un superusuario del origen se rechaza" || bad "clone superusuario" "error superuser" "rc=$rc $(printf '%s' "$out" | tail -3)"
    out=$(clk "$CLRO" "$CLPW" "$CLTMP/rota.yaml"); rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "nothing was built" && sin_restos; } \
      && ok "clone: una regla que no cabe (correo en un entero) no deja golden" || bad "clone regla rota" "nothing was built y sin restos" "rc=$rc $(printf '%s' "$out" | tail -3)"

    out=$(clk "$CLRO" "$CLPW" "$CLTMP/reglas.yaml"); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "public.users.email" && contiene "$out" "3 of 3 rows"; } \
      && ok "clone: golden $CLG construido, con el informe" || bad "clone" "rc 0 e informe" "rc=$rc $(printf '%s' "$out" | tail -5)"
    if $KLING ps -a 2>/dev/null | grep -qF -- "$CLG-clone-"; then
      bad "clone: máquina de construcción" "borrada" "sigue"
    else
      ok "clone: la máquina de construcción no queda"
    fi
    out=$($KLING db up "$CLG" -name "$CLC" 2>&1 </dev/null)
    if ! contiene "$out" "ready"; then
      bad "db up del golden enmascarado" "ready" "$out"
    else
      clsql() { $KLING exec -timeout 60s "$CLC" -- su -s /bin/sh postgres -c "psql -X -At -h /run/postgresql appdb -c \"$1\"" 2>&1; }
      out=$(clsql "SELECT count(*) FROM users WHERE email LIKE 'user\_%@example.invalid'")
      [ "$out" = 3 ] && ok "golden: los tres correos enmascarados" || bad "correos enmascarados" 3 "$out"
      out=$(clsql "SELECT count(*) FROM users u JOIN orders o ON o.customer_email = u.email")
      [ "$out" = 3 ] && ok "golden: el join por correo sigue casando (determinista)" || bad "join enmascarado" 3 "$out"
      out=$(clsql "SELECT count(DISTINCT customer_email) || ' ' || count(DISTINCT card) FROM orders")
      [ "$out" = "2 2" ] && ok "golden: mismo valor, mismo enmascarado" || bad "determinismo" "2 2" "$out"
      out=$(clsql "SELECT count(*) FILTER (WHERE phone IS NULL) || ' ' || (SELECT tier FROM users WHERE id = 1) FROM users")
      [ "$out" = "1 vip" ] && ok "golden: los NULL siguen NULL y lo no sospechoso queda" || bad "NULL y columnas sin regla" "1 vip" "$out"
      todo="$(clsql "SELECT string_agg(u::text, ' ') FROM users u") $(clsql "SELECT string_agg(o::text, ' ') FROM orders o")"
      fuga=""
      for v in $ORIG; do contiene "$todo" "$v" && fuga="$fuga $v"; done
      [ -z "$fuga" ] && ok "golden: ningún valor original en las tablas" || bad "golden sin originales" "ninguno" "$fuga"
      $KLING db rm "$CLC" >/dev/null 2>&1 </dev/null
    fi
    fuga=0
    for v in $ORIG "$CLPW" "$CLA_PW"; do grep -qF -- "$v" "$CLLOG" && fuga=$((fuga+1)); done
    [ "$fuga" = 0 ] && ok "clone: ni valores originales ni claves en la salida (0 coincidencias)" \
      || bad "fuga en la salida de clone" 0 "$fuga"
  fi
  $KLING template rm -f "$CLG" >/dev/null 2>&1
  adm "$CLA_DB" "DROP DATABASE IF EXISTS $CLDB" >/dev/null
  adm "$CLA_DB" "DROP ROLE IF EXISTS $CLRO" >/dev/null
  rm -rf "$CLTMP"; unset KLING_DB_STATE
fi

# ── 7f. kling db attach (modelo A) ───────────────────────────────────────────
# Una copia compartida por dos agentes que viven en OTRAS microVMs: cada uno
# conecta por el proxy de credenciales de su máquina con su marcador, y el
# proxy marca a la copia (resuelta por id en cada conexión) con una clave que
# el agente no ve. Congelar la copia corta la sesión viva y el siguiente
# intento falla; tras el thaw vuelve; otro dueño no puede; detach retira el
# acceso. Mismas variables que 7e; sin KLING_E2E_DB_GOLDEN se salta, avisando.
step "7f. kling db attach (modelo A)"
if [ -z "${KLING_E2E_DB_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_DB_GOLDEN no está (nombre de la plantilla Postgres): sin copia que compartir\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado (cd ext/db && go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db)\n"
else
  DBG="$KLING_E2E_DB_GOLDEN"
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  DBLOG="$DBTMP/salida.log"; : > "$DBLOG"
  dbk() { local o rc; o=$($KLING db "$@" 2>&1 </dev/null); rc=$?; printf '%s\n' "$o" >> "$DBLOG"; printf '%s\n' "$o"; return $rc; }
  dbsql() { $KLING exec -timeout 60s "$1" -- su -s /bin/sh postgres -c "psql -X -At -h /run/postgresql appdb -c \"$2\"" 2>&1; }
  dbid() { $KLING inspect "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'; }
  C="e2e-dba-$$"; A1="e2e-ag1-$$"; A2="e2e-ag2-$$"; A3="e2e-ag3-$$"
  # La sonda es un cliente mínimo del protocolo v3 (la imagen no trae psql):
  # lee su marcador de MMDS, entra por el proxy y cuenta las filas de e2e_a.
  # Con "hold" se queda con la sesión abierta y dice si se la cortan.
  SONDA_A='
import json, socket, struct, sys, time, urllib.request
host, env, user, db, modo = sys.argv[1:6]
t = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/latest/api/token", method="PUT",
    headers={"X-metadata-token-ttl-seconds": "60"}), timeout=4).read().decode()
store = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/",
    headers={"X-metadata-token": t, "Accept": "application/json"}), timeout=4).read().decode()
ph = json.loads(store).get("env", {}).get(env, "")
print("MARCADOR", "si" if ph.startswith("kling-cred-") else "NO", flush=True)
def leer(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c: raise EOFError
        b += c
    return b
def msg(s):
    h = leer(s, 5); return h[:1], leer(s, struct.unpack("!I", h[1:])[0] - 4)
def conectar():
    s = socket.create_connection((host, 5432), timeout=20)
    p = b"".join(k.encode() + b"\0" + v.encode() + b"\0" for k, v in (("user", user), ("database", db))) + b"\0"
    s.sendall(struct.pack("!II", 8 + len(p), 196608) + p)
    t, b = msg(s)
    if t == b"E": return s, "ERROR " + "".join(f[1:].decode() for f in b.split(b"\0") if f[:1] == b"C")
    if t != b"R" or b[:4] != b"\0\0\0\3": return s, "SINPASS"
    s.sendall(b"p" + struct.pack("!I", 5 + len(ph)) + ph.encode() + b"\0")
    while True:
        t, b = msg(s)
        if t == b"E": return s, "ERROR " + "".join(f[1:].decode() for f in b.split(b"\0") if f[:1] == b"C")
        if t == b"Z": return s, "LISTO"
try:
    s, r = conectar()
except Exception as e:
    print("LOGIN CAIDA", type(e).__name__, flush=True); sys.exit(0)
print("LOGIN", r, flush=True)
if r != "LISTO": sys.exit(0)
q = b"SELECT count(*) FROM e2e_a\0"
s.sendall(b"Q" + struct.pack("!I", 4 + len(q)) + q)
fila = ""
while True:
    t, b = msg(s)
    if t == b"D": fila = b[6:].decode(errors="replace")
    if t in (b"Z", b"E"): break
print("FILA", fila, flush=True)
if modo == "hold":
    print("HOLD", flush=True)
    s.settimeout(120)
    try:
        print("CORTADA" if not s.recv(1) else "DATOS", flush=True)
    except socket.timeout:
        print("SIGUE", flush=True)
    except Exception as e:
        print("CORTADA", type(e).__name__, flush=True)
'
  # sonda <agente> <host> <env> <user> [hold]
  sonda() { $KLING exec -timeout 150s "$1" -- python3 -c "$SONDA_A" "$2" "$3" "$4" appdb "${5:-once}" 2>&1; }
  agente() { $KLING run -image "$IMGVOL" -name "$1" -egress allowlist -allow example.org -allow-exec \
    -label kind=sandbox -ttl 15m -on-ttl remove "${@:2}" >/dev/null 2>&1; }

  out=$(dbk up "$DBG" -name "$C")
  if ! contiene "$out" "ready"; then
    bad "db up (7f)" "ready" "$out"
  elif ! agente "$A1" || ! agente "$A2" || ! agente "$A3" -label kling.db.owner=otro; then
    bad "agentes (7f)" "tres máquinas con egress allowlist" "alguna no arrancó"
  else
    CID=$(dbid "$C"); APPROLE=app
    dbsql "$C" "CREATE TABLE e2e_a(v text); INSERT INTO e2e_a VALUES ('x'),('y'); ALTER TABLE e2e_a OWNER TO $APPROLE" >/dev/null
    dbk role "$C" -ro -name e2e_ro >/dev/null
    TODAS="$(cat "$KLING_DB_STATE/copies/$CID/password" 2>/dev/null) $(cat "$KLING_DB_STATE/copies/$CID/e2e_ro.password" 2>/dev/null)"

    out=$(dbk attach "$A1" "$C"); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "attached to $C"; } && ok "attach: el agente 1 recibe la copia (rol de la aplicación)" \
      || bad "attach A1" "attached to $C" "rc=$rc $out"
    out=$(dbk attach "$A2" "$C" -role e2e_ro -host shared.db.internal); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "as e2e_ro"; } && ok "attach -role: el agente 2 recibe la copia con el rol de solo lectura" \
      || bad "attach A2 -role" "as e2e_ro" "rc=$rc $out"
    H1="$C.db.internal"

    out=$(sonda "$A1" "$H1" PGPASSWORD "$APPROLE")
    { contiene "$out" "MARCADOR si" && contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA 2"; } \
      && ok "agente 1: entra por el proxy con su marcador y lee (2 filas)" || bad "lectura A1" "MARCADOR si, LOGIN LISTO, FILA 2" "$out"
    out=$(sonda "$A2" shared.db.internal PGPASSWORD e2e_ro)
    { contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA 2"; } \
      && ok "agente 2: la misma copia, con su rol ro" || bad "lectura A2" "LOGIN LISTO, FILA 2" "$out"
    # La copia no se ramifica con el agente: el agente con attach no se puede forkear.
    out=$($KLING sandbox fork "$A1" -n 1 2>&1) && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "proxy credentials"; } && ok "un agente con attach no se ramifica (guardián de fork)" \
      || bad "fork del agente" "rechazo por credenciales del proxy" "rc=$rc $out"

    # Congelar la copia corta la sesión viva del agente 1...
    HOLD="$DBTMP/hold.out"
    sonda "$A1" "$H1" PGPASSWORD "$APPROLE" hold > "$HOLD" 2>&1 &
    HPID=$!
    for _ in $(seq 1 40); do grep -q HOLD "$HOLD" 2>/dev/null && break; sleep 0.5; done
    if grep -q HOLD "$HOLD"; then
      $KLING freeze "$C" >/dev/null 2>&1
      wait "$HPID" 2>/dev/null
      out=$(cat "$HOLD")
      contiene "$out" "CORTADA" && ok "freeze de la copia: la sesión abierta del agente se corta" \
        || bad "sesión viva al congelar" "CORTADA" "$out"
    else
      kill "$HPID" 2>/dev/null; wait "$HPID" 2>/dev/null
      bad "sesión de espera" "HOLD" "$(cat "$HOLD")"
      $KLING freeze "$C" >/dev/null 2>&1
    fi
    # ...y el siguiente intento falla sin llegar a ella.
    out=$(sonda "$A1" "$H1" PGPASSWORD "$APPROLE")
    contiene "$out" "LOGIN ERROR 08006" && ok "con la copia congelada, un nuevo intento falla (08006)" \
      || bad "intento con la copia congelada" "LOGIN ERROR 08006" "$out"
    $KLING thaw "$C" >/dev/null 2>&1
    out=$(sonda "$A1" "$H1" PGPASSWORD "$APPROLE")
    { contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA 2"; } && ok "tras el thaw el agente vuelve a entrar" \
      || bad "tras thaw" "LOGIN LISTO, FILA 2" "$out"

    # Otro dueño no puede: ni con un agente de otro dueño ni pidiéndolo como otro.
    out=$(dbk attach "$A3" "$C") && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "belongs to owner"; } && ok "attach de un agente de otro dueño: rechazado" \
      || bad "attach otro dueño" "belongs to owner" "rc=$rc $out"
    out=$(dbk attach "$A1" "$C" -owner otro -env OTRA) && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "belongs to owner"; } && ok "attach como otro dueño: rechazado" \
      || bad "attach -owner otro" "belongs to owner" "rc=$rc $out"

    # La auditoría del agente dice a qué máquina fue y por qué no llegó, sin claves.
    out=$($KLING machine audit "$A1" -tail 0 -json 2>&1)
    { contiene "$out" "\"upstream\":\"machine:$CID\"" && contiene "$out" '"reason":"machine_unavailable"'; } \
      && ok "audit del agente: upstream machine:<id> y machine_unavailable al congelar" \
      || bad "audit del agente" "machine:$CID y machine_unavailable" "$(printf '%s' "$out" | tail -3)"
    printf '%s\n' "$out" >> "$DBLOG"

    # detach retira el acceso: el marcador ya no vale.
    out=$(dbk detach "$A1" "$C"); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "detached"; } && ok "detach: el agente 1 pierde la copia" || bad "detach" "detached" "rc=$rc $out"
    out=$(sonda "$A1" "$H1" PGPASSWORD "$APPROLE")
    # Sin credencial Postgres el nombre ya no se desvía al proxy (o el proxy
    # cierra sin leer): cualquier cosa menos entrar.
    { contiene "$out" "LOGIN " && ! contiene "$out" "LOGIN LISTO"; } && ok "tras detach el agente ya no entra" \
      || bad "tras detach" "LOGIN ERROR o LOGIN CAIDA" "$out"
    out=$(sonda "$A2" shared.db.internal PGPASSWORD e2e_ro)
    contiene "$out" "LOGIN LISTO" && ok "el agente 2 sigue con su attach" || bad "A2 tras detach de A1" "LOGIN LISTO" "$out"

    fugas=0
    for pw in $TODAS; do grep -qF -- "$pw" "$DBLOG" && fugas=$((fugas+1)); done
    [ "$fugas" = 0 ] && ok "7f: ninguna clave en la salida de kling db ni en la auditoría" || bad "fuga de claves (7f)" 0 "$fugas"
  fi
  $KLING rm -f "$A1" >/dev/null 2>&1; $KLING rm -f "$A2" >/dev/null 2>&1; $KLING rm -f "$A3" >/dev/null 2>&1
  dbk rm "$C" >/dev/null 2>&1
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7g. kling db diff ────────────────────────────────────────────────────────
# Dos copias de la misma plantilla Postgres: iguales, diff dice same; tras
# cambiar una fila, añadir otra y una columna en la segunda, diff lo cuenta
# (esquema y filas) sin que ningún valor de la tabla salga en su salida. Mismas
# variables que 7e; sin KLING_E2E_DB_GOLDEN se salta, avisando.
step "7g. kling db diff"
if [ -z "${KLING_E2E_DB_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_DB_GOLDEN no está (nombre de la plantilla Postgres): sin copias que comparar\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado (cd ext/db && go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db)\n"
else
  DBG="$KLING_E2E_DB_GOLDEN"
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  DBLOG="$DBTMP/salida.log"; : > "$DBLOG"
  dbk() { local o rc; o=$($KLING db "$@" 2>&1 </dev/null); rc=$?; printf '%s\n' "$o" >> "$DBLOG"; printf '%s\n' "$o"; return $rc; }
  dbsql() { $KLING exec -timeout 60s "$1" -- su -s /bin/sh postgres -c "psql -X -At -h /run/postgresql appdb -c \"$2\"" 2>&1; }
  D1="e2e-dd1-$$"; D2="e2e-dd2-$$"
  o1=$(dbk up "$DBG" -name "$D1"); o2=$(dbk up "$DBG" -name "$D2")
  if ! contiene "$o1" "ready" || ! contiene "$o2" "ready"; then
    bad "db up (7g)" "dos copias ready" "$o1 / $o2"
  else
    for c in "$D1" "$D2"; do
      dbsql "$c" "CREATE TABLE e2e_diff(id int PRIMARY KEY, v text);
        INSERT INTO e2e_diff VALUES (1, 'e2e-valor-uno'), (2, 'e2e-valor-dos'), (3, 'e2e-valor-tres')" >/dev/null
    done
    out=$(dbk diff "$D1" "$D2" -json); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" '"same": true'; } && ok "diff de dos copias iguales: same" \
      || bad "diff iguales" '"same": true' "rc=$rc $(printf '%s' "$out" | tail -5)"
    dbsql "$D2" "UPDATE e2e_diff SET v = 'e2e-valor-cambiado' WHERE id = 2;
      INSERT INTO e2e_diff VALUES (4, 'e2e-valor-nuevo'); ALTER TABLE e2e_diff ADD COLUMN extra text" >/dev/null
    out=$(dbk diff "$D1" "$D2"); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "~ public.e2e_diff" && contiene "$out" "+ column extra"; } \
      && ok "diff: la columna nueva sale en el esquema" \
      || bad "diff esquema" "~ public.e2e_diff y + column extra" "rc=$rc $(printf '%s' "$out" | tail -8)"
    contiene "$out" "new 1, deleted 0, changed 1, unchanged 2" \
      && ok "diff: una fila nueva y una cambiada, por huellas" \
      || bad "diff filas" "new 1, deleted 0, changed 1, unchanged 2" "$(printf '%s' "$out" | tail -8)"
    out=$(dbk diff "$D1" "$D2" -schema-only -json); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" '"schema_only": true' && contiene "$out" '"same": false'; } \
      && ok "diff -schema-only -json: distinto, sin filas" || bad "diff -schema-only" 'schema_only true, same false' "rc=$rc"
    out=$(dbk diff "$D1" "$D1"); rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "same copy"; } && ok "diff de una copia consigo misma: rechazado" \
      || bad "diff misma copia" "same copy" "rc=$rc $out"
    # Ni un valor de la tabla en ninguna salida de diff: solo estructura y cuentas.
    if grep -q "e2e-valor" "$DBLOG"; then
      bad "diff sin datos" "ningún valor de e2e_diff en la salida" "$(grep -m2 e2e-valor "$DBLOG")"
    else
      ok "diff no saca ningún valor de las filas"
    fi
  fi
  dbk rm "$D1" >/dev/null 2>&1; dbk rm "$D2" >/dev/null 2>&1
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7g2. kling db branch ────────────────────────────────────────────────────
# Una base por rama de git, con el gancho post-checkout puesto: la rama nueva
# empieza con los datos de su padre (por la copia de reserva si el padre no
# cambió desde que se sacó, por el fork en caliente si cambió, incluido un
# nextval() que no escribe WAL), el .env de .git cambia en cada checkout, las
# demás ramas se congelan en segundo plano y ninguna salida lleva una clave.
# Mismas variables que 7e; sin KLING_E2E_DB_GOLDEN se salta, avisando.
step "7g2. kling db branch"
if [ -z "${KLING_E2E_DB_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_DB_GOLDEN no está (nombre de la plantilla Postgres): sin plantilla de la que sacar ramas\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado (cd ext/db && go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db)\n"
elif ! command -v git >/dev/null; then
  printf "  \033[33mskip\033[0m  sin git no hay ramas\n"
else
  DBG="$KLING_E2E_DB_GOLDEN"
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  BR="$DBTMP/repo"; BLOG="$DBTMP/gancho.log"; : > "$BLOG"
  # El gancho llama a ${KLING:-kling} desde el repo: ruta absoluta.
  KABS=$(command -v "$KLING"); export KLING="$KABS"
  dbsql() { $KLING exec -timeout 60s "$1" -- su -s /bin/sh postgres -c "psql -X -At -h /run/postgresql appdb -c \"$2\"" 2>&1; }
  # bcopy: la copia (o la reserva, con spare) de una rama, por -ls -json.
  bcopy() { (cd "$BR" && $KLING db branch -ls -json 2>/dev/null) | python3 -c '
import sys, json
b, spare = sys.argv[1], len(sys.argv) > 2
for r in json.load(sys.stdin):
    if r["branch"] == b and bool(r.get("spare")) == spare:
        print(r["copy"]); break' "$@"; }
  # bidle: espera a que acabe el trabajo de segundo plano del gancho.
  bidle() { local i; for i in $(seq 1 600); do pgrep -f "branch -settle" >/dev/null || return 0; sleep 0.2; done; }
  # bco: git checkout con la salida del gancho al registro; deja en BMS lo que tardó.
  bco() { local t0 t1; t0=$(date +%s%N); (cd "$BR" && git checkout -q "$@" 2>>"$BLOG"); t1=$(date +%s%N); BMS=$(( (t1 - t0) / 1000000 )); }
  benv() { grep '^PGPASSWORD=' "$BR/.git/kling-db.env" 2>/dev/null; }

  mkdir -p "$BR" && (cd "$BR" && git init -q -b main && git -c user.name=e2e -c user.email=e2e@localhost commit -q --allow-empty -m init)
  out=$(cd "$BR" && $KLING db branch -golden "$DBG" 2>&1); printf '%s\n' "$out" >> "$BLOG"
  BMAIN=$(bcopy main)
  { [ -n "$BMAIN" ] && [ "$(stat -c %a "$BR/.git/kling-db.env" 2>/dev/null)" = 600 ]; } \
    && ok "db branch: copia de main y su conexión en .git (0600)" || bad "db branch main" "copia y .git/kling-db.env 0600" "$out"
  out=$(cd "$BR" && $KLING db branch hook install 2>&1); contiene "$out" "installed" && ok "hook install" || bad "hook install" "installed" "$out"
  dbsql "$BMAIN" "CREATE TABLE e2e_br(v text); INSERT INTO e2e_br VALUES ('de-main')" >/dev/null

  bco -b e2e-a; ENVA=$(benv)
  BA=$(bcopy e2e-a)
  { [ -n "$BA" ] && [ "$(dbsql "$BA" "SELECT v FROM e2e_br")" = "de-main" ]; } \
    && ok "rama nueva (fork en caliente, $BMS ms): empieza con los datos de main" || bad "rama e2e-a" "copia con la fila de main" "copia=$BA"
  bidle
  [ "$($KLING inspect "$BMAIN" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')" = frozen ] \
    && ok "main queda congelada en segundo plano" || bad "main congelada" "frozen" "$($KLING ps | grep "$BMAIN")"
  [ -n "$(bcopy main spare)" ] && ok "al salir de main queda una copia de reserva suya" || bad "reserva de main" "una fila spare en -ls" "$(cd "$BR" && $KLING db branch -ls)"

  dbsql "$BA" "INSERT INTO e2e_br VALUES ('de-a')" >/dev/null
  bco main
  { [ "$BMS" -lt 1000 ] && [ "$(benv)" != "$ENVA" ]; } \
    && ok "volver a main: $BMS ms, y el .env cambia" || bad "volver a main" "< 1000 ms y otro .env" "$BMS ms"
  [ "$(dbsql "$BMAIN" "SELECT count(*) FROM e2e_br")" = 1 ] && ok "main no ve lo escrito en e2e-a" || bad "aislamiento" "1 fila" "$(dbsql "$BMAIN" "SELECT count(*) FROM e2e_br")"
  bidle

  SPARE=$(bcopy main spare)
  bco -b e2e-b
  BB=$(bcopy e2e-b)
  { [ -n "$SPARE" ] && [ "$BB" = "$SPARE" ] && [ "$(dbsql "$BB" "SELECT v FROM e2e_br")" = "de-main" ]; } \
    && ok "rama nueva desde main sin cambios: se queda la reserva ($BMS ms) con los datos de main" \
    || bad "adopción de la reserva" "e2e-b = $SPARE con la fila de main" "e2e-b=$BB"
  bidle; bco main; bidle

  # main cambia (una fila): la reserva ya no vale y la rama sale del fork.
  dbsql "$BMAIN" "INSERT INTO e2e_br VALUES ('otra-de-main')" >/dev/null
  SPARE=$(bcopy main spare)
  bco -b e2e-c
  BC=$(bcopy e2e-c)
  { [ -n "$BC" ] && [ "$BC" != "$SPARE" ] && [ "$(dbsql "$BC" "SELECT count(*) FROM e2e_br")" = 2 ]; } \
    && ok "con main cambiada no se usa la reserva: e2e-c sale del fork con la fila nueva" || bad "reserva caducada" "fork con 2 filas" "e2e-c=$BC reserva=$SPARE"
  bidle; bco main; bidle

  # Solo nextval(): ni xid ni (dentro de su lote) WAL. La huella lo ve igual.
  dbsql "$BMAIN" "CREATE SEQUENCE e2e_sq; SELECT nextval('e2e_sq')" >/dev/null
  bco e2e-a; bidle; bco main; bidle
  dbsql "$BMAIN" "SELECT nextval('e2e_sq')" >/dev/null
  SPARE=$(bcopy main spare)
  bco -b e2e-d
  BD=$(bcopy e2e-d)
  { [ -n "$BD" ] && [ "$BD" != "$SPARE" ] && [ "$(dbsql "$BD" "SELECT last_value FROM e2e_sq")" = 2 ]; } \
    && ok "un nextval() en main también invalida la reserva (la secuencia sigue en 2)" || bad "nextval" "fork con last_value 2" "e2e-d=$BD reserva=$SPARE"
  bidle

  # Ninguna clave en lo que imprimieron el gancho y los comandos.
  leak=""
  for d in "$KLING_DB_STATE"/copies/*/password; do
    [ -f "$d" ] && grep -qF "$(cat "$d")" "$BLOG" && leak="$leak $d"
  done
  [ -z "$leak" ] && ok "ninguna clave en la salida del gancho" || bad "claves en la salida" "ninguna" "$leak"

  (cd "$BR" && git checkout -q main 2>/dev/null); bidle
  for b in e2e-a e2e-b e2e-c e2e-d main; do (cd "$BR" && $KLING db branch -rm "$b" >/dev/null 2>&1); done
  left=$($KLING ps -a 2>/dev/null | grep -c "$(basename "$BMAIN" | cut -c1-9)")
  [ "$left" = 0 ] && ok "-rm de todas las ramas: sin copias ni reservas" || bad "limpieza" "0 copias del repo" "$left"
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7h. MariaDB: plantilla, proxy de credenciales, doctor y audit ────────────
# Con KLING_E2E_MYSQL_GOLDEN (una plantilla MariaDB de scripts/db-golden-mysql.sh,
# construida con kling db golden ... -engine mysql). Una copia; un agente la usa
# por el proxy de MySQL con SOLO el marcador (la clave de la copia la tiene el
# proxy); doctor limpio y con una cuenta anónima; audit con conexiones y sin SQL;
# rm. Ninguna salida lleva la clave.
#
# El proxy no marca a la red de kindling (172.30.0.0/16): se le da -upstream a un
# reenvío en el loopback del host (127.0.0.1:<libre> -> la copia, un relé en
# python3), con -upstream-tls disable (la copia no tiene TLS; mysql_native_password
# no manda la clave). El agente es la imagen $IMGVOL y habla el protocolo con una
# sonda en python3 (sin cliente de MySQL).
#
#   KLING_E2E_MYSQL_GOLDEN=my ./scripts/90-e2e.sh
step "7h. MariaDB (kling db + proxy de MySQL)"
if [ -z "${KLING_E2E_MYSQL_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_MYSQL_GOLDEN no está (nombre de una plantilla MariaDB): sin plantilla que probar\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado (cd ext/db && go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db)\n"
elif ! $KLING template inspect "$KLING_E2E_MYSQL_GOLDEN" >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  la plantilla %s no existe (kling db golden -script scripts/db-golden.sh build -engine mysql %s)\n" \
    "$KLING_E2E_MYSQL_GOLDEN" "$KLING_E2E_MYSQL_GOLDEN"
else
  MYG="$KLING_E2E_MYSQL_GOLDEN"
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  # Como en 7e: el estado de la plantilla (su clave) permite a doctor comprobar la rotación.
  DBREAL="$HOME/.local/state/kling-db/$MYG"
  if [ -d "$DBREAL" ]; then mkdir -p "$KLING_DB_STATE" && chmod 700 "$KLING_DB_STATE" && cp -a "$DBREAL" "$KLING_DB_STATE/"; fi
  DBLOG="$DBTMP/salida.log"; : > "$DBLOG"
  dbk() { local o rc; o=$($KLING db "$@" 2>&1 </dev/null); rc=$?; printf '%s\n' "$o" >> "$DBLOG"; printf '%s\n' "$o"; return $rc; }
  # mysql_sql corre SQL DENTRO de la copia como root por el socket local (unix_socket).
  mysql_sql() { $KLING exec -timeout 60s "$1" -- mariadb -N -B appdb -e "$2" 2>&1; }
  dbid() { $KLING inspect "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'; }
  MYC="e2e-my-$$"; MYA="e2e-myag-$$"; RELAY_PID=""
  out=$(dbk up "$MYG" -name "$MYC")
  if ! contiene "$out" "ready"; then
    bad "db up (MariaDB)" "ready" "$out"
  else
    ok "kling db up de la plantilla MariaDB: copia lista"
    MYPW=$(cat "$KLING_DB_STATE/copies/$(dbid "$MYC")/password" 2>/dev/null)
    TODAS="$MYPW ${KLING_E2E_MYSQL_GOLDEN_PASSWORD:-}"
    [ -n "$MYPW" ] && ok "la copia tiene su clave en el host" || bad "clave de la copia (MariaDB)" "un fichero con la clave" "nada"
    mysql_sql "$MYC" "CREATE TABLE e2e_my(v text); INSERT INTO e2e_my VALUES ('a'),('b'),('c')" >/dev/null
    # connect -dsn: mysql://app:...@IP:3306/appdb. Solo se usan host, puerto, usuario y base.
    read -r MYH MYP MYU MYD < <($KLING db connect "$MYC" -dsn 2>/dev/null </dev/null | python3 -c '
import sys, urllib.parse
u = urllib.parse.urlsplit(sys.stdin.read().strip())
print(u.hostname, u.port or 3306, urllib.parse.unquote(u.username or ""), u.path.lstrip("/"))')
    # El relé: 127.0.0.1:<libre> -> la copia. Escribe el puerto elegido y se queda sirviendo.
    RELAY_PORT_F="$DBTMP/relay.port"
    python3 -c '
import socket, sys, threading
dst = (sys.argv[1], int(sys.argv[2]))
ln = socket.socket(); ln.bind(("127.0.0.1", 0)); ln.listen(16)
open(sys.argv[3], "w").write(str(ln.getsockname()[1]))
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError: pass
    for s in (a, b):
        try: s.shutdown(socket.SHUT_RDWR)
        except OSError: pass
while True:
    c, _ = ln.accept()
    try: u = socket.create_connection(dst, timeout=10)
    except OSError: c.close(); continue
    for x, y in ((c, u), (u, c)): threading.Thread(target=pipe, args=(x, y), daemon=True).start()
' "$MYH" "$MYP" "$RELAY_PORT_F" >/dev/null 2>&1 &
    RELAY_PID=$!
    for _ in $(seq 1 20); do [ -s "$RELAY_PORT_F" ] && break; sleep 0.2; done
    RELAY_PORT=$(cat "$RELAY_PORT_F" 2>/dev/null)
    # La sonda: protocolo de MySQL a mano (saludo v10, HandshakeResponse41 con
    # mysql_native_password sobre el marcador de MMDS, COM_QUERY); y lo mismo con
    # un marcador falso. A la sonda solo le llega el sha256 de la clave (para ver
    # que no está en MMDS), nunca la clave.
    MYPW_SHA=$(printf '%s' "$MYPW" | python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')
    SONDA_MY='
import hashlib, json, socket, struct, sys, urllib.request
host, user, db, real = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]  # real: sha256 de la clave
t = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/latest/api/token", method="PUT",
    headers={"X-metadata-token-ttl-seconds": "60"}), timeout=4).read().decode()
store = urllib.request.urlopen(urllib.request.Request("http://169.254.169.254/",
    headers={"X-metadata-token": t, "Accept": "application/json"}), timeout=4).read().decode()
env = json.loads(store).get("env", {})
ph = env.get("MYSQL_PWD", "")
clave = any(hashlib.sha256(str(v).encode()).hexdigest() == real for v in env.values())
print("MMDS", "CLAVE" if clave else ("MARCADOR" if ph.startswith("kling-cred-") else "NADA"), flush=True)
def leer(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c: raise EOFError
        b += c
    return b
def pkt(s):
    h = leer(s, 4); return h[3], leer(s, h[0] | h[1] << 8 | h[2] << 16)
def enviar(s, seq, p): s.sendall(struct.pack("<I", len(p))[:3] + bytes([seq]) + p)
def nativo(pw, nonce):
    a = hashlib.sha1(pw.encode()).digest(); b = hashlib.sha1(a).digest()
    c = hashlib.sha1(nonce + b).digest(); return bytes(x ^ y for x, y in zip(a, c))
def lenenc(b, i):
    n = b[i]
    if n < 251: return n, i + 1
    k = {252: 2, 253: 3, 254: 8}[n]; return int.from_bytes(b[i + 1:i + 1 + k], "little"), i + 1 + k
def conectar(pw):
    s = socket.create_connection((host, 3306), timeout=20)
    _, g = pkt(s)
    i = g.index(b"\0", 1) + 1 + 4
    nonce = g[i:i + 8]; i += 8 + 1 + 2 + 1 + 2 + 2 + 1 + 10
    nonce += g[i:i + 12]
    caps = 0x1 | 0x8 | 0x200 | 0x2000 | 0x8000 | 0x80000
    auth = nativo(pw, nonce)
    p = struct.pack("<IIB", caps, 1 << 24, 45) + b"\0" * 23 + user.encode() + b"\0" + bytes([len(auth)]) + auth
    p += db.encode() + b"\0" + b"mysql_native_password\0"
    enviar(s, 1, p)
    _, r = pkt(s)
    if r[:1] == b"\0": return s, "LISTO"
    if r[:1] == b"\xff": return s, "ERROR %d" % struct.unpack("<H", r[1:3])[0]
    return s, "RARO %r" % r[:1]
try:
    s, r = conectar(ph)
except Exception as e:
    print("LOGIN CAIDA", type(e).__name__, flush=True); sys.exit(0)
print("LOGIN", r, flush=True)
if r == "LISTO":
    enviar(s, 0, b"\x03SELECT count(*) FROM e2e_my")
    _, r = pkt(s)
    if r[:1] == b"\xff":
        print("FILA ERROR", flush=True)
    else:
        n, _ = lenenc(r, 0)
        for _ in range(n): pkt(s)
        pkt(s)
        _, fila = pkt(s)
        l, i = lenenc(fila, 0)
        print("FILA", fila[i:i + l].decode(), flush=True)
    s.close()
try:
    print("FALSO", conectar("kling-cred-00000000000000000000")[1], flush=True)
except Exception as e:
    print("FALSO CAIDA", type(e).__name__, flush=True)
'
    MYDOM="my-$$.e2e.internal"
    if [ -z "$RELAY_PORT" ]; then
      bad "relé al loopback" "un puerto" "el relé no arrancó"
    elif ! $KLING run -image "$IMGVOL" -name "$MYA" -egress allowlist -allow example.org -allow-exec -ttl 15m -on-ttl remove >/dev/null 2>&1; then
      bad "agente (MariaDB)" "una máquina con egress allowlist" "no arrancó"
    else
      out=$(printf '%s' "$MYPW" | $KLING machine credential "$MYA" -type mysql -domain "$MYDOM" -user "$MYU" -database "$MYD" \
        -upstream "127.0.0.1:$RELAY_PORT" -upstream-tls disable -env MYSQL_PWD 2>&1); rc=$?
      printf '%s\n' "$out" >> "$DBLOG"
      [ "$rc" = 0 ] && ok "credential -type mysql: la clave de la copia queda en el proxy" \
        || bad "machine credential -type mysql" "salida 0" "rc=$rc $out"
      out=$($KLING exec -timeout 90s "$MYA" -- python3 -c "$SONDA_MY" "$MYDOM" "$MYU" "$MYD" "$MYPW_SHA" 2>&1)
      printf '%s\n' "$out" >> "$DBLOG"
      contiene "$out" "MMDS MARCADOR" && ok "el agente solo ve el marcador, no la clave" || bad "MMDS (mysql)" "MMDS MARCADOR" "$out"
      { contiene "$out" "LOGIN LISTO" && contiene "$out" "FILA 3"; } \
        && ok "con el marcador el agente entra por el proxy de MySQL y lee (3 filas)" || bad "login por el proxy de MySQL" "LOGIN LISTO y FILA 3" "$out"
      contiene "$out" "FALSO ERROR 1045" && ok "un marcador falso: 1045 sin llegar a la copia" || bad "marcador falso (mysql)" "FALSO ERROR 1045" "$out"
      out=$($KLING machine audit "$MYA" -tail 0 -json 2>&1)
      printf '%s\n' "$out" >> "$DBLOG"
      { contiene "$out" '"kind":"mysql"' && contiene "$out" '"reason":"bad_placeholder"'; } \
        && ok "machine audit: líneas kind mysql, el marcador falso denegado" || bad "audit del agente (mysql)" "kind mysql y bad_placeholder" "$(printf '%s' "$out" | tail -3)"
      if contiene "$out" "kling-cred-" || contiene "$out" "e2e_my"; then
        bad "audit mysql sin secretos" "ni marcador ni SQL" "$out"
      else
        ok "machine audit: ni el marcador ni el SQL"
      fi
    fi
    $KLING rm -f "$MYA" >/dev/null 2>&1
    [ -n "$RELAY_PID" ] && { kill "$RELAY_PID" 2>/dev/null; wait "$RELAY_PID" 2>/dev/null; }

    # doctor: la copia recién hecha, sin problemas; con una cuenta anónima, MY003.
    out=$(dbk doctor "$MYC"); rc=$?
    { [ "$rc" = 0 ] && contiene "$out" "; 0 problem(s)"; } && ok "doctor de la copia MariaDB: 0 problemas" \
      || bad "doctor MariaDB limpio" "0 problem(s), salida 0" "rc=$rc $(printf '%s' "$out" | tail -4)"
    mysql_sql "$MYC" "CREATE USER ''@'%'" >/dev/null
    out=$(dbk doctor "$MYC"); rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "MY003"; } && ok "doctor con una cuenta anónima: MY003" \
      || bad "doctor MariaDB anónima" "MY003, salida != 0" "rc=$rc $(printf '%s' "$out" | tail -4)"
    mysql_sql "$MYC" "DROP USER ''@'%'" >/dev/null

    # audit (server_audit con CONNECT): conexiones, sin SQL.
    out=$(dbk audit "$MYC" -since 1h)
    contiene "$out" "connect" && ok "audit de la copia MariaDB: conexiones" || bad "audit MariaDB" "eventos connect" "$(printf '%s' "$out" | tail -4)"
    if contiene "$out" "SELECT" || contiene "$out" "e2e_my"; then
      bad "audit MariaDB sin SQL" "ni SQL ni tablas" "$out"
    else
      ok "audit de la copia MariaDB: sin SQL"
    fi
  fi
  out=$(dbk rm "$MYC"); rc=$?
  { [ "$rc" = 0 ] && ! $KLING inspect "$MYC" >/dev/null 2>&1; } && ok "kling db rm: la copia MariaDB ya no está" \
    || bad "db rm (MariaDB)" "la copia borrada" "rc=$rc $out"
  fugas=0
  for pw in $TODAS; do grep -qF -- "$pw" "$DBLOG" && fugas=$((fugas+1)); done
  [ "$fugas" = 0 ] && ok "7h: ninguna clave en la salida de kling db, del agente ni de la auditoría" || bad "fuga de claves (7h)" 0 "$fugas"
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7h2. Redis y SQLite: up, fork, connect, rotate, doctor, rechazos y rm ────
# Con KLING_E2E_REDIS_GOLDEN y/o KLING_E2E_SQLITE_GOLDEN (plantillas de
# scripts/db-golden-redis.sh y db-golden-sqlite.sh, o kling db golden ...
# -engine redis|sqlite). Redis: cada copia estrena su clave (al invitado solo
# su SHA-256) y la del administrador; SQLite: sin clave, la base se abre.
#
#   KLING_E2E_REDIS_GOLDEN=rd KLING_E2E_SQLITE_GOLDEN=sq ./scripts/90-e2e.sh
step "7h2. Redis y SQLite (kling db)"
sha256_de() { if command -v sha256sum >/dev/null 2>&1; then printf '%s' "$1" | sha256sum | cut -d' ' -f1; else printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1; fi; }
if [ -z "${KLING_E2E_REDIS_GOLDEN:-}${KLING_E2E_SQLITE_GOLDEN:-}" ]; then
  printf "  \033[33mskip\033[0m  ni KLING_E2E_REDIS_GOLDEN ni KLING_E2E_SQLITE_GOLDEN: sin plantillas que probar\n"
elif ! $KLING db --help >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el plugin kling-db no está instalado\n"
else
  DBTMP=$(mktemp -d); export KLING_DB_STATE="$DBTMP/state"
  # Como en 7e y 7h: el estado de la plantilla Redis (su clave) permite a doctor
  # comprobar que la copia ya no la usa (RD052).
  DBREAL="$HOME/.local/state/kling-db/${KLING_E2E_REDIS_GOLDEN:-}"
  if [ -n "${KLING_E2E_REDIS_GOLDEN:-}" ] && [ -d "$DBREAL" ]; then
    mkdir -p "$KLING_DB_STATE" && chmod 700 "$KLING_DB_STATE" && cp -a "$DBREAL" "$KLING_DB_STATE/"
  fi
  DBLOG="$DBTMP/salida.log"; : > "$DBLOG"
  dbk() { local o rc; o=$($KLING db "$@" 2>&1 </dev/null); rc=$?; printf '%s\n' "$o" >> "$DBLOG"; printf '%s\n' "$o"; return $rc; }
  dbid() { $KLING inspect "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'; }
  # shellcheck disable=SC2016 # se expande en el invitado
  RDADM='c=$(command -v redis-cli || command -v valkey-cli) && REDISCLI_AUTH=$(cat /etc/kling-db/redis-admin) && export REDISCLI_AUTH && exec "$c" -s /run/redis/redis.sock'
  TODAS=""
  if [ -n "${KLING_E2E_REDIS_GOLDEN:-}" ]; then
    RC1="e2e-rd-$$"
    out=$(dbk up "$KLING_E2E_REDIS_GOLDEN" -name "$RC1")
    if ! contiene "$out" "ready"; then
      bad "db up (Redis)" "ready" "$out"
    else
      ok "kling db up de la plantilla Redis: copia lista"
      RPW=$(cat "$KLING_DB_STATE/copies/$(dbid "$RC1")/password" 2>/dev/null); TODAS="$TODAS $RPW"
      acl=$($KLING exec -i "$RC1" -- sh -c "$RDADM" <<<"ACL GETUSER app" 2>&1)
      contiene "$acl" "$(sha256_de "$RPW")" && ok "el usuario app tiene el SHA-256 de la clave del host" \
        || bad "hash de la copia Redis" "el SHA-256 de la clave del host" "$acl"
      contiene "$acl" "$RPW" && bad "Redis guarda la clave" "solo el hash" "la clave en ACL GETUSER"
      out=$(dbk fork "$RC1" -n 2)
      mapfile -t RFK < <(printf '%s\n' "$out" | awk '/  ready  /{print $1}')
      if [ "${#RFK[@]}" = 2 ]; then
        p1=$(cat "$KLING_DB_STATE/copies/$(dbid "${RFK[0]}")/password" 2>/dev/null)
        p2=$(cat "$KLING_DB_STATE/copies/$(dbid "${RFK[1]}")/password" 2>/dev/null)
        a0=$($KLING exec "$RC1" -- cat /etc/kling-db/redis-admin 2>/dev/null)
        a1=$($KLING exec "${RFK[0]}" -- cat /etc/kling-db/redis-admin 2>/dev/null)
        TODAS="$TODAS $p1 $p2"
        { [ -n "$p1" ] && [ "$p1" != "$p2" ] && [ "$p1" != "$RPW" ] && [ -n "$a1" ] && [ "$a0" != "$a1" ]; } \
          && ok "fork Redis: cada copia con su clave y su administrador" || bad "fork Redis" "claves distintas" "iguales o vacías"
        for c in "${RFK[@]}"; do dbk rm "$c" >/dev/null; done
      else
        bad "fork Redis -n 2" "2 copias listas" "$out"
      fi
      # Fuera de dbk: el DSN lleva la clave y no va al registro de fugas.
      out=$($KLING db connect "$RC1" -dsn 2>&1 </dev/null)
      [ "$out" = "redis://app:$RPW@${out#*@}" ] && ok "connect -dsn: redis:// con la clave del host" \
        || bad "connect -dsn (Redis)" "redis://app:<clave>@..." "(omitido: lleva la clave)"
      out=$(dbk rotate "$RC1"); rc=$?
      RPW2=$(cat "$KLING_DB_STATE/copies/$(dbid "$RC1")/password" 2>/dev/null); TODAS="$TODAS $RPW2"
      acl=$($KLING exec -i "$RC1" -- sh -c "$RDADM" <<<"ACL GETUSER app" 2>&1)
      { [ "$rc" = 0 ] && [ "$RPW2" != "$RPW" ] && contiene "$acl" "$(sha256_de "$RPW2")"; } \
        && ok "rotate Redis: clave nueva en el host y su hash en la copia" || bad "rotate Redis" "hash nuevo" "rc=$rc $out"
      out=$(dbk doctor "$RC1"); rc=$?
      { [ "$rc" = 0 ] && contiene "$out" "; 0 problem(s)"; } && ok "doctor de la copia Redis: 0 problemas" \
        || bad "doctor Redis" "0 problem(s)" "rc=$rc $(printf '%s' "$out" | tail -4)"
      out=$(dbk snapshot "$RC1" s1); rc=$?
      { [ "$rc" != 0 ] && contiene "$out" "postgres copies only"; } && ok "snapshot de una copia Redis: rechazado" \
        || bad "snapshot Redis" "rechazo claro" "rc=$rc $out"
    fi
    out=$(dbk rm "$RC1"); rc=$?
    { [ "$rc" = 0 ] && ! $KLING inspect "$RC1" >/dev/null 2>&1; } && ok "kling db rm: la copia Redis ya no está" \
      || bad "db rm (Redis)" "la copia borrada" "rc=$rc $out"
  fi
  if [ -n "${KLING_E2E_SQLITE_GOLDEN:-}" ]; then
    SC1="e2e-sq-$$"
    out=$(dbk up "$KLING_E2E_SQLITE_GOLDEN" -name "$SC1")
    if ! contiene "$out" "ready"; then
      bad "db up (SQLite)" "ready" "$out"
    else
      ok "kling db up de la plantilla SQLite: copia lista"
      [ -e "$KLING_DB_STATE/copies/$(dbid "$SC1")/password" ] && bad "SQLite sin clave" "ningún fichero de clave" "hay uno"
      SQF=$(printf '%s\n' "$out" | sed -n 's/.*sqlite3 \(\/var\/lib\/kling-db\/[a-z0-9_]*\.sqlite\).*/\1/p' | head -n1)
      $KLING exec -i "$SC1" -- sqlite3 -bail "$SQF" <<<"CREATE TABLE e2e_sq(v TEXT); INSERT INTO e2e_sq VALUES ('a');" >/dev/null 2>&1
      out=$(dbk fork "$SC1" -n 1)
      SFK=$(printf '%s\n' "$out" | awk '/  ready  /{print $1; exit}')
      n=$($KLING exec -i "$SFK" -- sqlite3 -bail -readonly "$SQF" <<<"SELECT count(*) FROM e2e_sq;" 2>&1)
      [ "$n" = 1 ] && ok "fork SQLite: la copia hija trae los datos del origen" || bad "fork SQLite" 1 "$n"
      [ -n "$SFK" ] && dbk rm "$SFK" >/dev/null
      out=$(dbk connect "$SC1")
      contiene "$out" "no password" && ok "connect SQLite: el fichero y cómo entrar" || bad "connect SQLite" "no password" "$out"
      out=$(dbk connect "$SC1" -dsn); rc=$?
      { [ "$rc" != 0 ] && contiene "$out" "-sqlite"; } && ok "connect -dsn de una copia SQLite: rechazado" || bad "connect -dsn SQLite" "rechazo" "rc=$rc $out"
      out=$(dbk doctor "$SC1"); rc=$?
      { [ "$rc" = 0 ] && contiene "$out" "; 0 problem(s)"; } && ok "doctor de la copia SQLite: 0 problemas" \
        || bad "doctor SQLite" "0 problem(s)" "rc=$rc $(printf '%s' "$out" | tail -4)"
    fi
    out=$(dbk rm "$SC1"); rc=$?
    { [ "$rc" = 0 ] && ! $KLING inspect "$SC1" >/dev/null 2>&1; } && ok "kling db rm: la copia SQLite ya no está" \
      || bad "db rm (SQLite)" "la copia borrada" "rc=$rc $out"
  fi
  fugas=0
  for pw in $TODAS; do grep -qF -- "$pw" "$DBLOG" && fugas=$((fugas+1)); done
  [ "$fugas" = 0 ] && ok "7h2: ninguna clave en la salida de kling db" || bad "fuga de claves (7h2)" 0 "$fugas"
  rm -rf "$DBTMP"; unset KLING_DB_STATE
fi

# ── 7i. autorización por inquilino (authz) ───────────────────────────────────
# Solo con KLING_E2E_AUTHZ=1: necesita un daemon arrancado con una política
# (docs/authz.md) que dé DOS tokens de inquilino, y los tokens en claro:
#
#   /etc/kling/authz.json (root, 0644):
#     {"rules": [], "tokens": [
#       {"sha256": "<sha256sum del token A>", "role": "tenant:e2ea"},
#       {"sha256": "<sha256sum del token B>", "role": "tenant:e2eb"}]}
#   KLING_E2E_AUTHZ=1 KLING_E2E_AUTHZ_TOKEN_A=<token A> KLING_E2E_AUTHZ_TOKEN_B=<token B> \
#     ./scripts/90-e2e.sh
#
# Quien corre el e2e sigue siendo admin sin token (root o el usuario del daemon,
# o una regla admin): el resto de secciones no cambia, y la limpieza la hace él.
# Los inquilinos se llaman e2ea y e2eb salvo KLING_E2E_AUTHZ_TENANT_A/_B.
step "7i. autorización por inquilino"
if [ "${KLING_E2E_AUTHZ:-0}" != 1 ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_AUTHZ no es 1 (hace falta un daemon con política y dos tokens de inquilino; ver la cabecera de 7i)\n"
elif [ -z "${KLING_E2E_AUTHZ_TOKEN_A:-}" ] || [ -z "${KLING_E2E_AUTHZ_TOKEN_B:-}" ]; then
  printf "  \033[33mskip\033[0m  KLING_E2E_AUTHZ=1 pero faltan KLING_E2E_AUTHZ_TOKEN_A o KLING_E2E_AUTHZ_TOKEN_B\n"
else
  TA="${KLING_E2E_AUTHZ_TENANT_A:-e2ea}"; TB="${KLING_E2E_AUTHZ_TENANT_B:-e2eb}"
  ka() { KLING_AUTHZ_TOKEN="$KLING_E2E_AUTHZ_TOKEN_A" $KLING "$@" 2>&1 </dev/null; }
  kb() { KLING_AUTHZ_TOKEN="$KLING_E2E_AUTHZ_TOKEN_B" $KLING "$@" 2>&1 </dev/null; }
  AZM="e2e-authz-$$"; AZS="e2e-authz-snap-$$"
  out=$(ka info)
  contiene "$out" "you are tenant:$TA" && ok "info con el token A: tenant:$TA" || bad "info con token A" "you are tenant:$TA" "$out"
  out=$(KLING_AUTHZ_TOKEN="no-es-un-token" $KLING ps 2>&1 </dev/null) && rc=0 || rc=$?
  { [ "$rc" != 0 ] && contiene "$out" "not valid"; } && ok "un token inválido: rechazado" || bad "token inválido" "error 'not valid'" "rc=$rc $out"
  out=$(ka run -name "$AZM" -image min -ttl 10m -on-ttl remove)
  if ! contiene "$out" "booted"; then
    bad "run como $TA" "una máquina" "$out"
  else
    out=$($KLING inspect "$AZM" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("labels", {}).get("kling.owner", ""))')
    [ "$out" = "$TA" ] && ok "run como inquilino: el daemon sella kling.owner=$TA" \
      || bad "kling.owner sellado" "$TA" "$out"
    # rc 0 además de no verla: un ps que falla tampoco la enseña, y eso no
    # prueba nada.
    out=$(kb ps -a) && rc=0 || rc=$?
    { [ "$rc" = 0 ] && ! contiene "$out" "$AZM"; } && ok "$TB no ve la máquina de $TA en ps" \
      || bad "ps de $TB" "salida 0, sin la máquina de $TA" "rc=$rc $out"
    out=$(kb inspect "$AZM") && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "doesn't exist"; } && ok "$TB: inspect de la máquina de $TA responde como si no existiera" \
      || bad "inspect ajeno" "doesn't exist" "rc=$rc $out"
    out=$(kb rm -f "$AZM") && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "doesn't exist" && $KLING inspect "$AZM" >/dev/null 2>&1; } \
      && ok "$TB no puede borrar la máquina de $TA (y se le dice que no existe)" \
      || bad "rm ajeno" "un error 'doesn't exist' y la máquina sigue" "rc=$rc $out"
    out=$(kb run -name "$AZM-b" -image min -label "kling.owner=$TA") && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "set by the daemon"; } && ok "$TB no puede ponerse kling.owner=$TA" \
      || bad "kling.owner ajeno" "set by the daemon" "rc=$rc $out"
    out=$(kb volume ls) && rc=0 || rc=$?
    { [ "$rc" != 0 ] && contiene "$out" "admin"; } && ok "volúmenes: solo admin" || bad "volume ls de inquilino" "needs the admin role" "rc=$rc $out"
    # Snapshots: el de A no lo ve B, y B no puede usar ese nombre (409, sin decir de quién es).
    out=$(ka save -force "$AZM" "$AZS") && rc=0 || rc=$?
    [ "$rc" = 0 ] && ok "save como $TA" || bad "save como $TA" "salida 0" "rc=$rc $out"
    out=$(kb template ls) && rc=0 || rc=$?
    { [ "$rc" = 0 ] && ! contiene "$out" "$AZS"; } && ok "$TB no ve la plantilla de $TA" \
      || bad "template ls de $TB" "salida 0, sin la plantilla de $TA" "rc=$rc $out"
    out=$(kb run -name "$AZM-b" -image min -ttl 10m -on-ttl remove)
    if contiene "$out" "booted"; then
      for rep in "" -replace; do
        # shellcheck disable=SC2086
        out=$(kb save -force $rep "$AZM-b" "$AZS") && rc=0 || rc=$?
        { [ "$rc" != 0 ] && contiene "$out" "is taken" && ! contiene "$out" "$TA"; } \
          && ok "save ${rep:-sin -replace} de $TB sobre el nombre de $TA: 'is taken', sin dueño" \
          || bad "save ${rep:-sin -replace} sobre nombre ajeno" "is taken, sin $TA" "rc=$rc $out"
      done
    else
      bad "run como $TB" "una máquina" "$out"
    fi
  fi
  $KLING rm -f "$AZM" >/dev/null 2>&1; $KLING rm -f "$AZM-b" >/dev/null 2>&1
  $KLING template rm -f "$AZS" >/dev/null 2>&1
fi

# ── 8. grafos de microVMs ─────────────────────────────────────────────────────
# Lo que los tests de Go no pueden ver: que <nodo>.graph resuelva dentro del
# invitado, que el proxy de enlace lleve la conexión al otro netns sin abrir
# el FORWARD, que un lazy despierte con la primera conexión, y que un fork
# hable con SUS nodos y no con los del original. web -> api -> db, db lazy
# desde una plantilla que sirve un fichero en el 5432 (sin Postgres: lo que
# se prueba es la arista, no la base).
step "8. Grafos"
if ! $KLING graph ls >/dev/null 2>&1; then
  printf "  \033[33mskip\033[0m  el daemon no conoce los grafos (capacidad graphs)\n"
else
  G="e2e-g-$$"; GDBT="e2e-gdb-$$"; GTMP=$(mktemp -d)
  GHTTP='import sys, urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1], timeout=50).read().decode().strip())
except Exception as e:
    print("FALLO", type(e).__name__, e)'
  # ghttp <máquina> <url>: GET desde dentro de la máquina.
  ghttp() { $KLING exec -timeout 90s "$1" -- python3 -c "$GHTTP" "$2" 2>&1; }
  # gsirve <máquina> <puerto> <dir>: un servidor HTTP que sobrevive al exec.
  gsirve() { $KLING exec "$1" -- sh -c "mkdir -p $3 && cd $3 && setsid python3 -m http.server $2 >/dev/null 2>&1 </dev/null &" >/dev/null 2>&1; }
  gestado() { $KLING graph inspect "$1" -json 2>/dev/null | python3 -c 'import sys,json; g=json.load(sys.stdin); print(g["state"], " ".join(n+"="+(v.get("state") or "-") for n,v in sorted(g["nodes"].items())))'; }
  gmarca() { $KLING exec "$1" -- sh -c "echo $2 > /srv/db/marca" >/dev/null 2>&1; }

  # La plantilla del nodo lazy: un servidor en el 5432 con un marcador.
  $KLING run -name "$GDBT-m" -image "$IMGVOL" -allow-exec >/dev/null 2>&1 \
    && gsirve "$GDBT-m" 5432 /srv/db && gmarca "$GDBT-m" plantilla && sleep 1 \
    && $KLING save "$GDBT-m" "$GDBT" >/dev/null 2>&1
  $KLING rm "$GDBT-m" >/dev/null 2>&1
  cat > "$GTMP/g.yaml" <<EOF
# e2e: web -> api -> db (lazy)
name: $G
nodes:
  web: {image: $IMGVOL, allow_exec: true}
  api: {image: $IMGVOL, allow_exec: true, ports: [8081]}
  db:  {from: $GDBT, ports: [5432], wake: lazy}
edges:
  - {from: web, to: api, kind: link, port: 8081}
  - {from: api, to: db, kind: link, port: 5432}
EOF
  out=$($KLING graph up "$GTMP/g.yaml" 2>&1)
  if contiene "$out" "Linux-only"; then
    printf "  \033[33mskip\033[0m  aristas entre máquinas: solo Linux en esta versión\n"
  elif ! contiene "$out" "up in"; then
    bad "graph up" "graph $G up" "$out"
  else
    ok "graph up: tres nodos, db lazy sin máquina ($(gestado "$G"))"
    gsirve "$G-api" 8081 /srv/api
    $KLING exec "$G-api" -- sh -c 'echo api-ok > /srv/api/index.html' >/dev/null 2>&1
    sleep 1
    out=$(ghttp "$G-web" http://api.graph:8081/)
    [ "$out" = "api-ok" ] && ok "web -> api.graph:8081 por la arista link" || bad "enlace web -> api" "api-ok" "$out"
    st=$(gestado "$G")
    contiene "$st" "db=-" && ok "db sigue sin máquina antes de la primera conexión" || bad "db lazy" "db=-" "$st"
    out=$(ghttp "$G-api" http://db.graph:5432/marca)
    st=$(gestado "$G")
    { [ "$out" = "plantilla" ] && contiene "$st" "db=running"; } \
      && ok "la primera conexión a db.graph despierta al lazy (db=running)" || bad "despertar lazy" "plantilla, db=running" "$out / $st"
    out=$($KLING exec "$G-web" -- python3 -c 'import socket
try:
    print(socket.gethostbyname("db.graph"))
except socket.gaierror as e:
    print("NXDOMAIN", e)' 2>&1)
    contiene "$out" "NXDOMAIN" && ok "web no resuelve db.graph (no tiene arista)" || bad "db.graph desde web" "NXDOMAIN" "$out"
    out=$($KLING exec "$G-web" -- python3 -c 'import socket
try:
    print(socket.gethostbyname("example.org"))
except socket.gaierror as e:
    print("NXDOMAIN", e)' 2>&1)
    contiene "$out" "NXDOMAIN" && ok "egress none: fuera de *.graph no resuelve nada" || bad "example.org desde web" "NXDOMAIN" "$out"
    out=$($KLING machine audit "$G-web" -json 2>&1)
    contiene "$out" '"kind":"link"' && ok "la auditoría de web tiene la conexión (kind link)" || bad "audit link" '"kind":"link"' "$(printf '%s' "$out" | tail -2)"

    $KLING graph freeze "$G" >/dev/null 2>&1
    st=$(gestado "$G")
    [ "$st" = "frozen api=frozen db=frozen web=frozen" ] && ok "graph freeze: los tres frozen" || bad "graph freeze" "frozen api=frozen db=frozen web=frozen" "$st"
    $KLING graph thaw "$G" >/dev/null 2>&1
    st=$(gestado "$G")
    [ "$st" = "running api=running db=running web=running" ] && ok "graph thaw: los tres running" || bad "graph thaw" "running api=running db=running web=running" "$st"
    out=$(ghttp "$G-web" http://api.graph:8081/)
    [ "$out" = "api-ok" ] && ok "tras el thaw la arista sigue" || bad "enlace tras thaw" "api-ok" "$out"

    out=$($KLING graph snapshot "$G" -json 2>&1)
    n=$(printf '%s' "$out" | python3 -c 'import sys,json; s=json.load(sys.stdin); t=s["templates"]; print(s["generation"], len(t), all(v.endswith("-%d" % s["generation"]) for v in t.values()))' 2>/dev/null)
    [ "$n" = "1 3 True" ] && ok "graph snapshot: tres plantillas de la misma generación" || bad "graph snapshot" "1 3 True" "$out"
    GSNAPS=$(printf '%s' "$out" | python3 -c 'import sys,json; print(" ".join(json.load(sys.stdin)["templates"].values()))' 2>/dev/null)

    gmarca "$G-db" antes-del-fork
    out=$($KLING graph fork "$G" -n 2 -q 2>&1); rc=$?
    GFORKS=$out
    [ "$rc" = 0 ] && [ "$(printf '%s\n' "$GFORKS" | grep -c .)" = 2 ] && ok "graph fork -n 2: dos grafos nuevos" || bad "graph fork" "dos nombres" "rc=$rc $out"
    gmarca "$G-db" original-despues
    for F in $GFORKS; do
      out=$(ghttp "$F-api" http://db.graph:5432/marca)
      [ "$out" = "antes-del-fork" ] && ok "fork $F: su api llega a SU db (estado del instante del fork)" \
        || bad "fork $F: api -> db" "antes-del-fork (no original-despues)" "$out"
      gmarca "$F-db" "propia-$F"
      out=$(ghttp "$F-api" http://db.graph:5432/marca)
      [ "$out" = "propia-$F" ] && ok "fork $F: lo que escribe su db lo ve su api" || bad "fork $F: su db" "propia-$F" "$out"
    done
    out=$(ghttp "$G-api" http://db.graph:5432/marca)
    [ "$out" = "original-despues" ] && ok "el original sigue con su db (ningún fork le llega)" || bad "original tras fork" "original-despues" "$out"

    # Bloque 4: el daemon se reinicia y el grafo y sus enlaces siguen.
    if [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
      ssh "${KLING_HOST#ssh://}" 'sudo systemctl restart kling' >/dev/null 2>&1
      sleep 4
      out=$(ghttp "$G-web" http://api.graph:8081/)
      { [ "$out" = "api-ok" ] && $KLING graph inspect "$G" >/dev/null 2>&1; } \
        && ok "tras reiniciar el daemon el grafo y su enlace siguen" || bad "grafo tras reinicio" "api-ok" "$out"
    else
      echo "  (daemon local: me salto el reinicio para no matar tu sesión)"
    fi

    for F in $GFORKS; do $KLING graph rm "$F" >/dev/null 2>&1; done
    $KLING graph rm "$G" >/dev/null 2>&1
    quedan=$($KLING ps -a 2>/dev/null | grep -c -- "$G" || true)
    tpls=$($KLING template ls -q 2>/dev/null | grep -c "^gfork-" || true)
    { [ "$quedan" = 0 ] && [ "$tpls" = 0 ] && ! $KLING graph inspect "$G" >/dev/null 2>&1; } \
      && ok "graph rm: sin máquinas, sin plantillas temporales de fork, sin grafo" \
      || bad "graph rm" "0 máquinas, 0 gfork-*" "máquinas=$quedan gfork=$tpls"
    for s in $GSNAPS; do $KLING template rm -f "$s" >/dev/null 2>&1; done
  fi
  $KLING graph rm -f "$G" >/dev/null 2>&1
  $KLING template rm -f "$GDBT" >/dev/null 2>&1

  # 8b. Aristas share y depends. Sin enlaces entre máquinas: valen también
  # en macOS. files es lazy, pero web depende de él: arranca con up.
  G2="e2e-g2-$$"
  cat > "$GTMP/g2.yaml" <<EOF
# e2e: web y worker ven /data de files; worker -> web -> files por depends
name: $G2
nodes:
  files:  {image: $IMGVOL, allow_exec: true, wake: lazy}
  web:    {image: $IMGVOL, allow_exec: true}
  worker: {image: $IMGVOL, allow_exec: true}
edges:
  - {from: web, to: files, kind: share, mount: /data}
  - {from: worker, to: files, kind: share, mount: /data, mode: rw}
  - {from: web, to: files, kind: depends}
  - {from: worker, to: web, kind: depends}
EOF
  out=$($KLING graph up "$GTMP/g2.yaml" 2>&1)
  if ! contiene "$out" "up in"; then
    bad "graph up con share y depends" "graph $G2 up" "$out"
  else
    st=$(gestado "$G2")
    [ "$st" = "running files=running web=running worker=running" ] \
      && ok "graph up con depends: files (lazy) arranca porque web depende de él" || bad "up con depends" "los tres running" "$st"
    $KLING exec "$G2-worker" -- sh -c 'echo desde-worker > /data/nota' >/dev/null 2>&1
    out=$($KLING exec "$G2-web" -- cat /data/nota 2>&1)
    [ "$out" = "desde-worker" ] && ok "share: web ve lo que worker escribe en la carpeta de files" || bad "share web" "desde-worker" "$out"
    out=$($KLING exec "$G2-files" -- cat /data/nota 2>&1)
    [ "$out" = "desde-worker" ] && ok "share: files (el dueño) ve la misma carpeta" || bad "share files" "desde-worker" "$out"
    $KLING exec "$G2-web" -- sh -c 'echo no > /data/de-web' >/dev/null 2>&1; rc=$?
    [ "$rc" != 0 ] && ok "share ro: web no puede escribir en ella" || bad "share ro" "fallo al escribir" "rc=$rc"
    out=$($KLING graph snapshot "$G2" 2>&1)
    contiene "$out" "share edges" && ok "graph snapshot se niega con aristas share (y dice por qué)" || bad "snapshot con share" "share edges" "$out"
    $KLING graph freeze "$G2" >/dev/null 2>&1
    $KLING graph thaw "$G2" >/dev/null 2>&1
    st=$(gestado "$G2")
    out=$($KLING exec "$G2-web" -- cat /data/nota 2>&1)
    { [ "$st" = "running files=running web=running worker=running" ] && [ "$out" = "desde-worker" ]; } \
      && ok "freeze/thaw en orden de depends; la carpeta sigue ahí" || bad "freeze/thaw con share" "running, desde-worker" "$st / $out"
    $KLING graph rm "$G2" >/dev/null 2>&1
    quedan=$($KLING ps -a 2>/dev/null | grep -c -- "$G2" || true)
    [ "$quedan" = 0 ] && ok "graph rm con share: sin máquinas" || bad "graph rm con share" "0 máquinas" "$quedan"
  fi
  $KLING graph rm -f "$G2" >/dev/null 2>&1
  # Un ciclo de depends no llega a arrancar nada.
  cat > "$GTMP/ciclo.yaml" <<EOF
name: $G2
nodes:
  a: {image: $IMGVOL}
  b: {image: $IMGVOL}
edges:
  - {from: a, to: b, kind: depends}
  - {from: b, to: a, kind: depends}
EOF
  out=$($KLING graph up "$GTMP/ciclo.yaml" 2>&1)
  contiene "$out" "cycle" && ok "un ciclo de depends se rechaza al validar" || bad "ciclo de depends" "cycle" "$out"
  $KLING graph rm -f "$G2" >/dev/null 2>&1
  rm -rf "$GTMP"
fi

# ── resumen ──────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fallo(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
