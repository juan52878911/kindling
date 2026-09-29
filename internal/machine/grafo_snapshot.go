package machine

// Snapshot y fork de un grafo entero (docs/grafos.md).
//
// UN INSTANTE CONSISTENTE: Commit pausa, vuelca y reanuda UNA máquina; hacerlo
// nodo a nodo guardaría la app de un momento y su base del siguiente. Para el
// grafo: (1) se pausan todos los nodos que corren; (2) se cortan las sesiones
// hacia ellos (Pause ya lo hace, y se repite para los que ya estaban
// pausados): una TCP a medias no sobrevive a una restauración, y mejor que
// muera limpia; (3) se vuelca cada uno SIN reanudarlo (commitPausada); (4) se
// reanudan todos. Cada nodo está pausado de forma continua desde (1) hasta su
// volcado, así que todos los volcados son del instante (1). Si un volcado
// falla, se reanuda todo y se borran las plantillas ya hechas.
//
// FORK: un snapshot consistente temporal (con la marca de fork, como `kling
// sandbox fork`) y, por cada copia, un grafo NUEVO (otro ID) cuyos nodos
// arrancan de esas plantillas. Las aristas se resuelven por (grafo, nodo), así
// que las copias se ven entre sí y nunca al original, sin tocar nada dentro de
// los invitados. Los invitados despiertan con los marcadores del original en
// la memoria: a cada copia se le entregan los MISMOS marcadores con el destino
// reescrito a su propio grafo. Es explícito (una arista del grafo, un fork
// pedido), a diferencia del fork de un sandbox con credenciales sueltas, que
// se sigue rechazando: un nodo con credenciales que no son de sus aristas no
// se ramifica.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Sustituibles en los tests (en init por el mismo ciclo que grafo.go).
var (
	pausarNodoGrafo     func(ctx context.Context, m *Manager, id string) error
	reanudarNodoGrafo   func(ctx context.Context, m *Manager, id string) error
	commitNodoPausado   func(ctx context.Context, m *Manager, id, name string) error
	borrarSnapshotGrafo func(m *Manager, name string) error
)

func init() {
	pausarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		_, err := m.Pause(ctx, id)
		return err
	}
	// Thaw de una pausada es reanudarla (pausa.go).
	reanudarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		_, err := m.Thaw(ctx, id)
		return err
	}
	commitNodoPausado = func(ctx context.Context, m *Manager, id, name string) error {
		_, err := m.commitPausada(ctx, id, name)
		return err
	}
	borrarSnapshotGrafo = func(m *Manager, name string) error { return m.removeSnapshot(name, true) }
}

// nodoMaquina es un nodo instanciado con su máquina.
type nodoMaquina struct {
	nodo, id string
	estado   api.State
}

// nodosParaVolcar son los nodos con máquina del grafo, que tienen que estar
// corriendo o pausados: uno congelado no tiene VMM que volcar (despierta el
// grafo antes). Los lazy sin instancia no se vuelcan: siguen siendo su
// plantilla.
func (m *Manager) nodosParaVolcar(gid string) ([]nodoMaquina, error) {
	nombres, ids := m.maquinasDeGrafo(gid)
	var out []nodoMaquina
	for i, id := range ids {
		mc, ok := m.Get(id)
		if !ok {
			return nil, fmt.Errorf("the machine of node %s no longer exists", nombres[i])
		}
		switch mc.State {
		case api.StateRunning, api.StatePaused:
		case api.StateWarm:
			return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf(
				"node %s is frozen: thaw the graph first (kling graph thaw), then snapshot it", nombres[i])}
		default:
			return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf("node %s is %s", nombres[i], mc.State)}
		}
		out = append(out, nodoMaquina{nodo: nombres[i], id: id, estado: mc.State})
	}
	if len(out) == 0 {
		return nil, &api.StatusError{Code: 409, Message: "no node of the graph has a machine yet: nothing to snapshot"}
	}
	return out, nil
}

