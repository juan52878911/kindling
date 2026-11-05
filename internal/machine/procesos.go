package machine

import "strings"

// Lectura de la tabla de procesos: pid -> línea de órdenes (argv unido con
// espacios, como la columna command de `ps -ww`).
//
// Vive en un fichero común, aunque solo la use macOS, para que sus pruebas
// corran también en Linux: es texto, y un fallo aquí deja VMMs huérfanos
// reteniendo RAM o, peor, mata el de una máquina viva.

// argSocketVMM es el argumento con el que el daemon le da su socket a un VMM
// (manager.go, startVMM): kling-vz, kling-hv y firecracker lo llaman igual.
const argSocketVMM = "--api-sock"

// vmmDeLinea devuelve el id de la máquina cuyo socket aparece en la línea de
// órdenes como --api-sock, o "" si no es un VMM de este daemon. prefix es
// "<raíz>/machines/".
//
// Tiene que ser el argumento --api-sock, no la ruta en cualquier sitio: un
// curl --unix-socket .../fc.sock que sondeaba el API de una máquina (o un
// tail, un lsof) pasaba por su VMM. Thaw lo «readoptaba» en vez de arrancar
// uno, la máquina quedaba running con el PID del curl y fallaba en cuanto
// este terminaba; y el vigilante lo mataba como VMM huérfano.
func vmmDeLinea(linea, prefix string) string {
	marca := argSocketVMM + " " + prefix
	i := strings.Index(linea, marca)
	if i < 0 {
		return ""
	}
	id, _, ok := strings.Cut(linea[i+len(marca):], "/fc.sock")
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

// vivosDeVMMEnTabla cuenta los VMM de las máquinas bajo prefix cuyo ejecutable
// se llama nombre ("kling-vz"): siguen corriendo con él y su volcado solo lo
// carga ese VMM.
func vivosDeVMMEnTabla(tabla map[int]string, prefix, nombre string) int {
	n := 0
	for _, linea := range tabla {
		i := strings.Index(linea, argSocketVMM+" "+prefix)
		if i < 0 || vmmDeLinea(linea, prefix) == "" {
			continue
		}
		// El ejecutable va delante de la ruta del socket; su directorio puede
		// llevar espacios ("Application Support").
		antes := linea[:i]
		if strings.HasPrefix(antes, nombre+" ") || strings.Contains(antes, "/"+nombre+" ") {
			n++
		}
	}
	return n
}
