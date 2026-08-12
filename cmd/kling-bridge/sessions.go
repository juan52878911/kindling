package main

// Cuántas sesiones caben de verdad en esta microVM.
//
// Cada sesión MCP arranca su PROPIO proceso del servidor dentro del invitado:
// el puente no multiplexa un solo proceso, porque un servidor de stdio no sabe
// atender dos conversaciones. Así que el tope de sesiones es, en realidad, un
// tope de procesos, y ponerlo sin mirar la memoria es invitar al OOM killer.
//
// Con el 32 fijo de antes, una microVM de 1 GiB aceptaba 32 servidores de node y
// el invitado se quedaba sin memoria a la mitad. El síntoma era el peor posible:
// las sesiones ya abiertas morían por señal mientras el puente seguía vivo y
// aceptando más.

import (
	"os"
	"strconv"
	"strings"
)

// sessionMiB es lo que se reserva por sesión.
//
// Un servidor MCP de node ronda los 30-40 MiB residentes, y los de Python algo
// más. 64 MiB deja margen para los picos sin ser tan generoso que el tope quede
// en uno. Es una estimación, y por eso el flag sigue existiendo para forzarla.
const sessionMiB = 64

// reservedMiB es lo que NO se reparte: el kernel del invitado, el propio puente
// y el espacio que necesita cualquier proceso para arrancar. Sin esta reserva,
// el tope calculado deja al invitado sin margen y el OOM killer se lleva por
// delante justo lo que acaba de arrancar.
const reservedMiB = 192

// maxSessionsCap acota por arriba aunque sobre memoria: cada sesión es también
// un descriptor y un proceso, y una microVM con cientos de servidores MCP es un
// problema distinto del que este número resuelve.
const maxSessionsCap = 32

// deriveMaxSessions calcula cuántas sesiones caben en la memoria del invitado.
//
// Devuelve al menos 1: un tope de cero dejaría la microVM inservible, y es mejor
// aceptar una sesión y que falle por memoria —con su mensaje— que rechazarlas
// todas desde el principio.
func deriveMaxSessions(totalMiB int) int {
	usable := totalMiB - reservedMiB
	if usable < sessionMiB {
		return 1
	}
	n := usable / sessionMiB
	if n > maxSessionsCap {
		return maxSessionsCap
	}
	return n
}

// guestMemMiB lee la memoria total del invitado. Devuelve 0 si no puede.
func guestMemMiB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(f[0])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
