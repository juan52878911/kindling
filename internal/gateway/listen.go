package gateway

import (
	"net"
	"strings"
)

// IsLoopback dice si una dirección de escucha queda confinada a la máquina.
//
// Existe para poder negarse a activar cosas que solo son aceptables en local.
// La primera es `-pprof`: registrar los perfiles detrás de un flag protege del
// descuido, pero el flag controla el REGISTRO, no el ACCESO — si el gateway
// escucha en la LAN, `-pprof` regala volcados de goroutines y la línea de
// comandos a cualquiera que alcance el puerto, y `/debug/pprof/profile` deja
// elegir a quien llama cuántos segundos de CPU consume.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")

	switch host {
	case "localhost":
		return true
	case "":
		// Host vacío en ":8080" significa todas las interfaces, igual que
		// 0.0.0.0. Se decide aquí porque ParseIP("") no lo distingue.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
