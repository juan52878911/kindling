package machine

// Reenvíos del loopback (macOS) frente a los upstream de Postgres fijados por
// el operador. Ver la cabecera de pkg/credproxy/upstream.go: los reenvíos de
// kindling viven en un rango reservado del loopback y ningún upstream puede
// apuntar ahí. Aquí van las dos comprobaciones del lado del daemon: que lo que
// abre kling-vz caiga de verdad en el rango (uno anterior abría puertos al
// azar, y entonces el rango no protegería nada) y, al entregar credenciales,
// que un upstream del loopback no coincida con el reenvío vivo de ninguna
// máquina. La que cuenta de verdad es la del momento de marcar (dialFijado,
// en el proceso que marca); estas dan un error claro antes y cubren versiones
// mezcladas.

import (
	"fmt"
	"net"
	"strconv"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// validarReenvios exige que cada reenvío sea 127.0.0.1 con un puerto del rango
// reservado. Un kling-vz que abra otro puerto es anterior a la regla: sus
// reenvíos podrían ser el upstream de otra máquina, y se para aquí.
func validarReenvios(fwd map[string]string) error {
	for guest, addr := range fwd {
		host, port, err := net.SplitHostPort(addr)
		n, errP := strconv.Atoi(port)
		if err != nil || errP != nil || host != "127.0.0.1" || !credproxy.PuertoReservado(n) {
			return fmt.Errorf("kling-vz forwarded guest port %s at %s, outside kindling's reserved range 127.0.0.1:%d-%d: rebuild kling-vz",
				guest, addr, credproxy.ForwardPortMin, credproxy.ForwardPortMax)
		}
	}
	return nil
}

// puertosReenviadosLocked son los puertos del loopback en los que alguna
// máquina expone a su invitado, con el nombre de la máquina. Con m.mu tomado.
func (m *Manager) puertosReenviadosLocked() map[int]string {
	out := map[int]string{}
	for _, mc := range m.byID {
		for _, addr := range mc.Forwards {
			if _, port, err := net.SplitHostPort(addr); err == nil {
				if n, err := strconv.Atoi(port); err == nil {
					out[n] = mc.Name
				}
			}
		}
	}
	return out
}

// comprobarUpstreams rechaza una credencial cuyo upstream del loopback sea el
// reenvío vivo de una máquina de kindling. Sin m.mu tomado.
func (m *Manager) comprobarUpstreams(creds []credproxy.Credential) error {
	hay := false
	for _, c := range creds {
		if _, ok := credproxy.UpstreamPuertoLoopback(c.Upstream); ok {
			hay = true
		}
	}
	if !hay {
		return nil
	}
	m.mu.RLock()
	ocupados := m.puertosReenviadosLocked()
	m.mu.RUnlock()
	return upstreamsContraReenvios(creds, ocupados)
}

// upstreamsContraReenvios es la comprobación en sí, sin el manager.
func upstreamsContraReenvios(creds []credproxy.Credential, ocupados map[int]string) error {
	for _, c := range creds {
		p, ok := credproxy.UpstreamPuertoLoopback(c.Upstream)
		if !ok {
			continue
		}
		if maq, hit := ocupados[p]; hit {
			return fmt.Errorf("credential for %s: -upstream %s is the loopback port where machine %s's guest is forwarded; a pinned upstream can't point at a kindling machine",
				c.Domain, c.Upstream, maq)
		}
	}
	return nil
}

// credencialesDeSpecs convierte specs de la API en credenciales sin marcador.
func credencialesDeSpecs(specs []api.CredentialSpec) []credproxy.Credential {
	out := make([]credproxy.Credential, len(specs))
	for i, s := range specs {
		out[i] = credencialDeSpec(s)
	}
	return out
}
