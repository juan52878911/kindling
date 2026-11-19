package machine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// cambiarEgresoRed reprograma la red de una máquina viva. Sustituible en tests:
// la de verdad pide root, netns e iptables.
var cambiarEgresoRed = func(n *knet.Net, viejo knet.Egress, viejos []string, nuevo knet.Egress, dominios []string) (knet.ResultadoEgreso, error) {
	return n.CambiarEgreso(viejo, viejos, nuevo, dominios)
}

// SetEgress cambia el modo de salida y/o la lista de dominios de una máquina
// sin reiniciarla (PATCH /machines/{ref}/egress). Con la red montada la
// reprograma en el acto (knet.CambiarEgreso) y deja constancia en el bus de
// eventos y en el log; sin red (parada, congelada con la red soltada) solo
// anota la política, que vale en el próximo arranque. Persiste en los dos casos.
//
// Se rechazan (estado inválido, 409) las máquinas que no se pueden cambiar sin
// romper algo: nodos de un grafo (sus aristas dependen del modo) y las que
// tienen credenciales (solo funcionan en allowlist).
func (m *Manager) SetEgress(ctx context.Context, ref string, req api.EgressRequest) (*api.EgressPolicy, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, noExiste(ref)
	}
	defer m.lock(mc.ID)()
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, noExiste(ref)
	}

	viejoS := cur.Egress
	if viejoS == "" {
		viejoS = string(knet.EgressNone)
	}
	viejo, _ := knet.ParseEgress(viejoS)
	nuevo := viejo
	if req.Egress != "" {
		var err error
		if nuevo, err = knet.ParseEgress(req.Egress); err != nil || string(nuevo) != req.Egress {
			return nil, peticionInvalida("invalid egress %q: use none, internet or allowlist", req.Egress)
		}
	}
	if req.Egress == "" && len(req.AllowDomains) == 0 {
		return nil, peticionInvalida("give egress, allow_domains or both")
	}
	if err := api.ValidateAllowDomains(req.AllowDomains); err != nil {
		return nil, comoPeticion(err)
	}
	var dominios []string
	switch {
	case nuevo != knet.EgressAllowlist:
		if len(req.AllowDomains) > 0 {
			return nil, peticionInvalida("allow_domains only applies to egress allowlist (the new mode is %s)", nuevo)
		}
	case len(req.AllowDomains) > 0:
		dominios = slices.Clone(req.AllowDomains)
	case viejo == knet.EgressAllowlist:
		dominios = slices.Clone(cur.AllowDomains)
	default:
		return nil, peticionInvalida("egress allowlist needs allow_domains")
	}

	m.mu.RLock()
	_, esNodo := m.especRedGrafoLocked(cur)
	m.mu.RUnlock()
	if esNodo {
		return nil, estadoInvalido("machine %q is a node of a graph: its edges depend on its egress mode; change the graph instead", cur.Name)
	}
	if len(cur.CredentialDomains) > 0 && nuevo != knet.EgressAllowlist {
		return nil, estadoInvalido("machine %q has credentials, which only work with egress allowlist: remove them first", cur.Name)
	}

	out := &api.EgressPolicy{Egress: string(nuevo), AllowDomains: dominios}
	if nuevo == viejo && slices.Equal(dominios, cur.AllowDomains) {
		out.Applied = api.EgressAppliedUnchanged
		return out, nil
	}

	n := knet.Plan(cur.NetIndex, cur.ID)
	vivo := cur.State == api.StateRunning || cur.State == api.StatePaused ||
		(cur.State == api.StateWarm && n.Exists())
	out.Applied = api.EgressAppliedNextStart
	if vivo {
		res, err := cambiarEgresoRed(n, viejo, slices.Clone(cur.AllowDomains), nuevo, dominios)
		if err != nil {
			if errors.Is(err, knet.ErrSinRestore) {
				return nil, estadoInvalido("%v", err)
			}
			return nil, fmt.Errorf("changing the egress of %s: %w", cur.Name, err)
		}
		out.Applied = api.EgressAppliedLive
		out.Narrowed, out.ConnectionsClosed = res.Estrecha, res.ConexionesCortadas
	}

	m.mu.Lock()
	live := m.byID[cur.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, retiradaDurante(cur.Name, "changing its egress")
	}
	live.Egress = string(nuevo)
	live.AllowDomains = dominios // se sustituye entero: nunca se modifica en su sitio (doc.go)
	m.persist()
	m.mu.Unlock()

	msg := fmt.Sprintf("egress %s -> %s", viejo, nuevo)
	if nuevo == knet.EgressAllowlist {
		msg += " (" + strings.Join(dominios, ", ") + ")"
	}
	switch {
	case out.Applied == api.EgressAppliedNextStart:
		msg += ": no network up, applies at the next start"
	case out.Narrowed && out.ConnectionsClosed:
		msg += ": narrowed, open connections closed"
	case out.Narrowed:
		msg += ": narrowed; open connections stay until they end (no conntrack on the host)"
	}
	log.Printf("egress: %s (%s): %s", cur.Name, shortID(cur.ID), msg)
	if m.bus != nil {
		m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvEgressChanged, ID: cur.ID, Name: cur.Name, Message: msg})
	}
	return out, nil
}
