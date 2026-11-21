package machine

// Reclamo atómico (POST /sandboxes/claim, api.CapabilityClaim): elegir una
// máquina del pool del gateway, etiquetarla y renovarla en una sola sección
// crítica de m.mu. Sustituye a la danza de dos fases con espera fija de 50 ms.

import (
	"errors"
	"fmt"
	"time"

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
	if err := api.ValidateNoGraphLabels(req.Labels); err != nil {
		return nil, comoPeticion(err)
	}
	if req.TTLSeconds < 0 {
		return nil, comoPeticion(errors.New("ttl_seconds can't be negative"))
	}
	switch req.OnTTL {
	case "", api.OnTTLFreeze, api.OnTTLRemove:
	default:
		return nil, comoPeticion(fmt.Errorf("on_ttl must be %q or %q", api.OnTTLFreeze, api.OnTTLRemove))
	}

	m.mu.Lock()
	var elegida *api.Machine
	for id, mc := range m.byID {
		if mc.State != api.StateRunning || !mc.AllowExec || m.transicion[id] != "" || !reclamable(mc.Labels, req) {
			continue
		}
		elegida = mc
		break
	}
	if elegida == nil {
		m.mu.Unlock()
		return nil, marcado(ErrNoReclamable, "no claimable machine matches the request")
	}
	if req.Name != "" && req.Name != elegida.Name {
		for id, mc := range m.byID {
			if mc.Name == req.Name || id == req.Name || m.nombresNaciendo[req.Name] {
				m.mu.Unlock()
				return nil, fmt.Errorf("%w: %q (kling rm it, or pick another -name)", ErrNameTaken, req.Name)
			}
		}
	}
	// La cuota del dueño, con lo que esta máquina va a sumarle: la pregunta
	// que peticionLlana no sabía hacer.
	if owner := req.Labels[api.LabelOwner]; owner != "" {
		if err := m.cuotaExcedidaLocked(owner, usoDe(elegida), elegida.ID); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	now := time.Now()
	elegida.Labels = api.MergeLabels(elegida.Labels, req.Labels)
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
	out := elegida.Clone()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: out.ID, Name: out.Name,
		Message: "claimed from the sandbox pool"})
	return out, nil
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
