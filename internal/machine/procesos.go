package machine

import (
	"strconv"
	"strings"
)

// Lectura de la tabla de procesos tal como la da `ps -axww -o pid=,command=`.
//
// Vive en un fichero común, aunque solo la use macOS, para que sus pruebas
// corran también en Linux: es texto, y un fallo aquí deja VMMs huérfanos
// reteniendo RAM o, peor, mata el de una máquina viva.

// parsearPS convierte la salida de ps en pid -> línea de órdenes.
//
// La línea viene con los argumentos unidos por espacios, así que no se puede
// volver a partir: la raíz de macOS ("Application Support") ya lleva uno. Por
// eso quien la usa busca subcadenas y no argumentos.
func parsearPS(out string) map[int]string {
	res := map[int]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		pidStr, cmd, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		res[pid] = strings.TrimSpace(cmd)
	}
	return res
}

// vmmDeLinea devuelve el id de la máquina cuyo socket aparece en la línea de
// órdenes, o "" si no es un VMM de este daemon. prefix es
// "<raíz>/machines/".
func vmmDeLinea(linea, prefix string) string {
	i := strings.Index(linea, prefix)
	if i < 0 {
		return ""
	}
	id, _, ok := strings.Cut(linea[i+len(prefix):], "/fc.sock")
	if !ok || id == "" || strings.ContainsAny(id, "/ ") {
		return ""
	}
	return id
}

// vmmsDeTabla es liveVMs sobre una tabla ya leída: id de máquina -> pid.
func vmmsDeTabla(tabla map[int]string, prefix string) map[string]int {
	out := map[string]int{}
	for pid, linea := range tabla {
		if id := vmmDeLinea(linea, prefix); id != "" {
			out[id] = pid
		}
	}
	return out
}
