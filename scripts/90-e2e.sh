#!/usr/bin/env bash
# Prueba de extremo a extremo contra un daemon REAL.
#
# Los tests de Go cubren la lógica, pero no pueden cubrir lo que de verdad se
# rompe en este proyecto: que una microVM arranque, que su red funcione, que el
# snapshot dorado despierte, que el gateway enrute con token y que un volumen
# sobreviva a la máquina. Todo eso necesita KVM, y por eso vive aquí y no en el
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
SVC="${SVC:-e2e-eco}"
VOL="${VOL:-e2e-vol}"
VOL2="${VOL2:-e2e-vol2}"
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
  [ "$KEEP" = "1" ] && { echo; echo "KEEP=1: no limpio. Restos: servicio $SVC, volumen $VOL"; return; }
  echo
  echo "limpiando..."
  for m in $($KLING ps -a 2>/dev/null | awk '/e2e-/ {print $1}'); do
    $KLING rm "$m" >/dev/null 2>&1
  done
  $KLING rmi "$SVC" >/dev/null 2>&1
  $KLING volume rm "$VOL" >/dev/null 2>&1
  $KLING volume rm "$VOL2" >/dev/null 2>&1
}
trap cleanup EXIT

# ── 1. el daemon está y tiene lo que hace falta ───────────────────────────────
step "1. Daemon"
info=$($KLING info 2>&1) || { echo "$info"; echo "no alcanzo el daemon"; exit 1; }
# Los espacios de la tabla son variables, así que se aplasta antes de comparar.
kvm=$(printf '%s' "$info" | tr -s ' ' | grep -i '^KVM:' || true)
contiene "$kvm" "sí" && ok "KVM disponible" || bad "KVM" "KVM: sí" "${kvm:-nada}"
contiene "$info" "irecracker" && ok "firecracker instalado" \
  || bad "firecracker" "una versión" "nada"

# ── 2. ciclo de vida: arrancar, congelar, descongelar ─────────────────────────
step "2. Ciclo de vida de una microVM"
NAME="e2e-vida-$$"
out=$($KLING run -name "$NAME" -image min 2>&1)
if contiene "$out" "arrancada"; then
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
contiene "$out" "creado" && ok "volumen creado" || bad "volume create" "creado" "$out"

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
if $KLING run -name "$NAME" -image min -volume "$VOL" >/dev/null 2>&1; then
  # No se puede BORRAR mientras alguien lo usa.
  out=$($KLING volume rm "$VOL" 2>&1)
  contiene "$out" "usan" && ok "se niega a borrar un volumen en uso" \
    || bad "volume rm en uso" "un rechazo" "$out"

  # Y tampoco se puede MONTAR dos veces. Un ext4 no admite dos escritores: cada
  # uno cachea metadatos que el otro no ve y el resultado es corrupción. Esta
  # comprobación es lo único que lo impide.
  out=$($KLING run -name "$NAME-bis" -image min -volume "$VOL" 2>&1)
  if contiene "$out" "en ESCRITURA"; then
    ok "se niega a montar el mismo volumen en dos escritores"
  else
    bad "doble montaje" "un rechazo nombrando a $NAME" "$out"
    $KLING rm "$NAME-bis" >/dev/null 2>&1
  fi

  # Con un escritor dentro no entra NADIE, ni a leer: vería metadatos cambiando
  # bajo sus pies.
  out=$($KLING run -name "$NAME-ro" -image min -volume "$VOL:/x:ro" 2>&1)
  if contiene "$out" "en ESCRITURA"; then
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
  if $KLING run -name "e2e-lec-$i-$$" -image min -volume "$VOL:/libs:ro" >/dev/null 2>&1; then
    LECTORES=$((LECTORES+1))
  fi
done
[ "$LECTORES" = "3" ] && ok "tres microVMs lo montan a la vez en lectura" \
  || bad "lectores concurrentes" "3" "$LECTORES"

# Y con lectores dentro no entra un escritor.
out=$($KLING run -name "e2e-esc-$$" -image min -volume "$VOL" 2>&1)
if contiene "$out" "leyendo"; then
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
if $KLING run -name "$NAME" -image min -volume "$VOL:/uno" -volume "$VOL2:/dos:ro" >/dev/null 2>&1; then
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
out=$($KLING run -name "e2e-choque-$$" -image min -volume "$VOL:/x" -volume "$VOL2:/x" 2>&1)
contiene "$out" "taparía" && ok "rechaza dos volúmenes en el mismo punto" \
  || bad "puntos de montaje repetidos" "un rechazo" "$out"
$KLING rm "e2e-choque-$$" >/dev/null 2>&1

# ── 4. el gateway pide token ─────────────────────────────────────────────────
step "4. Gateway y autenticación"
GW=$($KLING config show 2>/dev/null | awk '/^gateway.url/{print $2}')
if [ -z "$GW" ] || [ "$GW" = "" ]; then
  echo "  (sin gateway.url configurado; me salto el bloque)"
else
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GW/services")
  [ "$code" = "401" ] && ok "/services sin token -> 401" || bad "auth" "401" "$code"
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GW/healthz")
  [ "$code" = "200" ] && ok "/healthz abierto -> 200" || bad "healthz" "200" "$code"
fi

# ── 5. reconcile no destruye máquinas vivas ──────────────────────────────────
# El caso que motivó reescribirlo: el daemon se reinicia y una microVM viva NO
# debe perder su red ni su cgroup, aunque el estado en disco vaya por detrás.
step "5. El daemon se reinicia sin llevarse las microVMs por delante"
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

# ── resumen ──────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fallo(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
