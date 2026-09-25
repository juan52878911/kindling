package machine

import (
	"context"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// EL NIVEL "PAUSADA".
//
// Entre running (gasta CPU y RAM) y warm (congelada: su memoria en disco, sin
// proceso) hay un punto intermedio: el VMM sigue vivo con los vCPU parados
// (PATCH /vm Paused de Firecracker). No gasta CPU, retiene la RAM que el
// invitado ya tocó (~25 MiB de RSS una tarea Chispa, casi todo caché de página
// del mem.file compartible), y despertarla es solo reanudar: sin VMM nuevo,
// sin LoadSnapshot, sin fallos de página. Lo decide el planificador para lo
// pequeño y popular, con presupuesto de memoria (ver pkg/scheduler, pausar), y
// una pausada que se enfría se congela de verdad. docs/despertar.md tiene las
// cifras.
//
// Lo que NO es: un sitio donde dejar una máquina para siempre. Una pausada
// cuenta como viva para el vigilante, el TTL y la reconciliación (si su VMM
// muere, se trata como una running que muere), y Freeze, Stop y Remove la
// aceptan.

// Pause pausa una máquina running sin volcarla.
func (m *Manager) Pause(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State == api.StatePaused {
		return cur, nil
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("only a running machine can be paused (it is %s)", cur.State)
	}
	// Las carpetas vivas hablan con el invitado: pausado no contestaría y su
	// sesión caería por keepalive. No merece la pena: se congela o se deja.
	if hasLiveShares(cur) {
		return nil, fmt.Errorf("machine %s has live shared folders; freeze it instead of pausing it", cur.Name)
	}
	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}
	start := time.Now()
	if err := fc.New(sock).Pause(ctx); err != nil {
		return nil, fmt.Errorf("pausing: %w", err)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q was removed while it was being paused", mc.Name)
	}
	now := time.Now()
	live.State = api.StatePaused
	live.FrozenAt = &now
	m.persist()
	out := *live
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvFrozen, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("paused in %.1f ms (VMM kept, RAM retained)", msDesde(time.Since(start)))})
	return &out, nil
}

// reanudarLocked despierta una pausada: Resume y nada más. Sin resync: es la
// misma máquina sin clones, y el reloj del invitado (kvmclock) sigue al del
// host durante la pausa. Se llama con el candado de la máquina tomado.
func (m *Manager) reanudarLocked(ctx context.Context, mc *api.Machine, crono *cronoFases) (*api.Machine, error) {
	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}
	if err := fc.New(sock).Resume(ctx); err != nil {
		return nil, fmt.Errorf("resuming: %w", err)
	}
	if crono != nil {
		crono.marca(&crono.p.LoadMS)
	}
	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q was removed while it was being resumed", mc.Name)
	}
	now := time.Now()
	live.State = api.StateRunning
	live.StartedAt = &now
	live.FrozenAt = nil
	m.persist()
	out := *live
	m.mu.Unlock()
	return &out, nil
}
