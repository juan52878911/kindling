package machine

// Reclamo atómico (POST /sandboxes/claim, api.CapabilityClaim): elegir una
// máquina del pool del gateway, etiquetarla y renovarla en una sola sección
// crítica de m.mu. Sustituye a la danza de dos fases con espera fija de 50 ms.

import (
	"errors"
	"fmt"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// ErrNoReclamable: ninguna máquina cumple el reclamo.
var ErrNoReclamable = errors.New("no claimable machine")

// Reclamar entrega la primera máquina running, con AllowExec y sin transición
// que case con req (ver api.ClaimRequest). Nunca espera ni arranca nada: una
// dormida se deja al camino lento del llamador.
func (m *Manager) Reclamar(req api.ClaimRequest) (*api.Machine, error) {
	if len(req.Match) == 0 {
		return nil, comoPeticion(errors.New("match can't be empty"))
	}
	if err := api.ValidateLabels(req.Labels); err != nil {
		return nil, comoPeticion(err)
	}
	// Sin marca, la misma máquina se podría reclamar dos veces.
	if req.Labels[api.LabelClaim] == "" && req.Labels[api.LabelOwner] == "" {
		return nil, comoPeticion(fmt.Errorf("labels must carry %s or %s", api.LabelClaim, api.LabelOwner))
	}
	if err := api.ValidateNoGraphLabels(req.Labels); err != nil {
		return nil, comoPeticion(err)
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > api.SandboxMaxTTL {
		return nil, comoPeticion(fmt.Errorf("ttl_seconds must be between 0 and %d", api.SandboxMaxTTL))
	}
	if req.Name != "" && !validName.MatchString(req.Name) {
		return nil, comoPeticion(fmt.Errorf("invalid machine name %q", req.Name))
	}
	switch req.OnTTL {
	case "", api.OnTTLFreeze, api.OnTTLRemove:
	default:
		return nil, comoPeticion(fmt.Errorf("on_ttl must be %q or %q", api.OnTTLFreeze, api.OnTTLRemove))
	}

	// Se elige bajo m.mu, se suelta, se toma el cerrojo de ciclo de vida de la
	// candidata SIN esperar (quien lo tiene está congelando o borrando: Renew
	// y freezeSi/removeSi deciden con él) y se revalida bajo m.mu antes de
	// etiquetar. Una candidata ocupada pasa a la siguiente.
	descartadas := map[string]bool{}
	for {
		m.mu.Lock()
		var id string
		for cid, mc := range m.byID {
			if !descartadas[cid] && candidata(mc, m.transicion[cid], req) {
				id = cid
				break
			}
		}
		m.mu.Unlock()
		if id == "" {
			return nil, marcado(ErrNoReclamable, "no claimable machine matches the request")
		}
		descartadas[id] = true
		suelta, ok := m.tryLock(id)
		if !ok {
			continue
		}
		out, cambia, plan, err := m.etiquetarReclamada(id, req)
		suelta()
		if err != nil {
			return nil, err
		}
		if out == nil { // dejó de ser candidata entre la elección y el cerrojo
			continue
		}
		if cambia {
			// Como SetLabels: un dueño nuevo no hereda sesiones ni agente.
			m.invalidarSesiones(id, "kling db labels changed")
			invalidarAgente(plan)
			invalidarOrigen(id)
		}
		m.bus.Publish(api.Event{Time: *out.TTLAt, Type: api.EvStarted, ID: out.ID, Name: out.Name,
			Message: "claimed from the sandbox pool"})
		return out, nil
	}
}

func candidata(mc *api.Machine, transicion string, req api.ClaimRequest) bool {
	return mc.State == api.StateRunning && mc.AllowExec && transicion == "" && reclamable(mc.Labels, req)
}

// etiquetarReclamada revalida la candidata id con su cerrojo de ciclo de vida
// tomado y la etiqueta. (nil, ...) sin error: ya no es candidata.
func (m *Manager) etiquetarReclamada(id string, req api.ClaimRequest) (*api.Machine, bool, *knet.Net, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	elegida := m.byID[id]
	if elegida == nil || !candidata(elegida, m.transicion[id], req) {
		return nil, false, nil, nil
	}
	if req.Name != "" && req.Name != elegida.Name {
		for oid, mc := range m.byID {
			if mc.Name == req.Name || oid == req.Name || m.nombresNaciendo[req.Name] {
				return nil, false, nil, fmt.Errorf("%w: %q (kling rm it, or pick another -name)", ErrNameTaken, req.Name)
			}
		}
	}
	// Las etiquetas ya fusionadas, no solo las del reclamo.
	fusionadas := api.MergeLabels(elegida.Labels, req.Labels)
	if err := api.ValidateLabels(fusionadas); err != nil {
		return nil, false, nil, comoPeticion(err)
	}
	// La cuota del dueño, con lo que esta máquina va a sumarle.
	if owner := req.Labels[api.LabelOwner]; owner != "" {
		if err := m.cuotaExcedidaLocked(owner, usoDe(elegida), elegida.ID); err != nil {
			return nil, false, nil, err
		}
	}
	now := time.Now()
	cambia := cambianEtiquetasDB(elegida.Labels, req.Labels)
	elegida.Labels = fusionadas
	if req.Name != "" {
		elegida.Name = req.Name
	}
	if req.TTLSeconds > 0 {
		elegida.TTLSeconds = req.TTLSeconds
	}
	if req.OnTTL != "" {
		elegida.OnTTL = req.OnTTL
	}
	elegida.TTLAt = &now
	m.persist()
	return elegida.Clone(), cambia, knet.Plan(elegida.NetIndex, elegida.ID), nil
}

// reclamable: las etiquetas casan con Match y no traen nada de Absent ni lo
// que el daemon exige ausente siempre (dueño: robo entre inquilinos; reclamo:
// ya entregada; fondo del daemon: la gestiona reclamarFondo).
func reclamable(labels map[string]string, req api.ClaimRequest) bool {
	for k, v := range req.Match {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, k := range req.Absent {
		if _, ok := labels[k]; ok {
			return false
		}
	}
	for _, k := range [...]string{api.LabelOwner, api.LabelClaim, api.LabelPool, api.LabelPoolReady} {
		if _, ok := labels[k]; ok {
			return false
		}
	}
	return true
}

// SembrarParaPruebas mete mc en byID sin arrancar nada: para los tests de
// otros paquetes que necesitan máquinas que reclamar sin Firecracker.
func (m *Manager) SembrarParaPruebas(mc *api.Machine) {
	m.mu.Lock()
	m.byID[mc.ID] = mc
	m.mu.Unlock()
}