// snapshotConsistente vuelca los nodos del grafo gid en el mismo instante,
// uno en la plantilla nombre(nodo) cada uno, y devuelve las plantillas por
// nodo. Con el cerrojo del grafo tomado.
func (m *Manager) snapshotConsistente(ctx context.Context, gid string, nombre func(nodo string) string) (map[string]string, error) {
	nodos, err := m.nodosParaVolcar(gid)
	if err != nil {
		return nil, err
	}
	// Deshacer no es opcional aunque quien lo pidió se haya ido.
	limpio := context.WithoutCancel(ctx)
	var pausadas []string
	reanudar := func() {
		for _, id := range pausadas {
			if err := reanudarNodoGrafo(limpio, m, id); err != nil {
				log.Printf("graph %s: couldn't resume %s after the snapshot: %v", shortID(gid), shortID(id), err)
			}
		}
	}
	// (1) pausar todos los que corren.
	for _, n := range nodos {
		if n.estado != api.StateRunning {
			continue
		}
		if err := pausarNodoGrafo(ctx, m, n.id); err != nil {
			reanudar()
			return nil, fmt.Errorf("pausing node %s: %w", n.nodo, err)
		}
		pausadas = append(pausadas, n.id)
	}
	// (2) ninguna sesión hacia el grafo cruza el instante.
	for _, n := range nodos {
		m.invalidarSesiones(n.id, "graph snapshot")
	}
	// (3) volcar cada uno, sin reanudarlo.
	hechas := map[string]string{}
	for _, n := range nodos {
		name := nombre(n.nodo)
		if err := commitNodoPausado(ctx, m, n.id, name); err != nil {
			for _, s := range hechas {
				if berr := borrarSnapshotGrafo(m, s); berr != nil {
					log.Printf("graph %s: couldn't remove partial template %s: %v", shortID(gid), s, berr)
				}
			}
			reanudar()
			return nil, fmt.Errorf("snapshot of node %s: %w", n.nodo, err)
		}
		hechas[n.nodo] = name
	}
	// (4) reanudar los que pausamos (los que ya estaban pausados, así siguen).
	reanudar()
	return hechas, nil
}

// GraphSnapshot guarda el grafo en un instante consistente: una plantilla
// <prefijo>-<nodo>-<generación> por nodo con máquina, todas de la misma
// generación. prefijo "" es el nombre del grafo.
func (m *Manager) GraphSnapshot(ctx context.Context, ref, prefijo string) (*api.GraphSnapshot, error) {
	gid, err := m.idGrafo(ref)
	if err != nil {
		return nil, err
	}
	defer m.lock(claveCerrojoGrafo(gid))()
	m.mu.RLock()
	g := m.grafos[gid]
	var nombreGrafo string
	var gen int
	if g != nil {
		nombreGrafo, gen = g.Name, g.Generation+1
	}
	m.mu.RUnlock()
	if g == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoGraph, gid)
	}
	if prefijo == "" {
		prefijo = nombreGrafo
	}
	if err := api.ValidateGraphName(prefijo); err != nil {
		return nil, &api.StatusError{Code: 400, Message: "-name: " + err.Error()}
	}
	nombre := func(nodo string) string { return fmt.Sprintf("%s-%s-%d", prefijo, nodo, gen) }
	nodos, err := m.nodosParaVolcar(gid)
	if err != nil {
		return nil, err
	}
	for _, n := range nodos {
		name := nombre(n.nodo)
		if !validName.MatchString(name) {
			return nil, &api.StatusError{Code: 400, Message: fmt.Sprintf("template name %q is not valid", name)}
		}
		if _, err := os.Stat(m.snapDir(name)); err == nil {
			return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf("template %q already exists (use another -name, or kling template rm it)", name)}
		}
	}
	plantillas, err := m.snapshotConsistente(ctx, gid, nombre)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if g := m.grafos[gid]; g != nil {
		g.Generation = gen
	}
	m.mu.Unlock()
	if err := m.guardarGrafo(gid); err != nil {
		return nil, err
	}
	log.Printf("graph %s (%s): snapshot generation %d, %d template(s)", nombreGrafo, shortID(gid), gen, len(plantillas))
	return &api.GraphSnapshot{Graph: nombreGrafo, Generation: gen, Templates: plantillas}, nil
}

