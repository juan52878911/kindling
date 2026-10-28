package machine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un volcado que falla reanuda la máquina antes de rendirse. Si el VMM no
// contesta a esa reanudación —sigue escribiendo lo que se le pidió, o se
// atascó un momento—, matarlo en el acto (lo que se hacía) perdía el trabajo
// del invitado por algo que suele arreglarse solo en segundos. Ahora se queda
// running con Transition freezing (el planificador no le enruta nada) y el
// vigilante reintenta la reanudación en cada vuelta. Solo si sigue sin
// contestar pasado plazoReanudacion se da por perdida, como antes.

// reanudacion es una máquina pendiente de reanudar.
type reanudacion struct {
	pid   int       // el VMM que quedó en pausa; si cambia, ya no es esta
	desde time.Time // primer intento fallido
	causa string    // por qué falló el volcado y la reanudación
}

// plazoReanudacion es cuánto se reintenta antes de darla por perdida. Es una
// variable para los tests.
var plazoReanudacion = 10 * time.Minute

// plazoReintento es lo que espera cada reintento del vigilante: el VMM ya tuvo
// el plazo largo del volcado; si ahora no contesta en esto, otra vuelta.
const plazoReintento = 10 * time.Second

// apuntarReanudacion deja mc pendiente de reanudar. La transición freezing ya
// la tiene puesta Freeze, que no la retira.
func (m *Manager) apuntarReanudacion(mc *api.Machine, causa, rerr error) {
	log.Printf("freeze %s failed (%v) and resuming it failed too (%v): the watcher will retry", mc.Name, causa, rerr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reanudaciones == nil {
		m.reanudaciones = map[string]reanudacion{}
	}
	pid := mc.PID
	if live := m.byID[mc.ID]; live != nil {
		pid = live.PID
	}
	m.reanudaciones[mc.ID] = reanudacion{pid: pid, desde: time.Now(),
		causa: fmt.Sprintf("freeze failed (%v) and could not resume it either: %v", causa, rerr)}
}

// olvidarReanudacion la retira y, si sigue en freezing, le quita la transición.
// Con m.mu.
func (m *Manager) olvidarReanudacionLocked(id string) {
	delete(m.reanudaciones, id)
	if m.transicion[id] == api.TransitionFreezing {
		delete(m.transicion, id)
	}
}

// reanudarPendientes reintenta las reanudaciones pendientes. La llama el
// vigilante en cada vuelta; sin pendientes no cuesta nada.
func (m *Manager) reanudarPendientes() {
	m.mu.RLock()
	if len(m.reanudaciones) == 0 {
		m.mu.RUnlock()
		return
	}
	ids := make([]string, 0, len(m.reanudaciones))
	for id := range m.reanudaciones {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.reanudarPendiente(id)
	}
}

func (m *Manager) reanudarPendiente(id string) {
	// Con el cerrojo de ciclo de vida, sin esperar: si alguien la tiene (otro
	// freeze, un rm), lo que haga manda; se mira en la siguiente vuelta.
	soltar, ok := m.tryLock(id)
	if !ok {
		return
	}
	defer soltar()

	m.mu.Lock()
	r, hay := m.reanudaciones[id]
	live := m.byID[id]
	sock := m.socket[id]
	if !hay {
		m.mu.Unlock()
		return
	}
	// Ya no es la que quedó en pausa: la borraron, el VMM murió (sweep la dio
	// por failed), o se congeló o paró después.
	if live == nil || live.State != api.StateRunning || live.PID != r.pid || sock == "" {
		m.olvidarReanudacionLocked(id)
		m.mu.Unlock()
		return
	}
	copia := *live
	m.mu.Unlock()

	err := fc.New(sock).ConPlazo(plazoReintento).Resume(context.Background())
	if err == nil {
		log.Printf("%s: resumed after a failed freeze", copia.Name)
		m.mu.Lock()
		m.olvidarReanudacionLocked(id)
		m.mu.Unlock()
		return
	}
	if time.Since(r.desde) < plazoReanudacion {
		return
	}
	// Ni así: la máquina no es recuperable y dejarla running sería mentir.
	m.mu.Lock()
	m.olvidarReanudacionLocked(id)
	m.mu.Unlock()
	m.fail(&copia, fmt.Errorf("%s (gave up after %s: %v)", r.causa, plazoReanudacion, err))
}

// Atascadas son las máquinas que el daemon no ha podido llevar al estado que
// se le pidió y sigue reintentando: un freeze que no pudo reanudarla (A5) y
// un rm cuyo VMM sobrevivió al SIGKILL (M4). Lo enseña `kling doctor`.
func (m *Manager) Atascadas() []api.StuckMachine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []api.StuckMachine
	for id, r := range m.reanudaciones {
		if mc := m.byID[id]; mc != nil {
			out = append(out, api.StuckMachine{ID: id, Name: mc.Name, PID: mc.PID, Transition: api.TransitionFreezing,
				Since: r.desde, Problem: r.causa + "; the daemon keeps trying to resume it"})
		}
	}
	for id, mc := range m.byID {
		if esTumba(mc) && mc.PID != 0 {
			out = append(out, api.StuckMachine{ID: id, Name: mc.Name, PID: mc.PID, Transition: api.TransitionRemoving,
				Problem: fmt.Sprintf("removed, but its VMM (pid %d) survived SIGKILL; its network and disk stay until it exits", mc.PID)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
