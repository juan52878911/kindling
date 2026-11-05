package machine

// Puentes al host (kling run -host-port): un puerto TCP del loopback del Mac
// (un Ollama o un llama-server con Metal) que el invitado alcanza en la
// pasarela de su red, 172.16.0.1 o host.kindling.internal, en vez de darle una
// GPU virtual.
//
// Lo fija solo quien crea la máquina (RunRequest.HostPorts) y viaja con ella
// (Machine.HostPorts) para que un thaw o un start lo rehagan; nada del
// invitado lo pide ni lo cambia. kling-hv y kling-vz lo atienden en su
// pasarela (vz/internal/grafo/puentes.go) y piden cada conexión al broker del
// daemon (linkbroker.KindLoopback), que comprueba aquí que ese puerto del
// host es de esa máquina (puertoHostDeMaquina) y marca él: el perfil de
// sandbox no abre el loopback al VMM. Abre ESOS puertos aunque la máquina
// tenga egress none, y nada más: la política de salida no cambia.
//
// En Linux no hay puente: el invitado vive en un netns y llegar al loopback
// del host pediría un proxy por puerto dentro de él; se rechaza con un error
// claro (puenteHostSoportado).

import (
	"context"
	"fmt"
	"slices"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// validarPuentesHost comprueba RunRequest.HostPorts antes de reservar nada.
func validarPuentesHost(ps []api.HostPort) error {
	if len(ps) == 0 {
		return nil
	}
	if !puenteHostSoportado {
		return peticionInvalida("host ports (-host-port) are only supported on macOS (kling-hv and kling-vz); " +
			"on Linux reach a host service through an allowlisted address or run it in another machine")
	}
	if err := api.ValidateHostPorts(ps); err != nil {
		return peticionInvalida("%v", err)
	}
	for _, p := range ps {
		if credproxy.PuertoReservado(p.Host) {
			return peticionInvalida("host port %s: loopback ports %d-%d are kindling's forwards to other machines' guests",
				p, credproxy.ForwardPortMin, credproxy.ForwardPortMax)
		}
	}
	return nil
}

// puertoHostDeMaquina dice si el puerto port del loopback del host es uno de
// los puentes de la máquina id. Con m.mu tomado o no: lee bajo su RLock.
func (m *Manager) puertoHostDeMaquina(id string, port int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	return mc != nil && slices.ContainsFunc(mc.HostPorts, func(p api.HostPort) bool { return p.Host == port })
}

// enviarPuentesVZ manda al kling-vz (o kling-hv) de la máquina sus puentes al
// host, si tiene. Un VMM que no los entiende es un error: la máquina se pidió
// con ellos, y arrancarla sin ellos sería un fallo silencioso.
func enviarPuentesVZ(ctx context.Context, c *fc.Client, ps []api.HostPort) error {
	if len(ps) == 0 {
		return nil
	}
	info, err := c.KlingInfo(ctx)
	if err != nil {
		return fmt.Errorf("asking the VMM for its capabilities: %w", err)
	}
	if !slices.Contains(info.CredentialKinds, api.CapabilityHostPorts) {
		return fmt.Errorf("this VMM can't bridge host ports (-host-port): rebuild kling-hv or kling-vz, " +
			"and make sure the daemon gives it its link broker")
	}
	out := make([]fc.KlingHostPort, 0, len(ps))
	for _, p := range ps {
		out = append(out, fc.KlingHostPort{Host: p.Host, Guest: p.Guest})
	}
	if err := c.SetKlingHostPorts(ctx, out); err != nil {
		return fmt.Errorf("handing the host ports to the VMM: %w", err)
	}
	return nil
}
