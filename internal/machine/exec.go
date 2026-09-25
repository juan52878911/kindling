package machine

// Lo que el daemon necesita del manager para exec, ficheros y sandboxes. La
// conversación con el invitado la lleva el daemon (daemon/exec.go); aquí se
// decide si la máquina puede recibirla.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
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
	// Con on_ttl=freeze el TTL es un plazo de INACTIVIDAD, no una vida máxima:
	// usar el sandbox lo reinicia, igual que el segador del gateway con los
	// servicios. Sin esto, un sandbox dormido que alguien despierta volvería a
	// dormirse en la siguiente vuelta del vigilante, porque su reloj ya venció.
	// Con on_ttl=remove no se toca: ahí el TTL es una vida máxima y usarlo no
	// debe poder alargarla indefinidamente.
	if mc.OnTTL != api.OnTTLRemove {
		m.touchTTL(mc.ID)
	}
	if mc.State == api.StateWarm || mc.State == api.StatePaused {
		thawed, err := m.Thaw(ctx, mc.ID)
		if err != nil {
			return nil, fmt.Errorf("thawing %s: %w", mc.Name, err)
		}
		mc = thawed
	}
	if mc.State != api.StateRunning || !mc.Reachable() {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	return mc, nil
}

// touchTTL reinicia el reloj del TTL de una máquina que se acaba de usar.
func (m *Manager) touchTTL(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc := m.byID[id]
	if mc == nil || mc.TTLSeconds <= 0 {
		return
	}
	ahora := time.Now()
	mc.TTLAt = &ahora
	m.persist()
}

// Renew hace que el TTL de la máquina venza ttlSeconds a partir de ahora.
//
// Renovar es poner el reloj del TTL (TTLAt) a ahora. Una máquina congelada
// también se puede renovar: un sandbox dormido vence igual, y quien lo quiere
// conservar no debería tener que despertarlo para pedirlo.
//
// ttlSeconds 0 conserva el plazo que ya tenía y solo reinicia el reloj; sobre
// una máquina sin TTL no hace nada.
func (m *Manager) Renew(ref string, ttlSeconds int) (*api.Machine, error) {
	if ttlSeconds < 0 {
		return nil, fmt.Errorf("ttl can't be negative")
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
	if mc.State != api.StateRunning && mc.State != api.StateWarm && mc.State != api.StatePaused {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, mc.Name, mc.State)
	}
	if ttlSeconds == 0 {
		ttlSeconds = mc.TTLSeconds
	}
	if ttlSeconds > 0 {
		ahora := time.Now()
		mc.TTLAt = &ahora
		mc.TTLSeconds = ttlSeconds
		m.persist()
	}
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

// ProbeGuestPort pregunta al VMM si algo escucha en ese puerto dentro del
// invitado. ok=false cuando no se puede preguntar así —Linux, donde el host
// llega al invitado por su IP y basta un dial, o una máquina sin reenvío de ese
// puerto o sin VMM vivo— y quien llama sondea la dirección como siempre.
//
// En macOS es la única forma fiable: el reenvío de loopback lo abre kling-vz y
// acepta la conexión escuche el invitado o no.
func (m *Manager) ProbeGuestPort(ctx context.Context, id string, port int) (open, ok bool) {
	m.mu.RLock()
	mc := m.byID[id]
	sock := m.socket[id]
	var fwd bool
	if mc != nil {
		_, fwd = mc.Forwards[strconv.Itoa(port)]
	}
	m.mu.RUnlock()
	if !fwd || sock == "" {
		return false, false
	}
	open, err := fc.New(sock).KlingProbe(ctx, port)
	if err != nil {
		// Un kling-vz anterior a /kling/probe: se sondea el reenvío.
		return false, false
	}
	return open, true
}
