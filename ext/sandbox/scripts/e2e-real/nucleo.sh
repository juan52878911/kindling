#!/usr/bin/env bash
# Prueba del núcleo nuevo con microVMs reales en el daemon privado kt-par.
set -uo pipefail
K="${KLING_BIN:-kling}"
pass=0; fail=0
ok(){ echo "  ok    $1"; pass=$((pass+1)); }
bad(){ echo "  FALLO $1 | esperaba: $2 | obtuvo: $3"; fail=$((fail+1)); }
M=pcore
$K rm -f $M >/dev/null 2>&1
out=$($K run -name $M -image toolchain -mem 512M -allow-exec -egress none 2>&1) || { echo "$out"; exit 1; }
echo "  $(echo "$out" | head -1)"
for _ in $(seq 1 50); do $K exec $M -- true >/dev/null 2>&1 && break; sleep 0.2; done
o=$($K exec $M -- sh -c 'echo hola' 2>&1); [ "$o" = hola ] && ok "exec" || bad exec hola "$o"
# procesos de fondo
pid=$($K exec -d $M -- sh -c 'for i in 1 2 3; do echo linea$i; sleep 1; done; exit 5' 2>&1)
case "$pid" in ''|*[!0-9]*) bad "exec -d" "un pid" "$pid";; *) ok "exec -d devuelve pid $pid";; esac
o=$($K proc status $M "$pid" 2>&1); case "$o" in *running*) ok "proc status: running";; *) bad "proc status" running "$o";; esac
$K proc wait -timeout 20s $M "$pid" >/dev/null 2>&1; c=$?
[ "$c" = 5 ] && ok "proc wait: código 5" || bad "proc wait" 5 "$c"
o=$($K proc out $M "$pid" 2>&1); case "$o" in *linea1*linea3*) ok "proc out trae la salida";; *) bad "proc out" "linea1..linea3" "$o";; esac
# sesiones
sid=$($K session create -w /tmp $M 2>&1 | tail -1)
$K exec -s "$sid" $M -- sh -c 'mkdir -p d1 && cd d1 && export PAR=42' >/dev/null 2>&1
o=$($K exec -s "$sid" $M -- sh -c 'echo "$(pwd) $PAR"' 2>&1)
[ "$o" = "/tmp/d1 42" ] && ok "sesión conserva cwd y env" || bad "sesión" "/tmp/d1 42" "$o (sid=$sid)"
# proceso de fondo sobrevive a freeze/thaw
pid2=$($K exec -d $M -- sh -c 'while :; do date +%s; sleep 1; done' 2>&1)
sleep 1; $K freeze $M >/dev/null 2>&1 && $K thaw $M >/dev/null 2>&1
sleep 2; o=$($K proc status $M "$pid2" 2>&1)
case "$o" in *running*) ok "proceso de fondo vivo tras freeze/thaw";; *) bad "freeze/thaw" running "$o";; esac
$K proc signal $M "$pid2" >/dev/null 2>&1; $K proc wait -timeout 10s $M "$pid2" >/dev/null 2>&1
o=$($K proc status $M "$pid2" 2>&1); case "$o" in *running*) bad "proc signal" "terminado" "$o";; *) ok "proc signal lo para";; esac
# directorios
T=$(mktemp -d); mkdir -p $T/src/sub; echo uno > $T/src/a.txt; echo dos > $T/src/sub/b.txt
$K cp $T/src $M:/tmp/dir >/dev/null 2>&1
o=$($K ls $M:/tmp/dir 2>&1); case "$o" in *a.txt*sub*|*sub*a.txt*) ok "kling ls ve lo subido";; *) bad "kling ls" "a.txt y sub" "$o";; esac
$K cp $M:/tmp/dir $T/vuelta >/dev/null 2>&1
diff -r $T/src $T/vuelta >/dev/null 2>&1 && ok "cp de directorio ida y vuelta idéntico" || bad "cp dir" "idéntico" "$(diff -r $T/src $T/vuelta 2>&1 | head -3)"
rm -rf $T
echo "core: $pass ok · $fail fallo(s)"
