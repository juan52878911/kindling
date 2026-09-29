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
// Antes de (1), los nodos congelados se despiertan (y se vuelven a congelar
// al final) y los nodos con volúmenes los sueltan con el invitado en marcha:
// ver snapshotConsistente.
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
	"sort"
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
	// Soltar y devolver los volúmenes de un nodo: hablar con su agente.
	soltarVolumenesNodo   func(ctx context.Context, m *Manager, id string) error
	devolverVolumenesNodo func(ctx context.Context, m *Manager, id string) error
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
	soltarVolumenesNodo = func(_ context.Context, m *Manager, id string) error {
		mc, ok := m.Get(id)
		if !ok {
			return fmt.Errorf("machine %s no longer exists", shortID(id))
		}
		return m.releaseVolumes(mc)
	}
	devolverVolumenesNodo = func(_ context.Context, m *Manager, id string) error {
		mc, ok := m.Get(id)
		if !ok {
			return fmt.Errorf("machine %s no longer exists", shortID(id))
		}
		return m.acquireVolumes(mc)
	}
}

// nodoMaquina es un nodo instanciado con su máquina.
type nodoMaquina struct {
	nodo, id  string
	estado    api.State
	volumenes []api.VolumeAttachment
}

// nodosParaVolcar son los nodos con máquina del grafo, que tienen que estar
// corriendo, pausados o congelados. Los lazy sin instancia no se vuelcan:
// siguen siendo su plantilla. Todo lo que se rechaza se rechaza aquí, antes
// de tocar ninguna máquina.
//
// Un nodo congelado no tiene VMM que volcar: snapshotConsistente lo despierta,
// lo vuelca con los demás y lo vuelve a congelar. Un nodo con volúmenes los
// suelta antes de la pausa (hablando con su agente, como Commit); uno que YA
// está pausado no contesta, así que con volúmenes se rechaza con el consejo
// de despertarlo.
//
// Un grafo con aristas share no se vuelca: sus nodos llevan montada una
// carpeta viva del host, y la memoria volcada despertaría en cada instancia
// con un montaje que no le corresponde (lo mismo que `kling commit` de una
// máquina con -share). Ver docs/grafos.md.
func (m *Manager) nodosParaVolcar(gid string) ([]nodoMaquina, error) {
	m.mu.RLock()
	var carpetas bool
	if g := m.grafos[gid]; g != nil {
		for _, e := range g.Edges {
			carpetas = carpetas || e.Kind == api.GraphEdgeShare
		}
	}
	m.mu.RUnlock()
	if carpetas {
		return nil, &api.StatusError{Code: 409, Message: "the graph has share edges: its nodes have a live host folder mounted, " +
			"and a snapshot would carry that mount to every instance restored from it; snapshot and fork don't take them in this version " +
			"(freeze and thaw work)"}
	}
	nombres, ids := m.maquinasDeGrafo(gid, false)
	var out []nodoMaquina
	for i, id := range ids {
		mc, ok := m.Get(id)
		if !ok {
			return nil, fmt.Errorf("the machine of node %s no longer exists", nombres[i])
		}
		switch mc.State {
		case api.StateRunning, api.StateWarm:
		case api.StatePaused:
			if len(mc.Volumes) > 0 {
				return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf(
					"node %s is paused and has volumes: releasing them needs its guest agent, and a paused guest doesn't answer; "+
						"resume it first (kling graph thaw), then snapshot", nombres[i])}
			}
		default:
			return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf("node %s is %s", nombres[i], mc.State)}
		}
		out = append(out, nodoMaquina{nodo: nombres[i], id: id, estado: mc.State,
			volumenes: append([]api.VolumeAttachment(nil), mc.Volumes...)})
	}
	if len(out) == 0 {
		return nil, &api.StatusError{Code: 409, Message: "no node of the graph has a machine yet: nothing to snapshot"}
	}
	return out, nil
}

