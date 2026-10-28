package machine

// El healthcheck de una arista depends (api.GraphEdge.Health): From no
// arranca hasta que el test, ejecutado DENTRO de To, acaba con 0. Es el
// condition: service_healthy de Docker Compose; sin él, lo más que había era
// que un puerto de To contestase, y un postgres acepta conexiones antes de
// aceptar sesiones.
//
// La conversación con el agente para ejecutar la lleva el daemon
// (daemon/exec.go): el gestor recibe una función (FijarSondaExec) y la llama.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// SondaExec ejecuta argv dentro de mc (con exec encendido) con plazo y
// devuelve su código de salida.
type SondaExec func(ctx context.Context, mc *api.Machine, argv []string, plazo time.Duration) (int, error)

// FijarSondaExec da al gestor con qué ejecutar los healthchecks de las
// aristas depends. Sin ella, una arista con Health falla.
func (m *Manager) FijarSondaExec(f SondaExec) {
	m.mu.Lock()
	m.sondaExec = f
	m.mu.Unlock()
}

// esperarSalud repite el test de h dentro de la máquina del nodo hasta que
// pasa, con los tiempos de Docker (api.GraphHealth.Tiempos).
func (m *Manager) esperarSalud(ctx context.Context, gid, nodo string, h *api.GraphHealth) error {
	m.mu.RLock()
	sonda := m.sondaExec
	var mc *api.Machine
	if g := m.grafos[gid]; g != nil {
		mc = m.byID[g.Nodes[nodo].MachineID]
	}
	var copia api.Machine
	if mc != nil {
		copia = *mc
	}
	m.mu.RUnlock()
	switch {
	case sonda == nil:
		return errors.New("this daemon cannot run healthchecks")
	case mc == nil:
		return fmt.Errorf("node %s has no machine", nodo)
	case !copia.AllowExec:
		return fmt.Errorf("node %s runs without exec, so its healthcheck cannot run inside it", nodo)
	}
	return repetirSalud(ctx, h, time.Now, func(ctx context.Context, plazo time.Duration) (int, error) {
		return sonda(ctx, &copia, h.Test, plazo)
	})
}

// repetirSalud es el bucle de esperarSalud, aparte para probarlo. Lo que se
// espera es el primer intento que pasa, así que se intenta cada pasoArranque
// (200 ms) y no cada intervalo; el veredicto de enfermo llega cuando lo daría
// Docker: pasados el arranque y reintentos intervalos.
func repetirSalud(ctx context.Context, h *api.GraphHealth, ahora func() time.Time, intento func(context.Context, time.Duration) (int, error)) error {
	intervalo, plazo, reintentos, arranque, paso := h.Tiempos()
	limite := min(arranque+time.Duration(reintentos)*intervalo, api.GraphHealthMaxWait)
	inicio := ahora()
	n := 0
	for {
		cod, err := intento(ctx, plazo)
		n++
		if err == nil && cod == 0 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("%q exited with %d", strings.Join(h.Test, " "), cod)
		}
		if ahora().Sub(inicio) >= limite {
			return fmt.Errorf("unhealthy after %s (%d attempts): %w", limite.Round(time.Millisecond), n, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), err)
		case <-time.After(paso):
		}
	}
}
