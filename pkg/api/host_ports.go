package api

import (
	"fmt"
	"strconv"
	"strings"
)

// CapabilityHostPorts: RunRequest.HostPorts (kling run -host-port). Un daemon
// sin ella ignoraría el campo y la máquina arrancaría sin el puente: el CLI
// lo pregunta antes.
const CapabilityHostPorts = "host-ports"

// HostBridgeName es el nombre con el que el invitado llega a los puertos del
// host que le abre -host-port: el DNS de la máquina lo contesta con la
// pasarela (HostBridgeIP) en todos los modos de egress, solo si tiene alguno.
const HostBridgeName = "host.kindling.internal"

// HostBridgeIP es la pasarela de la red de la máquina en macOS (vz/internal/vnet
// GatewayIP), donde atiende el puente.
const HostBridgeIP = "172.16.0.1"

// MaxHostPorts acota los puertos del host que se le abren a una máquina.
const MaxHostPorts = 16

// HostPort es un puerto TCP del loopback del host (Host) que el invitado
// alcanza en HostBridgeIP:Guest. Solo lo fija quien crea la máquina, nunca el
// invitado.
type HostPort struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

func (p HostPort) String() string {
	if p.Host == p.Guest {
		return strconv.Itoa(p.Host)
	}
	return fmt.Sprintf("%d:%d", p.Host, p.Guest)
}

// ParseHostPort lee el valor de -host-port: "11434" (el mismo puerto en los
// dos lados) o "HOST:GUEST" ("11434:8000": el 11434 del host en el 8000 de
// la pasarela). Solo TCP: "/tcp" se acepta, "/udp" no.
func ParseHostPort(s string) (HostPort, error) {
	v := strings.TrimSpace(s)
	if p, ok := strings.CutSuffix(v, "/tcp"); ok {
		v = p
	} else if strings.Contains(v, "/") {
		return HostPort{}, fmt.Errorf("-host-port %q: only TCP is bridged", s)
	}
	h, g, dos := strings.Cut(v, ":")
	if !dos {
		g = h
	}
	host, err1 := strconv.Atoi(h)
	guest, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil {
		return HostPort{}, fmt.Errorf("-host-port %q: want PORT or HOST:GUEST, like 11434 or 11434:8000", s)
	}
	p := HostPort{Host: host, Guest: guest}
	return p, ValidateHostPorts([]HostPort{p})
}

// ValidateHostPorts comprueba los puertos de RunRequest.HostPorts: en rango,
// sin repetir el del invitado y fuera de los que la pasarela ya usa (53 DNS,
// 80 y 443 del proxy de credenciales). Lo que cada daemon prohíbe además en el
// lado del host (el rango de reenvíos, ver credproxy.PuertoReservado) lo
// comprueba él.
func ValidateHostPorts(ps []HostPort) error {
	if len(ps) > MaxHostPorts {
		return fmt.Errorf("%d host ports; the limit is %d", len(ps), MaxHostPorts)
	}
	vistos := map[int]bool{}
	for _, p := range ps {
		switch {
		case p.Host < 1 || p.Host > 65535 || p.Guest < 1 || p.Guest > 65535:
			return fmt.Errorf("host port %s: ports go from 1 to 65535", p)
		case p.Guest == 53 || p.Guest == 80 || p.Guest == 443:
			return fmt.Errorf("host port %s: guest port %d is taken on %s (DNS and the credential proxy); map it to another one, like %d:%d",
				p, p.Guest, HostBridgeIP, p.Host, p.Guest+10000)
		case vistos[p.Guest]:
			return fmt.Errorf("two host ports on guest port %d", p.Guest)
		}
		vistos[p.Guest] = true
	}
	return nil
}