// snapshotConsistente vuelca los nodos del grafo gid en el mismo instante,
// uno en la plantilla nombre(nodo) cada uno, y devuelve las plantillas por
// nodo. Con el cerrojo del grafo tomado.
//
// Antes del instante: (0) despierta los congelados, en orden de arranque, y
// (0b) pide a los nodos con volúmenes que los suelten, con el invitado aún en
// marcha (lo mismo que Commit de una máquina: la caché de ext4 no puede ir en
// la memoria volcada de un disco que no viaja con ella). Después: reanuda los
// que pausó, les devuelve los volúmenes y vuelve a congelar los que despertó,
// en orden de parada. Lo mismo si algo falla: el grafo queda como estaba.
func (m *Manager) snapshotConsistente(ctx context.Context, gid string, nombre func(nodo string) string) (map[string]string, error) {
	nodos, err := m.nodosParaVolcar(gid)
	if err != nil {
		return nil, err
	}
	// Deshacer no es opcional aunque quien lo pidió se haya ido.
	limpio := context.WithoutCancel(ctx)
	// El orden de parada (el de freeze): cada nodo antes que los nodos de los
	// que depende. nodos va en el de arranque.
	_, orden := m.maquinasDeGrafo(gid, true)
	posicion := map[string]int{}
	for i, id := range orden {
		posicion[id] = i
	}
	porParada := append([]nodoMaquina(nil), nodos...)
	sort.SliceStable(porParada, func(i, j int) bool { return posicion[porParada[i].id] < posicion[porParada[j].id] })

	pausadas, soltadas, despertadas := map[string]bool{}, map[string]bool{}, map[string]bool{}
	// deshacer devuelve cada nodo a como estaba: reanudar antes de devolver
	// los volúmenes (un invitado pausado no contesta) y devolverlos antes de
	// congelar (Freeze los vuelve a soltar, como con cualquier máquina).
	deshacer := func() {
		for _, n := range nodos {
			if !pausadas[n.id] {
				continue
			}
			if err := reanudarNodoGrafo(limpio, m, n.id); err != nil {
				log.Printf("graph %s: couldn't resume %s after the snapshot: %v", shortID(gid), shortID(n.id), err)
			}
		}
		for _, n := range nodos {
			if !soltadas[n.id] {
				continue
			}
			if err := devolverVolumenesNodo(limpio, m, n.id); err != nil {
				log.Printf("warning: graph %s: node %s ended up without its volumes after the snapshot: %v", shortID(gid), n.nodo, err)
			}
		}
		for _, n := range porParada {
			if !despertadas[n.id] {
				continue
			}
			if mc, ok := m.Get(n.id); !ok || (mc.State != api.StateRunning && mc.State != api.StatePaused) {
				continue
			}
			if err := congelarNodoGrafo(limpio, m, n.id); err != nil {
				log.Printf("graph %s: couldn't freeze node %s again after the snapshot (it stays running): %v", shortID(gid), n.nodo, err)
			}
		}
	}
	estado := map[string]api.State{}
	// (0) despertar los congelados, en orden de arranque.
	for _, n := range nodos {
		estado[n.id] = n.estado
		if n.estado != api.StateWarm {
			continue
		}
		if err := despertarNodoGrafo(ctx, m, n.id); err != nil {
			deshacer()
			return nil, fmt.Errorf("thawing frozen node %s for the snapshot: %w", n.nodo, err)
		}
		despertadas[n.id] = true
		estado[n.id] = api.StateRunning
	}
	// (0b) soltar los volúmenes, con los invitados en marcha. Se anota antes
	// de pedirlo: una petición fallida pudo aplicarse, y devolver un volumen
	// que no se soltó es inocuo.
	for _, n := range nodos {
		if len(n.volumenes) == 0 {
			continue
		}
		soltadas[n.id] = true
		if err := soltarVolumenesNodo(ctx, m, n.id); err != nil {
			deshacer()
			return nil, fmt.Errorf("releasing the volumes of node %s: %w", n.nodo, err)
		}
	}
	// (1) pausar todos los que corren, en orden de parada; se reanudan en el
	// de arranque.
	for _, n := range porParada {
		if estado[n.id] != api.StateRunning {
			continue
		}
		if err := pausarNodoGrafo(ctx, m, n.id); err != nil {
			deshacer()
			return nil, fmt.Errorf("pausing node %s: %w", n.nodo, err)
		}
		pausadas[n.id] = true
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
			deshacer()
			return nil, fmt.Errorf("snapshot of node %s: %w", n.nodo, err)
		}
		hechas[n.nodo] = name
	}
	// (4) reanudar los que pausamos (los que ya estaban pausados, así siguen),
	// devolverles los volúmenes y volver a congelar los que despertamos.
	deshacer()
	return hechas, nil
}

