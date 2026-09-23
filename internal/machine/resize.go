package machine

// Memoria elástica.
//
// Firecracker no admite añadir memoria a una VM en marcha, pero sí mover el
// globo (virtio-balloon). Así que la elasticidad se consigue al revés: la VM
// arranca con un TECHO (MemMaxMiB) y el globo inflado reteniendo la diferencia
// con lo que se le da (MemMiB). Subir la memoria es desinflar; bajarla, inflar.
// El invitado no reinicia, no pierde nada, y el host solo paga lo que el
// invitado toca de verdad.

import (
	"context"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// minResizeMiB es lo mínimo que se deja a un invitado: por debajo, el kernel y
// el agente ya no caben y lo siguiente es el OOM dentro.
const minResizeMiB = 128

// globoBase es lo que retiene el globo de una máquina en reposo: la diferencia
// entre su techo y su memoria. 0 en una máquina sin techo.
// objetivoSinEstadisticas es hasta dónde se infla el globo al apretar una
// máquina cuyo invitado no da estadísticas de memoria (macOS): se le deja la
// mitad de su memoria, y nunca menos de minResizeMiB. Es a ciegas, así que es
// prudente: con la mitad un invitado ocioso sigue sirviendo, y el globo se
// desinfla justo después, con lo que el invitado puede volver a crecer.
func objetivoSinEstadisticas(mc *api.Machine) int {
	suelo := max(minResizeMiB, mc.MemMiB/2)
	if suelo >= mc.MemMiB {
		return globoBase(mc) // tan pequeña que no hay nada que apretar
	}
	return globoBase(mc) + mc.MemMiB - suelo
}

func globoBase(mc *api.Machine) int {
	if mc == nil || mc.MemMaxMiB <= mc.MemMiB {
		return 0
	}
	return mc.MemMaxMiB - mc.MemMiB
}

// Resize cambia la memoria de una máquina en marcha dentro de su techo.
func (m *Manager) Resize(ctx context.Context, ref string, memMiB int) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, ErrNoMachine
	}
	defer m.lock(mc.ID)()

	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, ErrNoMachine
	}
	if cur.MemMaxMiB <= 0 {
		return nil, fmt.Errorf("%s was started without mem_max_mib, so its memory is fixed. "+
			"Start it with `kling run -mem-max N` (or commit a snapshot of one that was) to resize it", cur.Name)
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, cur.Name, cur.State)
	}
	if memMiB < minResizeMiB || memMiB > cur.MemMaxMiB {
		return nil, fmt.Errorf("mem_mib has to be between %d and %d (its ceiling)", minResizeMiB, cur.MemMaxMiB)
	}

	// Subir memoria consume RAM del host; se admite como cualquier arranque.
	if crece := memMiB - cur.MemMiB; crece > 0 {
		soltar, err := m.reserveMemoryMakingRoom(ctx, crece, "", cur.ID)
		if err != nil {
			return nil, err
		}
		defer soltar()
	}

	m.mu.RLock()
	sock := m.socket[cur.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", cur.ID)
	}
	c := fc.New(sock)
	objetivo := cur.MemMaxMiB - memMiB
	if err := c.PatchBalloon(ctx, objetivo); err != nil {
		return nil, fmt.Errorf("moving the balloon: %w", err)
	}
	// Al bajar, el invitado va entregando páginas a su ritmo; se espera un poco
	// para que el resultado que se devuelve sea cierto y no una intención.
	if memMiB < cur.MemMiB {
		waitBalloon(ctx, c, objetivo)
	}

	m.mu.Lock()
	vivo := m.byID[cur.ID]
	if vivo == nil {
		m.mu.Unlock()
		return nil, ErrNoMachine
	}
	antes := vivo.MemMiB
	vivo.MemMiB = memMiB
	m.persist()
	out := *vivo
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvResized, ID: cur.ID, Name: cur.Name,
		Message: fmt.Sprintf("memory %d → %d MiB (ceiling %d)", antes, memMiB, cur.MemMaxMiB)})
	return &out, nil
}
