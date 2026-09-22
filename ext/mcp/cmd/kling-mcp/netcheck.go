package main

import (
	stdnet "net"
	"strings"
)

// Copia del criterio de internal/net de kindling: las direcciones que una
// microVM no puede alcanzar nunca. La sonda profunda lo usa para juzgar el
// resolv.conf del invitado.

// blocked son los destinos que una microVM no debe alcanzar jamás, ni siquiera
// con salida a internet habilitada.
var blocked = []string{
	"10.0.0.0/8",     // privada
	"172.16.0.0/12",  // privada (incluye nuestra propia 172.16/30 y 172.30/16)
	"192.168.0.0/16", // privada: la LAN de casa vive aquí
	"169.254.0.0/16", // link-local y metadatos de cloud
	"127.0.0.0/8",    // loopback del host
	"100.64.0.0/10",  // CGNAT
}

// isBlockedIP dice si una IP cae en alguno de los rangos que jamás se permiten,
// para no sembrar el ipset con una privada que devuelva un resolver hostil.
func isBlockedIP(ip stdnet.IP) bool {
	for _, cidr := range blocked {
		if _, n, err := stdnet.ParseCIDR(cidr); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// InalcanzableDesdeUnInvitado dice si una direccion, escrita como texto, es de
// las que una microVM no puede alcanzar jamas.
//
// Es el mismo criterio que filtra lo que entra en el ipset, expuesto para quien
// necesite comprobar una CONFIGURACION en vez de un destino: un resolv.conf que
// apunte a 127.0.0.53 o a la IP privada del router es inservible dentro de un
// invitado, y saberlo no requiere tocar la red.
func InalcanzableDesdeUnInvitado(dir string) bool {
	ip := stdnet.ParseIP(strings.TrimSpace(dir))
	if ip == nil {
		return true // lo que no es una IP no lleva a ninguna parte
	}
	return isBlockedIP(ip)
}
