#!/usr/bin/env bash
# Prueba de carga y estrés contra un daemon REAL con KVM.
#
# El e2e (90-e2e.sh) comprueba que cada cosa funciona una vez. Esto comprueba lo
# otro: qué pasa con muchas a la vez, qué pasa cuando no cabe, y si el sistema
# queda limpio después. Las tres preguntas que un test funcional no contesta.
#
#   ./95-stress.sh                  contra el contexto activo de kling
#   N=30 ./95-stress.sh             30 sandboxes en la ráfaga
#   KEEP=1 ./95-stress.sh           no limpia al terminar
#
# Imprime una tabla con lo medido. Sale distinto de cero si algo quedó mal: RAM
# sin devolver, procesos huérfanos o máquinas que no se recogieron.

set -uo pipefail

KLING="${KLING:-kling}"
IMG="${IMG:-toolchain}"
TPL="${TPL:-stress-tpl}"
N="${N:-20}"          # sandboxes de la ráfaga
PAR="${PAR:-10}"      # cuántos a la vez
EXECS="${EXECS:-50}"  # ejecuciones concurrentes
KEEP="${KEEP:-0}"
PREFIJO="st-$$"

fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; }
bad()  { printf "  \033[31mFALLO\033[0m %s\n" "$1"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
med()  { printf "  %-46s %s\n" "$1" "$2"; }

need() { command -v "$1" >/dev/null || { echo "falta $1" >&2; exit 1; }; }
need "$KLING"; need python3

# percentil lee números por stdin y saca min/p50/p95/max en milisegundos.
percentil() {
  # El script va en -c y no en un heredoc: un heredoc ocupa la ENTRADA ESTÁNDAR
  # de python, que es justo por donde llegan los datos. Con heredoc, python leía
  # su propio código como datos y no salía ningún número.
  python3 -c '
import sys
xs = sorted(float(l) for l in sys.stdin if l.strip())
if not xs:
    print("sin datos"); sys.exit()
p = lambda q: xs[min(len(xs)-1, int(q*len(xs)))]
print(f"min {xs[0]:.0f} ms | p50 {p(.5):.0f} ms | p95 {p(.95):.0f} ms | max {xs[-1]:.0f} ms | n={len(xs)}")
'
}

limpiar() {
  [ "$KEEP" = "1" ] && { echo; echo "KEEP=1: no limpio."; return; }
  echo; echo "limpiando..."
  for m in $($KLING ps -a 2>/dev/null | awk -v p="$PREFIJO" '$0 ~ p {print $1}'); do
    $KLING rm "$m" >/dev/null 2>&1
  done
  $KLING rmi "$TPL" >/dev/null 2>&1
}
trap limpiar EXIT

# Estado inicial, para comparar al final.
# Con `set -o pipefail`, el `|| echo 0` de una tuberia cuyo pgrep no encuentra
# nada se dispara AUNQUE el resto haya impreso su cero: salian dos lineas y la
# aritmetica de mas abajo se rompia. Se captura primero y se decide despues.
vmms() {
  local n
  n=$(pgrep -c firecracker 2>/dev/null || true)
  echo "${n:-0}" | tr -cd '0-9'
  echo
}
libre() { awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0; }

VMMS0=$(vmms); LIBRE0=$(libre)
step "0. Punto de partida"
med "microVMs corriendo" "$($KLING ps -q 2>/dev/null | wc -l | tr -d ' ')"
med "procesos firecracker" "$VMMS0"
med "memoria disponible" "$LIBRE0 MiB"

# ── 1. plantilla: una máquina preparada y congelada ──────────────────────────
step "1. Plantilla (arranque en frío + commit)"
t0=$(date +%s%3N)
$KLING run -image "$IMG" -name "$PREFIJO-tpl" -allow-exec >/dev/null 2>&1 \
  && $KLING exec "$PREFIJO-tpl" -- sh -c 'echo plantilla > /root/marca' >/dev/null 2>&1 \
  && $KLING commit "$PREFIJO-tpl" "$TPL" >/dev/null 2>&1
t1=$(date +%s%3N)
if $KLING snapshots 2>/dev/null | grep -q "$TPL"; then
  med "preparar y congelar la plantilla" "$((t1-t0)) ms"
  ok "la plantilla quedó congelada"
else
  bad "no pude preparar la plantilla"; exit 1
fi
$KLING rm "$PREFIJO-tpl" >/dev/null 2>&1

# ── 2. ráfaga de sandboxes desde el snapshot ─────────────────────────────────
step "2. Ráfaga: $N sandboxes desde la plantilla, de $PAR en $PAR"
tmp=$(mktemp -d); rafaga0=$(date +%s%3N)
for i in $(seq 1 "$N"); do
  (
    t0=$(date +%s%3N)
    if $KLING sandbox create -from "$TPL" -name "$PREFIJO-$i" -ttl 10m -q >/dev/null 2>&1; then
      t1=$(date +%s%3N); echo $((t1-t0)) > "$tmp/$i.ok"
    else
      echo 1 > "$tmp/$i.err"
    fi
  ) &
  while [ "$(jobs -r | wc -l)" -ge "$PAR" ]; do wait -n; done
done
wait
rafaga1=$(date +%s%3N)
creados=$(ls "$tmp"/*.ok 2>/dev/null | wc -l | tr -d ' ')
fallidos=$(ls "$tmp"/*.err 2>/dev/null | wc -l | tr -d ' ')
med "creados / pedidos" "$creados / $N"
med "tiempo total de la ráfaga" "$((rafaga1-rafaga0)) ms"
med "latencia de creación" "$(cat "$tmp"/*.ok 2>/dev/null | percentil)"
[ "$fallidos" -gt 0 ] && med "rechazados (sin sitio)" "$fallidos"
# Rechazar por falta de memoria es correcto; lo que no vale es romperse.
[ "$creados" -gt 0 ] && ok "la ráfaga creó sandboxes" || bad "no se creó ninguno"

# ── 3. ejecuciones concurrentes ──────────────────────────────────────────────
step "3. $EXECS ejecuciones concurrentes repartidas entre los sandboxes"
vivos=($($KLING sandbox ls 2>/dev/null | awk -v p="$PREFIJO" '$0 ~ p {print $2}'))
if [ "${#vivos[@]}" -gt 0 ]; then
  tmp2=$(mktemp -d)
  for i in $(seq 1 "$EXECS"); do
    sb="${vivos[$((i % ${#vivos[@]}))]}"
    (
      t0=$(date +%s%3N)
      out=$($KLING exec "$sb" -- sh -c 'echo $((21*2))' 2>&1)
      t1=$(date +%s%3N)
      [ "$out" = "42" ] && echo $((t1-t0)) > "$tmp2/$i.ok" || echo "$out" > "$tmp2/$i.err"
    ) &
    while [ "$(jobs -r | wc -l)" -ge "$PAR" ]; do wait -n; done
  done
  wait
  okx=$(ls "$tmp2"/*.ok 2>/dev/null | wc -l | tr -d ' ')
  med "correctas / pedidas" "$okx / $EXECS"
  med "latencia de exec" "$(cat "$tmp2"/*.ok 2>/dev/null | percentil)"
  [ "$okx" = "$EXECS" ] && ok "todas las ejecuciones devolvieron lo suyo" \
    || bad "$((EXECS-okx)) ejecuciones fallaron: $(cat "$tmp2"/*.err 2>/dev/null | head -2)"
else
  bad "no hay sandboxes vivos que ejercitar"
fi

# ── 4. presión de memoria: crear hasta que no quepa ──────────────────────────
# Con microVMs grandes se llega al limite pronto; con las de la plantilla (que
# comparten el mem.file del snapshot) harian falta cientos, que es justo la
# densidad que este proyecto persigue.
step "4. Presión: crear microVMs de ${GRANDE:-1024} MiB hasta el rechazo"
rechazo=""; extra=0
for i in $(seq 1 60); do
  out=$($KLING sandbox create -image "$IMG" -mem "${GRANDE:-1024}" -name "$PREFIJO-p$i" -ttl 5m -q 2>&1)
  if [ $? -ne 0 ]; then rechazo="$out"; break; fi
  extra=$((extra+1))
done
med "sandboxes extra hasta el rechazo" "$extra"
if [ -n "$rechazo" ]; then
  # Hay tres formas legítimas de decir que no, y las tres explican la causa:
  #
  #   - no cabe (507): la memoria libre del host no da para el tamaño pedido;
  #   - tope de máquinas (409): el límite de seguridad del daemon;
  #   - el invitado no llegó a escuchar: kindling SOBREASIGNA memoria a
  #     propósito —es lo que hace posible la densidad, porque una microVM no
  #     toca toda la RAM que declara— así que con el host saturado el rechazo
  #     llega por ahí y no por la cuenta de memoria. Es honesto siempre que
  #     diga que el host está saturado, que es lo que se comprueba.
  case "$rechazo" in
    *"doesn't fit"*|*"usable"*|*"machine limit"*)
      ok "el rechazo explica la causa (memoria o tope de máquinas)" ;;
    *"saturated"*)
      ok "el rechazo explica la causa (host saturado)" ;;
    *) bad "rechazo poco claro: $rechazo" ;;
  esac
  # El daemon tiene que seguir atendiendo después de decir que no.
  $KLING info >/dev/null 2>&1 && ok "el daemon sigue respondiendo tras el rechazo" \
    || bad "el daemon dejó de responder"
else
  med "rechazo" "no llegó: cabían los 60"
fi

# Las maquinas de la prueba de presion se retiran antes de seguir: congelarlas
# escribiria un mem.file del tamano de su RAM cada una, y con microVMs de 1,5 GiB
# eso llena el disco del laboratorio. La prueba que venia a hacer ya esta hecha.
for m in $($KLING ps -a 2>/dev/null | awk -v p="$PREFIJO-p" '$0 ~ p {print $1}'); do
  $KLING rm "$m" >/dev/null 2>&1
done

# ── 5. congelar y despertar en masa ──────────────────────────────────────────
# Con máquinas propias: las de la ráfaga pueden haber vencido su TTL si la fase
# de presión tardó (en un host anidado tarda), y entonces esta fase se quedaba
# sin nada que congelar y fallaba por el reloj, no por el producto.
K="${K:-12}"
step "5. Congelar y despertar $K microVMs"
for i in $(seq 1 "$K"); do
  $KLING sandbox create -from "$TPL" -name "$PREFIJO-c$i" -ttl 30m -q >/dev/null 2>&1 &
  while [ "$(jobs -r | wc -l)" -ge "$PAR" ]; do wait -n; done
done
wait
ids=($($KLING ps -a 2>/dev/null | awk -v p="$PREFIJO-c" '$0 ~ p && /running/ {print $1}'))
t0=$(date +%s%3N)
for id in "${ids[@]}"; do $KLING freeze "$id" >/dev/null 2>&1 & done; wait
t1=$(date +%s%3N)
med "congelar ${#ids[@]} microVMs" "$((t1-t0)) ms"
tmp3=$(mktemp -d) || { bad "no pude crear un directorio temporal (¿disco lleno?)"; tmp3=""; }
for id in "${ids[@]}"; do
  ( t0=$(date +%s%3N); $KLING thaw "$id" >/dev/null 2>&1 && { t1=$(date +%s%3N); [ -n "$tmp3" ] && echo $((t1-t0)) > "$tmp3/$id"; } ) &
  while [ "$(jobs -r | wc -l)" -ge "$PAR" ]; do wait -n; done
done
wait
med "latencia de thaw" "$(cat "$tmp3"/* 2>/dev/null | percentil)"
vueltas=$(ls "$tmp3" 2>/dev/null | wc -l | tr -d ' ')
[ "${#ids[@]}" -gt 0 ] && [ "$vueltas" = "${#ids[@]}" ] && ok "las $vueltas microVMs volvieron del snapshot" \
  || bad "volvieron $vueltas de ${#ids[@]}"

# ── 6. matar un VMM por debajo: el daemon tiene que darse cuenta ─────────────
step "6. Matar un firecracker a mano (lo que pasa cuando el kernel invitado entra en pánico)"
if $KLING run -image "$IMG" -name "$PREFIJO-victima" >/dev/null 2>&1; then
  # El id completo sale del JSON: `ps` lo recorta, y con jailer el proceso se
  # llama /firecracker y solo lleva el id entero en --id.
  victima=$($KLING ps -a -json 2>/dev/null | python3 -c 'import json,sys; print(next((m["id"] for m in json.load(sys.stdin) if m["name"].endswith("-victima")), ""))')
  pid=$(pgrep -f -- "--id $victima" | head -1)
  if [ -n "$pid" ]; then
    sudo kill -9 "$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null
    sleep 12   # el vigilante pasa cada 10 s
    estado=$($KLING ps -a -json 2>/dev/null | python3 -c 'import json,sys; v=sys.argv[1]; print(next((m["state"] for m in json.load(sys.stdin) if m["id"]==v), ""))' "$victima")
    case "$estado" in
      failed|"") ok "el daemon marcó la máquina muerta (estado: ${estado:-recogida})" ;;
      *) bad "sigue diciendo que está $estado sobre un proceso que ya no existe" ;;
    esac
  else
    bad "no encontré el proceso de la víctima ($victima)"
  fi
else
  bad "no pude arrancar la víctima"
fi

# ── 7. lo que queda al final ─────────────────────────────────────────────────
step "7. Estado final"
for m in $($KLING ps -a 2>/dev/null | awk -v p="$PREFIJO" '$0 ~ p {print $1}'); do
  $KLING rm "$m" >/dev/null 2>&1
done
sleep 3
VMMS1=$(vmms); LIBRE1=$(libre)
med "procesos firecracker (antes → después)" "$VMMS0 → $VMMS1"
med "memoria disponible (antes → después)" "$LIBRE0 → $LIBRE1 MiB"
[ "${VMMS1:-0}" -le "${VMMS0:-0}" ] && ok "no quedaron procesos huérfanos" \
  || bad "quedaron $((VMMS1-VMMS0)) firecracker de más"
# 200 MiB de margen: la caché del host se mueve sola.
python3 -c "import sys; sys.exit(0 if $LIBRE1 >= $LIBRE0 - 200 else 1)" \
  && ok "la memoria volvió al host" || bad "faltan $((LIBRE0-LIBRE1)) MiB por devolver"

printf "\n\033[1m%d fallo(s)\033[0m\n" "$fail"
[ "$fail" -eq 0 ]
