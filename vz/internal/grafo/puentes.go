package grafo

// Puentes al host (kling run -host-port): un puerto TCP del loopback del Mac
// que el invitado alcanza en la pasarela. Es una arista más, con el host al
// otro lado: la red trae aquí cada conexión a un puerto de puente, se pide al
// daemon (linkbroker.KindLoopback) y el daemon comprueba que ese puerto del
// host es de ESTA máquina (se fijó al crearla, nunca desde el invitado),
// marca él mismo y entrega el socket. Este proceso no puede abrir el loopback
// por su cuenta: el perfil de sandbox no lo deja.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// CapPuentes es la capacidad que anuncia GET /kling/info (credential_kinds)
// cuando este proceso sabe atender PUT /kling/host-ports
// (api.CapabilityHostPorts).
const CapPuentes = "host-ports"

// NombrePuente es api.HostBridgeName: el nombre con el que el invitado llega
// a los puentes (el DNS lo contesta con la pasarela).
const NombrePuente = "host.kindling.internal"

// MaxPuentes es api.MaxHostPorts.
const MaxPuentes = 16

// Puente es un puerto del loopback del host (Host) en el puerto Guest de la
// pasarela.
type Puente struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

// ValidarPuentes comprueba los puentes que manda el daemon: las mismas reglas
// que api.ValidateHostPorts, y además que no pisen una arista link.
func (g *Grafo) validarPuentes(ps []Puente) error {
	if len(ps) > MaxPuentes {
		return fmt.Errorf("%d host ports; the limit is %d", len(ps), MaxPuentes)
	}
	vistos := map[int]bool{}
	for _, p := range ps {
		switch {
		case p.Host < 1 || p.Host > 65535 || p.Guest < 1 || p.Guest > 65535:
			return fmt.Errorf("host port %d:%d out of range", p.Host, p.Guest)
		case p.Guest == 53 || p.Guest == 80 || p.Guest == 443:
			return fmt.Errorf("host port %d:%d: guest port %d is reserved on the gateway", p.Host, p.Guest, p.Guest)
		case credproxy.PuertoReservado(p.Host):
			return fmt.Errorf("host port %d is in kindling's reserved forward range", p.Host)
		case vistos[p.Guest]:
			return fmt.Errorf("two host ports on guest port %d", p.Guest)
		}
		if _, ok := g.enlaces[uint16(p.Guest)]; ok {
			return fmt.Errorf("guest port %d is already a graph link", p.Guest)
		}
		vistos[p.Guest] = true
	}
	return nil
}

// SetPuentes fija los puentes al host (sustituye los anteriores; la lista
// vacía los quita).
func (g *Grafo) SetPuentes(ps []Puente) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.validarPuentes(ps); err != nil {
		return err
	}
	nuevos := make(map[uint16]int, len(ps))
	sems := make(map[uint16]chan struct{}, len(ps))
	for _, p := range ps {
		gp := uint16(p.Guest)
		nuevos[gp] = p.Host
		if s, ok := g.semPuentes[gp]; ok {
			sems[gp] = s
		} else {
			sems[gp] = make(chan struct{}, MaxConnsEnlace)
		}
	}
	g.puentes, g.semPuentes = nuevos, sems
	return nil
}

// HayPuentes dice si la máquina tiene algún puente (el DNS contesta entonces
// NombrePuente).
func (g *Grafo) HayPuentes() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.puentes) > 0
}

func (g *Grafo) puente(port uint16) (int, chan struct{}, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	h, ok := g.puentes[port]
	return h, g.semPuentes[port], ok
}

// servirPuente atiende una conexión del invitado al puente del puerto port
// de la pasarela, hacia el puerto hp del loopback del host. Como Serve: se
// pide la conexión antes de aceptar la del invitado, y un host que no escucha
// le llega como RST. Una línea de auditoría por conexión.
func (g *Grafo) servirPuente(ctx context.Context, port uint16, hp int, sem chan struct{}, aceptar func() (net.Conn, error), rechazar func()) {
	inicio := time.Now()
	rec := credproxy.Record{Kind: credproxy.KindLink, Host: net.JoinHostPort(NombrePuente, strconv.Itoa(int(port))),
		Upstream: "host:" + net.JoinHostPort("127.0.0.1", strconv.Itoa(hp))}
	defer func() {
		rec.MS = time.Since(inicio).Milliseconds()
		g.auditar(rec)
	}()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		rechazar()
		rec.Reason = credproxy.ReasonBusy
		return
	}
	up, resp, err := g.pedir(ctx, linkbroker.Request{V: linkbroker.Version, Kind: linkbroker.KindLoopback, Port: hp})
	if err != nil {
		rechazar()
		rec.Denied, rec.Reason = true, motivo(resp.Reason)
		g.logf("host port %d -> 127.0.0.1:%d: %v", port, hp, err)
		return
	}
	defer up.Close()
	unir(up, aceptar, rechazar, &rec)
}
