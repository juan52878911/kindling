package machine

// Lo que el daemon necesita del manager para exec, ficheros y sandboxes. La
// conversación con el invitado la lleva el daemon (daemon/exec.go); aquí se
// decide si la máquina puede recibirla.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

var (
	// ErrExecNotAllowed: la máquina se creó sin AllowExec.
	ErrExecNotAllowed = errors.New("exec is not enabled on this machine")
	// ErrExecNotInSnapshot: se pidió AllowExec sobre un snapshot que no la tiene.
	ErrExecNotInSnapshot = errors.New("the snapshot has no exec")
	// ErrNoMachine: no hay máquina con esa referencia.
	ErrNoMachine = errors.New("that machine doesn't exist")
	// ErrNotRunning: la máquina no puede atender (parada, fallida...).
	ErrNotRunning = errors.New("the machine is not running")
)

// ExecTarget devuelve la máquina lista para recibir exec o ficheros: existe,
// tiene la ejecución encendida y está corriendo. Una congelada se descongela
// primero —es lo que promete el modelo: vuelve en milisegundos cuando hace
// falta—.
func (m *Manager) ExecTarget(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, ErrNoMachine
	}
	if !mc.AllowExec {
		return nil, fmt.Errorf("%w: %s was created without allow_exec. A service microVM never gets it; "+
			"create a sandbox (kling sandbox create) or a machine with kling run -allow-exec", ErrExecNotAllowed, mc.Name)
	}
	if mc.State == api.StateWarm {
		thawed, err := m.Thaw(ctx, mc.ID)
		if err != nil {
			return nil, fmt.Errorf("thawing %s: %w", mc.Name, err)
		}
		mc = thawed
	}
	if mc.State != api.StateRunning || mc.IP == "" {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	return mc, nil
}

// Renew hace que el TTL de la máquina venza ttlSeconds a partir de ahora.
//
// El TTL se cuenta desde StartedAt, así que alargarlo es subir TTLSeconds hasta
// lo que lleva corriendo más lo pedido. No hace falta un campo nuevo, y una
// máquina renovada que se congela y se descongela sigue la regla de siempre.
func (m *Manager) Renew(ref string, ttlSeconds int) (*api.Machine, error) {
	if ttlSeconds <= 0 {
		return nil, fmt.Errorf("ttl must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var mc *api.Machine
	for _, c := range m.byID {
		if c.ID == ref || c.Name == ref || (len(ref) >= 4 && len(c.ID) >= len(ref) && c.ID[:len(ref)] == ref) {
			mc = c
			break
		}
	}
	if mc == nil {
		return nil, ErrNoMachine
	}
	if mc.State != api.StateRunning || mc.StartedAt == nil {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	elapsed := int(time.Since(*mc.StartedAt) / time.Second)
	mc.TTLSeconds = elapsed + ttlSeconds
	m.persist()
	c := *mc
	return &c, nil
}

// ImageHasAgent dice si una imagen lleva agente de invitado (kling-guest o el
// puente de kindling-mcp, que lo embebe). Sin agente no hay quien atienda exec:
// un sandbox sobre esa imagen arrancaría y nadie contestaría nunca.
func (m *Manager) ImageHasAgent(ctx context.Context, image string) (bool, error) {
	base, layer, err := m.imageLayer(image)
	if err != nil {
		return false, err
	}
	return imageHasBridge(ctx, base, layer)
}