// nombreFork es el nombre de un grafo copia: el del original recortado y un
// sufijo al azar (24 como mucho, ver api.ValidateGraphName).
func nombreFork(original string) string {
	if len(original) > 15 {
		original = strings.TrimRight(original[:15], "-")
	}
	return original + "-" + newID()[:8]
}

// credencialesDeFork comprueba que las credenciales de la máquina de cada
// nodo son solo las de sus aristas (las demás no se multiplican por N sin que
// nadie lo pida) y las devuelve por nodo.
func (m *Manager) credencialesDeFork(gid string, nodos []nodoMaquina) (map[string][]credproxy.Credential, error) {
	out := map[string][]credproxy.Credential{}
	for _, n := range nodos {
		creds, err := m.cargarCredenciales(n.id)
		if err != nil {
			return nil, fmt.Errorf("node %s: reading its credentials: %w", n.nodo, err)
		}
		for _, c := range creds {
			if c.UpstreamOwner != gid || c.UpstreamMachine == "" {
				return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf(
					"node %s has a credential that is not one of its graph edges (%s for %s), and a fork would hand it to every copy; "+
						"remove it (kling machine credential) or declare it as an edge", n.nodo, c.Env, c.Domain)}
			}
		}
		out[n.nodo] = creds
	}
	return out, nil
}

