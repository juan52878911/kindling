package machine

import "strings"

// Lectura de la tabla de procesos: pid -> línea de órdenes (argv unido con
// espacios, como la columna command de `ps -ww`).
//
// Vive en un fichero común, aunque solo la use macOS, para que sus pruebas
// corran también en Linux: es texto, y un fallo aquí deja VMMs huérfanos
// reteniendo RAM o, peor, mata el de una máquina viva.

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
