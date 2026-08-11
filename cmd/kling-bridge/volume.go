package main

// Montaje del volumen persistente dentro de la microVM.
//
// El puente es PID 1: corre antes que nadie y es el único sitio donde se puede
// montar sin tocar el init de la imagen base. Ponerlo en el init obligaría a
// reconstruir TODAS las imágenes existentes para estrenar volúmenes; aquí no
// hace falta reconstruir ninguna.
//
// El punto de montaje llega por la línea de comandos del kernel
// (kling.volume=/data) en vez de estar grabado en la imagen, así que el mismo
// snapshot dorado sirve con volúmenes distintos, o montado en otro sitio.
//
// El montaje en sí vive en volume_linux.go: syscall.Mount no existe fuera de
// Linux, y sin separarlo un `go build ./...` en un Mac —donde se desarrolla—
// no compilaría.

import (
	"os"
	"strings"
)

// volumeDevice es el tercer disco. vda es la base de solo lectura y vdb el
// overlay de la máquina; el volumen siempre entra el último.
const volumeDevice = "/dev/vdc"

// volumeBootParam debe coincidir con api.VolumeBootParam. Se repite aquí como
// literal a propósito: el puente se compila estático para el invitado y no debe
// arrastrar el paquete api solo por una constante.
const volumeBootParam = "kling.volume"

// volumeMountpoint lee de /proc/cmdline dónde montar el volumen y en qué modo.
//
// El valor es "/ruta" o "/ruta:ro". El modo va pegado al punto de montaje, no
// como parámetro aparte, para que sea imposible leer uno sin el otro: montar en
// escritura un volumen que se pidió de solo lectura corrompería lo que están
// leyendo las demás microVMs que lo comparten.
func volumeMountpoint() (mountpoint string, readOnly bool) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", false
	}
	for _, tok := range strings.Fields(string(b)) {
		v, ok := strings.CutPrefix(tok, volumeBootParam+"=")
		if !ok || v == "" {
			continue
		}
		if mp, found := strings.CutSuffix(v, ":ro"); found {
			return mp, true
		}
		return v, false
	}
	return "", false
}