// GraphFork ramifica el grafo en n grafos nuevos desde un instante
// consistente. Todo o nada.
func (m *Manager) GraphFork(ctx context.Context, ref string, n int) (out []*api.Graph, errOut error) {
	if n == 0 {
		n = 1
	}
	if n < 0 || n > api.GraphForkMax {
		return nil, &api.StatusError{Code: 400, Message: fmt.Sprintf("count must be between 1 and %d", api.GraphForkMax)}
	}
	gid, err := m.idGrafo(ref)
	if err != nil {
		return nil, err
	}
	defer m.lock(claveCerrojoGrafo(gid))()
	m.mu.RLock()
	orig := m.grafos[gid]
	if orig != nil {
		orig = copiaGrafo(orig)
	}
	m.mu.RUnlock()
	if orig == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoGraph, gid)
	}
	nodos, err := m.nodosParaVolcar(gid)
	if err != nil {
		return nil, err
	}
	credsPorNodo, err := m.credencialesDeFork(gid, nodos)
	if err != nil {
		return nil, err
	}
	secretos, err := m.cargarSecretosGrafo(gid)
	if err != nil {
		return nil, err
	}
	sufijo := newID()[:8]
	nombre := func(nodo string) string { return "gfork-" + gid[:12] + "-" + nodo + "-" + sufijo }
	// Reservadas de principio a fin: entre el volcado y la primera
	// restauración, ni el barrido de forks ni un `template rm` se las llevan.
	for _, nd := range nodos {
		defer m.reserveDir(reservaSnapshot(nombre(nd.nodo)))()
	}
	plantillas, err := m.snapshotConsistente(ctx, gid, nombre)
	if err != nil {
		return nil, fmt.Errorf("forking graph %s: %w", orig.Name, err)
	}
	var creados []string
	defer func() {
		if errOut == nil {
			return
		}
		limpio := context.WithoutCancel(ctx)
		for _, c := range creados {
			_ = m.eliminarGrafo(limpio, c)
		}
		for _, p := range plantillas {
			if !m.forkEnUso(p) {
				_ = m.removeSnapshot(p, true)
			}
		}
		out = nil
	}()
	for _, p := range plantillas {
		origen := ""
		for _, nd := range nodos {
			if plantillas[nd.nodo] == p {
				origen = nd.id
			}
		}
		if err := os.WriteFile(filepath.Join(m.snapDir(p), forkMarca), []byte(origen+"\n"), 0o644); err != nil {
			return nil, fmt.Errorf("marking fork template %s: %w", p, err)
		}
	}
	origDe := map[string]string{} // nodo -> máquina original
	for _, nd := range nodos {
		origDe[nd.nodo] = nd.id
	}
	gen := orig.Generation + 1
	for i := 0; i < n; i++ {
		ng := copiaGrafo(orig)
		ng.ID = newID()
		ng.Name = nombreFork(orig.Name)
		ng.ForkOf = gid
		ng.Generation = gen
		ng.CreatedAt = time.Now().UTC()
		ng.State = ""
		for nombreNodo, nd := range ng.Nodes {
			nd.MachineID, nd.State = "", ""
			if p := plantillas[nombreNodo]; p != "" {
				nd.From, nd.Image, nd.Shares = p, "", nil
			}
			ng.Nodes[nombreNodo] = nd
		}
		m.mu.Lock()
		if m.grafos == nil {
			m.grafos = map[string]*api.Graph{}
		}
		m.grafos[ng.ID] = ng
		m.mu.Unlock()
		creados = append(creados, ng.ID)
		if err := m.guardarSecretosGrafo(ng.ID, secretos); err != nil {
			return nil, err
		}
		if err := m.guardarGrafo(ng.ID); err != nil {
			return nil, err
		}
		for _, nombreNodo := range ng.SortedNodeNames() {
			if plantillas[nombreNodo] == "" {
				continue // lazy sin instancia en el original: sigue así
			}
			if err := m.instanciarNodo(ctx, ng.ID, nombreNodo, origDe[nombreNodo]); err != nil {
				return nil, fmt.Errorf("fork %d of %d: node %s: %w", i+1, n, nombreNodo, err)
			}
		}
		for _, nombreNodo := range ng.SortedNodeNames() {
			creds, err := m.credencialesReescritas(gid, ng.ID, credsPorNodo[nombreNodo])
			if err != nil {
				return nil, err
			}
			if err := m.conectarNodo(ctx, ng.ID, nombreNodo, creds); err != nil {
				return nil, fmt.Errorf("fork %d of %d: node %s: %w", i+1, n, nombreNodo, err)
			}
		}
		g, err := m.Graph(ng.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	m.mu.Lock()
	if g := m.grafos[gid]; g != nil {
		g.Generation = gen
	}
	m.mu.Unlock()
	if err := m.guardarGrafo(gid); err != nil {
		log.Printf("graph %s: couldn't save its generation: %v", shortID(gid), err)
	}
	log.Printf("graph %s (%s): forked into %d graph(s)", orig.Name, shortID(gid), n)
	return out, nil
}

// credencialesReescritas son las credenciales de un nodo del grafo gid,
// apuntadas al mismo nodo destino del grafo nuevo: mismo marcador, misma
// clave, otro destino y otro dueño. nil si no hay ninguna.
func (m *Manager) credencialesReescritas(gid, nuevo string, creds []credproxy.Credential) ([]credproxy.Credential, error) {
	if len(creds) == 0 {
		return nil, nil
	}
	m.mu.RLock()
	g := m.grafos[nuevo]
	var nombres []string
	if g != nil {
		nombres = g.SortedNodeNames()
	}
	m.mu.RUnlock()
	destino := map[string]string{} // ID virtual en el original -> nodo
	for _, nm := range nombres {
		destino[idVirtual(gid, nm)] = nm
	}
	out := make([]credproxy.Credential, 0, len(creds))
	for _, c := range creds {
		nodo, ok := destino[c.UpstreamMachine]
		if !ok || c.UpstreamOwner != gid {
			return nil, errors.New("a credential of the node doesn't point to a node of its graph")
		}
		c.UpstreamMachine = idVirtual(nuevo, nodo)
		c.UpstreamOwner = nuevo
		out = append(out, c)
	}
	return out, nil
}