// volumenesDeFork rechaza el fork de un grafo con un volumen en escritura en
// cualquiera de sus nodos, instanciado o no: un ext4 no admite dos
// escritores, y la primera copia ya chocaría con el original. En solo
// lectura se comparte, como en `kling sandbox fork`.
func volumenesDeFork(g *api.Graph, nodos []nodoMaquina) error {
	rechazo := func(nodo, vol string) error {
		return &api.StatusError{Code: 409, Message: fmt.Sprintf(
			"node %s has volume %q mounted read-write, and only one machine at a time can write to a volume; "+
				"mount it read-only (:ro) to fork the graph, or snapshot it (kling graph snapshot) and start each copy with its own volume",
			nodo, vol)}
	}
	for _, n := range nodos {
		for _, v := range n.volumenes {
			if !v.ReadOnly {
				return rechazo(n.nodo, v.Name)
			}
		}
	}
	for _, nombre := range g.SortedNodeNames() {
		for _, v := range g.Nodes[nombre].Volumes {
			if !v.ReadOnly {
				return rechazo(nombre, v.Name)
			}
		}
	}
	return nil
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
	if err := volumenesDeFork(orig, nodos); err != nil {
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
		creados = append(creados, ng.ID)
		g, err := m.crearCopiaFork(ctx, gid, ng, secretos, plantillas, origDe, credsPorNodo)
		if err != nil {
			return nil, fmt.Errorf("fork %d of %d: %w", i+1, n, err)
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

// crearCopiaFork registra el grafo ng (una copia de gid), arranca los nodos
// que tenían máquina en el original desde sus plantillas y les monta las
// aristas con las credenciales del original reescritas. Con el cerrojo del
// grafo nuevo: ya se ve en la lista mientras arranca.
func (m *Manager) crearCopiaFork(ctx context.Context, gid string, ng *api.Graph, secretos map[string]string,
	plantillas, origDe map[string]string, credsPorNodo map[string][]credproxy.Credential) (*api.Graph, error) {
	defer m.lock(claveCerrojoGrafo(ng.ID))()
	m.mu.Lock()
	if m.grafos == nil {
		m.grafos = map[string]*api.Graph{}
	}
	m.grafos[ng.ID] = ng
	m.mu.Unlock()
	if err := m.guardarSecretosGrafo(ng.ID, secretos); err != nil {
		return nil, err
	}
	if err := m.guardarGrafo(ng.ID); err != nil {
		return nil, err
	}
	// En el orden de depends, como up (todos salen del mismo instante, así
	// que cada dependencia ya estaba lista cuando se volcó).
	nombres, err := ng.StartOrder()
	if err != nil {
		nombres = ng.SortedNodeNames()
	}
	for _, nombreNodo := range nombres {
		if plantillas[nombreNodo] == "" {
			continue // lazy sin instancia en el original: sigue así
		}
		if err := m.instanciarNodo(ctx, ng.ID, nombreNodo, origDe[nombreNodo]); err != nil {
			return nil, fmt.Errorf("node %s: %w", nombreNodo, err)
		}
	}
	for _, nombreNodo := range nombres {
		creds, err := m.credencialesReescritas(gid, ng.ID, credsPorNodo[nombreNodo])
		if err != nil {
			return nil, err
		}
		if err := m.conectarNodo(ctx, ng.ID, nombreNodo, creds); err != nil {
			return nil, fmt.Errorf("node %s: %w", nombreNodo, err)
		}
	}
	return m.Graph(ng.ID)
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
