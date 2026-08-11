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

// volumeMountpoint lee el punto de montaje de /proc/cmdline.
func volumeMountpoint() string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, tok := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(tok, volumeBootParam+"="); ok && v != "" {
			return v
		}
	}
	return ""
}
