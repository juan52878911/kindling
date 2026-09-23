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
# Los volúmenes los monta el agente de invitado, y la imagen mínima no lo lleva:
# el daemon rechaza montarlos ahí a propósito, porque el disco se engancharía y
# nadie lo montaría. Para los bloques de volúmenes hace falta una imagen con
# agente: la de herramientas (`kling images toolchain`) lleva kling-guest.
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
}
trap cleanup EXIT

# ── 1. el daemon está y tiene lo que hace falta ───────────────────────────────
step "1. Daemon"
info=$($KLING info 2>&1) || { echo "$info"; echo "no alcanzo el daemon"; exit 1; }
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
out=$($KLING freeze "$NAME" 2>&1)
contiene "$out" "warm" && ok "freeze -> warm ($(echo "$out" | grep -o '[0-9]* ms' | head -1))" \
  || bad "freeze" "estado warm" "$out"

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

  # Shell interactiva. Necesita un terminal, así que se le pone uno falso con
  # `script`: sin él, `kling shell` se niega a propósito.
  if command -v script >/dev/null 2>&1; then
    out=$(printf 'tty; stty size; exit 5\n' | script -qec "$KLING shell $SB" /dev/null 2>&1 | tr -d '\r')
    contiene "$out" "/dev/pts/" && ok "shell: hay un pseudoterminal de verdad dentro" \
      || bad "kling shell" "un /dev/pts/N" "$out"
    # El código de la shell remota tiene que llegar al proceso local.
    printf 'exit 5\n' | script -qec "$KLING shell $SB" /dev/null >/dev/null 2>&1
    # `script` devuelve el código del comando que envuelve.
    code=$?
    [ "$code" = "5" ] && ok "shell: el código de salida remoto llega al local" \
      || bad "código de kling shell" "5" "$code"
    out=$($KLING shell "$SB" </dev/null 2>&1)
    contiene "$out" "needs a terminal" && ok "shell: se niega sin terminal, y lo explica" \
      || bad "kling shell sin tty" "un rechazo explicando" "$out"
  else
    echo "  (sin util-linux script: me salto la shell interactiva)"
  fi

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
   && $KLING commit "$TPL" "$SNAP" >/dev/null 2>&1; then
  out=$($KLING sandbox create -from "$SNAP" -name "$SB-snap" -q 2>&1) \
    && out=$($KLING exec "$SB-snap" -- cat /root/marca 2>&1)
  [ "$out" = "plantilla" ] && ok "sandbox desde un snapshot con exec, con su estado" \
    || bad "sandbox -from" "plantilla" "$out"
  $KLING sandbox rm "$SB-snap" >/dev/null 2>&1
else
  bad "snapshot con exec" "run -allow-exec + commit" "falló"
fi
$KLING rm "$TPL" >/dev/null 2>&1; $KLING rmi "$SNAP" >/dev/null 2>&1

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
  out=$($KLING commit "$SH-copy" "$SH-snap" 2>&1)
  contiene "$out" "cannot be committed" && ok "commit of a machine with a share is refused" \
    || failE "commit with a share" "cannot be committed" "$out"
  $KLING rmi "$SH-snap" >/dev/null 2>&1
else
  failE "run -share (copy)" "a machine" "$out"
fi
$KLING rm "$SH-copy" >/dev/null 2>&1
rm -rf "$src"

roots=$($KLING info 2>&1 | tr -s ' ' | grep '^share roots:' || true)
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

# ── resumen ──────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fallo(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
