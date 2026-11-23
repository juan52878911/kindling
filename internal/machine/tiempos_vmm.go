package machine

import (
	"context"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// K-03: en macOS las fases del cronómetro (SpawnMS, LoadMS, ResumeMS...) son
// lo que ve el daemon desde fuera; lo que el VMM hace dentro (confinarse,
// crear la VM, cargar el estado) lo mide él y lo da en GET /kling/info
// (timings). Se pide DESPUÉS de contestar, en segundo plano, para no sumar
// una petición al camino de quien despierta o crea, y se cuelga del desglose
// que ya quedó en la máquina. Firecracker no tiene esa ruta: allí no se pide
// (vmmDaTiempos, en plataforma_*.go).

// plazoTiemposVMM acota esa lectura: un VMM que no contesta no deja nada
// esperando más que esto.
const plazoTiemposVMM = 2 * time.Second

// leerTiemposVMM pide a c su desglose de la última creación de la VM. nil si
// no lo da (un VMM anterior, un error) o si no es de una restauración: el de
// un arranque en frío no describe este despertar.
func leerTiemposVMM(ctx context.Context, c *fc.Client) *api.VMMPhases {
	info, err := c.KlingInfo(ctx)
	if err != nil || info.Timings == nil || info.Timings.Kind != "restore" {
		return nil
	}
	return info.Timings
}

// anotarTiemposDespertar cuelga el desglose del VMM de c del despertar fases
// de la máquina id, si sigue siendo el último (otro despertar o un borrado lo
// dejan como está). fases no se toca: ya se devolvió a quien despertó; la
// máquina pasa a señalar una copia. Dice si lo anotó.
func (m *Manager) anotarTiemposDespertar(ctx context.Context, c *fc.Client, id string, fases *api.WakePhases) bool {
	t := leerTiemposVMM(ctx, c)
	if t == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.byID[id]
	if cur == nil || cur.Wake != fases {
		return false
	}
	w := *fases
	w.VMM = t
	cur.Wake = &w
	return true
}

// anotarTiemposCreacion es anotarTiemposDespertar para la creación desde un
// snapshot (CreatePhases).
func (m *Manager) anotarTiemposCreacion(ctx context.Context, c *fc.Client, id string, fases *api.CreatePhases) bool {
	t := leerTiemposVMM(ctx, c)
	if t == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.byID[id]
	if cur == nil || cur.Create != fases {
		return false
	}
	p := *fases
	p.VMM = t
	cur.Create = &p
	return true
}

// tiemposVMMTrasDespertar lanza anotarTiemposDespertar en segundo plano, solo
// donde el VMM los da.
func (m *Manager) tiemposVMMTrasDespertar(c *fc.Client, id string, fases *api.WakePhases) {
	if !vmmDaTiempos || c == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), plazoTiemposVMM)
		defer cancel()
		m.anotarTiemposDespertar(ctx, c, id, fases)
	}()
}

// tiemposVMMTrasCrear es tiemposVMMTrasDespertar para una creación.
func (m *Manager) tiemposVMMTrasCrear(c *fc.Client, id string, fases *api.CreatePhases) {
	if !vmmDaTiempos || c == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), plazoTiemposVMM)
		defer cancel()
		m.anotarTiemposCreacion(ctx, c, id, fases)
	}()
}
