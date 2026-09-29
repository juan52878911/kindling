package machine

// Grafos de microVMs (docs/grafos.md, docs/grafos-diseno.md): un conjunto de
// máquinas con nombre, aristas declaradas y ciclo de vida atómico.
//
// DÓNDE VIVE: el grafo (nodos, aristas, generación) en
// $KLING_ROOT/store/graph/<id>.json, y en memoria bajo m.mu, porque el
// resolvedor de cada arista lo consulta con m.mu tomado en CADA conexión. Las
// claves de las aristas credential, cifradas aparte (<id>.secrets.enc, con la
// clave del almacén de credenciales y el grafo como dato autenticado). Cada
// máquina lleva además kling.graph=<id> y kling.graph.node=<nodo>, que solo
// pone el daemon (SetLabels, run, sandbox y fork las rechazan).
//
// QUIÉN ES QUIÉN: un nodo se identifica por (grafo, nodo), nunca por su
// máquina: la de un lazy aún no existe, la de un fork es otra. El manager
// guarda la máquina de cada nodo (GraphNode.MachineID) y la resuelve en cada
// conexión (grafo_red.go).
//
// CANDADOS: cada operación de grafo toma el cerrojo de ciclo de vida
// "graph:<id>" (no choca con los de máquinas, que son hexadecimal), y dentro
// los de sus máquinas, siempre en ese orden. Despertar un nodo también toma
// el del grafo: un freeze del grafo y un despertar no se cruzan a medias.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/durable"
)

// ErrNoGraph es un grafo que no existe (404 en la API).
var ErrNoGraph = errors.New("graph doesn't exist")

// Sustituibles en los tests: arrancar, despertar, congelar o montar la red de
// verdad pide KVM, firecracker y root. Se asignan en init: Run y Thaw acaban
// llamando (por el resolvedor de las credenciales) a código que las usa, y
// asignarlas en la declaración sería un ciclo de inicialización.
var (
	arrancarNodoGrafo  func(ctx context.Context, m *Manager, req api.RunRequest) (*api.Machine, error)
	despertarNodoGrafo func(ctx context.Context, m *Manager, id string) error
	congelarNodoGrafo  func(ctx context.Context, m *Manager, id string) error
	montarRedGrafo     = knet.SetGraph
	enviarGrafo        func(ctx context.Context, m *Manager, id string) error
)

func init() {
	enviarGrafo = enviarGrafoPlataforma
	arrancarNodoGrafo = func(ctx context.Context, m *Manager, req api.RunRequest) (*api.Machine, error) {
		return m.Run(ctx, req)
	}
	despertarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		_, err := m.Thaw(ctx, id)
		return err
	}
	congelarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		_, err := m.Freeze(ctx, id)
		return err
	}
}

// claveCerrojoGrafo es la clave del cerrojo de ciclo de vida de un grafo.
func claveCerrojoGrafo(id string) string { return "graph:" + id }

func (m *Manager) dirGrafos() string { return filepath.Join(m.root, "store", "graph") }

func (m *Manager) rutaGrafo(id string) string { return filepath.Join(m.dirGrafos(), id+".json") }

func (m *Manager) rutaSecretosGrafo(id string) string {
	return filepath.Join(m.dirGrafos(), id+".secrets.enc")
}

// idVirtual es el ID de un nodo: lo que llevan como UpstreamMachine las
// credenciales de sus aristas y lo que identifica sus sesiones. 32
// hexadecimales (una máquina tiene 16): no se confunde con ninguna.
func idVirtual(gid, nodo string) string {
	return hexSHA256("kling-graph-node\x00" + gid + "\x00" + nodo)[:32]
}

// sinEtiquetasGrafo quita kling.graph y kling.graph.* de un juego de
// etiquetas (las de una plantilla, que no es de ningún grafo).
func sinEtiquetasGrafo(labels map[string]string) map[string]string {
	hay := false
	for k := range labels {
		if api.IsGraphLabel(k) {
			hay = true
			break
		}
	}
	if !hay {
		return labels
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !api.IsGraphLabel(k) {
			out[k] = v
		}
	}
	return out
}

// copiaGrafo es una copia profunda (los mapas y listas no se comparten).
func copiaGrafo(g *api.Graph) *api.Graph {
	c := *g
	c.Nodes = make(map[string]api.GraphNode, len(g.Nodes))
	for k, n := range g.Nodes {
		n.Ports = append([]int(nil), n.Ports...)
		n.AllowDomains = append([]string(nil), n.AllowDomains...)
		n.Volumes = append([]api.VolumeAttachment(nil), n.Volumes...)
		n.Shares = append([]api.ShareSpec(nil), n.Shares...)
		if n.Labels != nil {
			l := make(map[string]string, len(n.Labels))
			for lk, lv := range n.Labels {
				l[lk] = lv
			}
			n.Labels = l
		}
		c.Nodes[k] = n
	}
	c.Edges = append([]api.GraphEdge(nil), g.Edges...)
	for i := range c.Edges {
		c.Edges[i].Secret = ""
	}
	return &c
}

// ── persistencia ─────────────────────────────────────────────────────────────

// cargarGrafos lee los grafos del almacén. Uno ilegible se salta con un aviso:
// sus máquinas siguen existiendo, y sin grafo sus aristas fallan cerradas.
func (m *Manager) cargarGrafos() {
	entradas, err := os.ReadDir(m.dirGrafos())
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grafos == nil {
		m.grafos = map[string]*api.Graph{}
	}
	for _, e := range entradas {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !api.KeyPattern.MatchString(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.dirGrafos(), e.Name()))
		if err != nil {
			log.Printf("graph %s: can't read it: %v", id, err)
			continue
		}
		var g api.Graph
		if err := json.Unmarshal(b, &g); err != nil || g.ID != id {
			log.Printf("graph %s: unreadable, skipped (its machines keep running; its edges fail closed)", id)
			continue
		}
		m.grafos[id] = &g
		for nombre, n := range g.Nodes {
			if n.MachineID != "" {
				m.virtuales.Store(n.MachineID, idVirtual(id, nombre))
			}
		}
	}
}

// guardarGrafo escribe el grafo id tal como está en memoria. Con el cerrojo
// del grafo tomado y SIN m.mu.
func (m *Manager) guardarGrafo(id string) error {
	m.mu.RLock()
	g := m.grafos[id]
	var b []byte
	var err error
	if g != nil {
		c := copiaGrafo(g)
		c.State = ""
		for k, n := range c.Nodes {
			n.State = ""
			c.Nodes[k] = n
		}
		b, err = json.MarshalIndent(c, "", "  ")
	}
	m.mu.RUnlock()
	if g == nil {
		return fmt.Errorf("%w: %s", ErrNoGraph, id)
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dirGrafos(), 0o700); err != nil {
		return err
	}
	return durable.Escribir(m.rutaGrafo(id), b, 0o600)
}

// guardarSecretosGrafo cifra las claves de las aristas credential del grafo
// id (por GraphEdge.Key). Sin claves, borra el fichero.
func (m *Manager) guardarSecretosGrafo(id string, secretos map[string]string) error {
	if len(secretos) == 0 {
		err := os.Remove(m.rutaSecretosGrafo(id))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(m.dirGrafos(), 0o700); err != nil {
		return err
	}
	sellado, err := m.sellar(secretos, "graph:"+id)
	if err != nil {
		return err
	}
	return escribirSellado(m.rutaSecretosGrafo(id), sellado)
}

// cargarSecretosGrafo descifra las claves del grafo id (nil sin fichero).
func (m *Manager) cargarSecretosGrafo(id string) (map[string]string, error) {
	sellado, err := os.ReadFile(m.rutaSecretosGrafo(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string]string
	if err := m.abrir(sellado, "graph:"+id, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ── consultas ────────────────────────────────────────────────────────────────

// grafoPorRefLocked resuelve un grafo por ID exacto, nombre o prefijo único
// de ID. Con m.mu tomado.
func (m *Manager) grafoPorRefLocked(ref string) (*api.Graph, error) {
	if g := m.grafos[ref]; g != nil {
		return g, nil
	}
	var porNombre, porPrefijo []*api.Graph
	for id, g := range m.grafos {
		if g.Name == ref {
			porNombre = append(porNombre, g)
		}
		if len(ref) >= 4 && strings.HasPrefix(id, ref) {
			porPrefijo = append(porPrefijo, g)
		}
	}
	switch {
	case len(porNombre) == 1:
		return porNombre[0], nil
	case len(porNombre) == 0 && len(porPrefijo) == 1:
		return porPrefijo[0], nil
	case len(porNombre)+len(porPrefijo) > 1:
		return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf("graph %q is ambiguous: use its id", ref)}
	}
	return nil, &api.StatusError{Code: 404, Message: fmt.Sprintf("graph %q doesn't exist", ref)}
}

// idGrafo resuelve ref a un ID.
func (m *Manager) idGrafo(ref string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, err := m.grafoPorRefLocked(ref)
	if err != nil {
		return "", err
	}
	return g.ID, nil
}

// vistaGrafoLocked es la copia que ve la API: con el estado de cada nodo y
// del grafo calculados ahora. Con m.mu tomado.
func (m *Manager) vistaGrafoLocked(g *api.Graph) *api.Graph {
	c := copiaGrafo(g)
	// Solo cuentan los nodos con máquina: un lazy sin instancia está como
	// debe, esperando a su primera conexión, y no hace parcial a nadie.
	corriendo, dormidos := 0, 0
	for nombre, n := range c.Nodes {
		if n.MachineID == "" {
			n.State = ""
			c.Nodes[nombre] = n
			continue
		}
		mc := m.byID[n.MachineID]
		switch {
		case mc == nil:
			n.State = "missing"
			dormidos++
		case mc.State == api.StateRunning:
			n.State = string(mc.State)
			corriendo++
		case mc.State == api.StateWarm:
			// En la API y el CLI el estado warm se llama frozen.
			n.State = "frozen"
			dormidos++
		default:
			n.State = string(mc.State)
			dormidos++
		}
		c.Nodes[nombre] = n
	}
	switch {
	case corriendo > 0 && dormidos == 0:
		c.State = api.GraphStateRunning
	case corriendo == 0:
		c.State = api.GraphStateFrozen
	default:
		c.State = api.GraphStatePartial
	}
	return c
}

// Graphs lista los grafos, por nombre.
func (m *Manager) Graphs() []*api.Graph {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*api.Graph, 0, len(m.grafos))
	for _, g := range m.grafos {
		out = append(out, m.vistaGrafoLocked(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Graph devuelve un grafo por ID, nombre o prefijo de ID.
func (m *Manager) Graph(ref string) (*api.Graph, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, err := m.grafoPorRefLocked(ref)
	if err != nil {
		return nil, err
	}
	return m.vistaGrafoLocked(g), nil
}

// ── up ───────────────────────────────────────────────────────────────────────

// secretosDeAristas comprueba que cada arista credential trae su clave y que
// no sobra ninguna, y devuelve el mapa limpio.
func secretosDeAristas(g *api.Graph, secretos map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range g.Edges {
		if e.Kind != api.GraphEdgeCredential {
			continue
		}
		s := secretos[e.Key()]
		if s == "" {
			return nil, fmt.Errorf("credential edge %s -> %s (%s) has no secret (secrets[%q])", e.From, e.To, e.Env, e.Key())
		}
		if len(s) > credproxy.MaxSecret {
			return nil, fmt.Errorf("credential edge %s -> %s: the secret is longer than %d bytes", e.From, e.To, credproxy.MaxSecret)
		}
		out[e.Key()] = s
	}
	for k := range secretos {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("secret %q is not for any credential edge of the graph", k)
		}
	}
	return out, nil
}

// memoriaDeNodo es la memoria con la que arrancaría n: la pedida, la de su
// plantilla o la de siempre.
func (m *Manager) memoriaDeNodo(n api.GraphNode) int {
	if n.MemMiB > 0 {
		return n.MemMiB
	}
	if n.From != "" {
		if s, _, err := m.loadSnapshotCached(n.From); err == nil && s.MemMiB > 0 {
			return s.MemMiB
		}
	}
	return 256
}

// GraphUp crea el grafo g y arranca sus nodos eager. secretos son las claves
// de sus aristas credential (por GraphEdge.Key). Todo o nada: si un nodo no
// arranca, se borra lo creado.
func (m *Manager) GraphUp(ctx context.Context, g api.Graph, secretos map[string]string) (out *api.Graph, errOut error) {
	if err := api.ValidateGraph(&g); err != nil {
		return nil, &api.StatusError{Code: 400, Message: err.Error()}
	}
	sec, err := secretosDeAristas(&g, secretos)
	if err != nil {
		return nil, &api.StatusError{Code: 400, Message: err.Error()}
	}
	for i := range g.Edges {
		g.Edges[i].Secret = ""
	}

	// Antes de arrancar el primero: cuántas máquinas y cuánta memoria piden
	// los eager. Sin esto, el tercer nodo fallaría con 507 después de haber
	// arrancado los dos primeros para nada.
	eager, memoria := 0, 0
	for _, n := range g.Nodes {
		if n.Wake == api.GraphWakeEager {
			eager++
			memoria += m.memoriaDeNodo(n)
		}
	}
	m.mu.RLock()
	total := len(m.byID)
	m.mu.RUnlock()
	if tope := maxMachines(); total+eager > tope {
		return nil, &api.StatusError{Code: api.StatusMachineLimit, Message: fmt.Sprintf(
			"graph %s needs %d machines and there are %d of %d: %s (KLING_MAX_MACHINES)", g.Name, eager, total, tope, machineLimitMarkText)}
	}
	if memoria > 0 {
		soltar, err := m.reserveMemory(memoria, "")
		if err != nil {
			return nil, fmt.Errorf("graph %s needs %d MiB for its eager nodes: %w", g.Name, memoria, err)
		}
		soltar()
	}

	g.ID = newID()
	g.CreatedAt = time.Now().UTC()
	defer m.lock(claveCerrojoGrafo(g.ID))()

	m.mu.Lock()
	for _, otro := range m.grafos {
		if otro.Name == g.Name {
			m.mu.Unlock()
			return nil, &api.StatusError{Code: 409, Message: fmt.Sprintf("graph %q already exists (kling graph rm %s)", g.Name, g.Name)}
		}
	}
	if m.grafos == nil {
		m.grafos = map[string]*api.Graph{}
	}
	m.grafos[g.ID] = &g
	m.mu.Unlock()
	defer func() {
		if errOut != nil {
			m.eliminarGrafo(context.WithoutCancel(ctx), g.ID)
		}
	}()
	if err := m.guardarSecretosGrafo(g.ID, sec); err != nil {
		return nil, err
	}
	if err := m.guardarGrafo(g.ID); err != nil {
		return nil, err
	}
	for _, nombre := range g.SortedNodeNames() {
		if g.Nodes[nombre].Wake != api.GraphWakeEager {
			continue
		}
		if err := m.instanciarNodo(ctx, g.ID, nombre, ""); err != nil {
			return nil, fmt.Errorf("graph %s: node %s: %w", g.Name, nombre, err)
		}
	}
	// Las aristas cuando ya existen todos los eager: una credencial a un nodo
	// que aún no ha arrancado se comprobaría contra nada.
	for _, nombre := range g.SortedNodeNames() {
		if err := m.conectarNodo(ctx, g.ID, nombre, nil); err != nil {
			return nil, fmt.Errorf("graph %s: node %s: %w", g.Name, nombre, err)
		}
	}
	log.Printf("graph %s (%s): up with %d node(s), %d eager", g.Name, shortID(g.ID), len(g.Nodes), eager)
	return m.Graph(g.ID)
}

// peticionDeNodo es el RunRequest que arranca el nodo nombre del grafo g.
func peticionDeNodo(g *api.Graph, nombre, forkOf string) api.RunRequest {
	n := g.Nodes[nombre]
	etiquetas := map[string]string{api.LabelGraph: g.ID, api.LabelGraphNode: nombre}
	if len(n.Ports) > 0 {
		ps := make([]string, len(n.Ports))
		for i, p := range n.Ports {
			ps[i] = strconv.Itoa(p)
		}
		etiquetas[api.LabelPorts] = strings.Join(ps, ",")
	}
	if forkOf != "" {
		etiquetas[api.LabelForkOf] = forkOf
	}
	return api.RunRequest{
		Name:         g.Name + "-" + nombre,
		Image:        n.Image,
		From:         n.From,
		VCPUs:        n.VCPUs,
		MemMiB:       n.MemMiB,
		Egress:       n.Egress,
		AllowDomains: n.AllowDomains,
		TTLSeconds:   n.IdleFreezeSeconds,
		Volumes:      n.Volumes,
		Shares:       n.Shares,
		AllowExec:    n.AllowExec,
		Labels:       api.MergeLabels(n.Labels, etiquetas),
	}
}

// instanciarNodo arranca la máquina del nodo nombre y la anota en el grafo.
// Con el cerrojo del grafo tomado. forkOf es el ID de la máquina original en
// un fork.
func (m *Manager) instanciarNodo(ctx context.Context, gid, nombre, forkOf string) error {
	m.mu.RLock()
	g := m.grafos[gid]
	var req api.RunRequest
	if g != nil {
		req = peticionDeNodo(g, nombre, forkOf)
	}
	m.mu.RUnlock()
	if g == nil {
		return fmt.Errorf("%w: %s", ErrNoGraph, gid)
	}
	mc, err := arrancarNodoGrafo(ctx, m, req)
	if err != nil {
		return err
	}
	m.mu.Lock()
	g = m.grafos[gid]
	if g != nil {
		n := g.Nodes[nombre]
		n.MachineID = mc.ID
		g.Nodes[nombre] = n
	}
	m.mu.Unlock()
	if g == nil {
		// El grafo se borró mientras arrancaba: la máquina no es de nadie.
		_ = m.Remove(mc.ID)
		return fmt.Errorf("%w: %s (removed while node %s was starting)", ErrNoGraph, gid, nombre)
	}
	m.virtuales.Store(mc.ID, idVirtual(gid, nombre))
	return m.guardarGrafo(gid)
}

// ── freeze, thaw, rm ─────────────────────────────────────────────────────────

// maquinasDeGrafo son las máquinas instanciadas del grafo, por nodo en orden.
func (m *Manager) maquinasDeGrafo(gid string) (nombres, ids []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g := m.grafos[gid]
	if g == nil {
		return nil, nil
	}
	for _, n := range g.SortedNodeNames() {
		if id := g.Nodes[n].MachineID; id != "" {
			nombres = append(nombres, n)
			ids = append(ids, id)
		}
	}
	return nombres, ids
}

// GraphFreeze congela todos los nodos que corren (o están pausados). Sigue
// con los demás si uno falla, y devuelve el primer error: el grafo queda
// partial y se ve.
func (m *Manager) GraphFreeze(ctx context.Context, ref string) (*api.Graph, error) {
	gid, err := m.idGrafo(ref)
	if err != nil {
		return nil, err
	}
	defer m.lock(claveCerrojoGrafo(gid))()
	nombres, ids := m.maquinasDeGrafo(gid)
	var primero error
	for i, id := range ids {
		mc, ok := m.Get(id)
		if !ok || (mc.State != api.StateRunning && mc.State != api.StatePaused) {
			continue
		}
		if err := congelarNodoGrafo(ctx, m, id); err != nil && primero == nil {
			primero = fmt.Errorf("node %s: %w", nombres[i], err)
		}
	}
	g, gerr := m.Graph(gid)
	if primero != nil {
		return g, primero
	}
	return g, gerr
}

// GraphThaw despierta todos los nodos instanciados (congelados o pausados).
// Un lazy sin instancia sigue sin ella.
func (m *Manager) GraphThaw(ctx context.Context, ref string) (*api.Graph, error) {
	gid, err := m.idGrafo(ref)
	if err != nil {
		return nil, err
	}
	defer m.lock(claveCerrojoGrafo(gid))()
	nombres, ids := m.maquinasDeGrafo(gid)
	var primero error
	for i, id := range ids {
		mc, ok := m.Get(id)
		if !ok || (mc.State != api.StateWarm && mc.State != api.StatePaused) {
			continue
		}
		if err := despertarNodoGrafo(ctx, m, id); err != nil && primero == nil {
			primero = fmt.Errorf("node %s: %w", nombres[i], err)
		}
	}
	g, gerr := m.Graph(gid)
	if primero != nil {
		return g, primero
	}
	return g, gerr
}

// GraphRemove borra el grafo y sus máquinas.
func (m *Manager) GraphRemove(ctx context.Context, ref string) error {
	gid, err := m.idGrafo(ref)
	if err != nil {
		return err
	}
	defer m.lock(claveCerrojoGrafo(gid))()
	return m.eliminarGrafo(ctx, gid)
}

// eliminarGrafo es GraphRemove con el cerrojo del grafo ya tomado (o de un
// grafo que aún no conoce nadie). Primero lo quita de memoria, para que
// ninguna arista resuelva a nada mientras se borran sus máquinas.
func (m *Manager) eliminarGrafo(ctx context.Context, gid string) error {
	m.mu.Lock()
	g := m.grafos[gid]
	delete(m.grafos, gid)
	m.mu.Unlock()
	if g == nil {
		return fmt.Errorf("%w: %s", ErrNoGraph, gid)
	}
	var primero error
	var plantillas []string
	for _, nombre := range g.SortedNodeNames() {
		n := g.Nodes[nombre]
		if n.From != "" {
			plantillas = append(plantillas, n.From)
		}
		if n.MachineID == "" {
			continue
		}
		if _, ok := m.Get(n.MachineID); ok {
			if err := m.Remove(n.MachineID); err != nil && primero == nil {
				primero = fmt.Errorf("node %s: %w", nombre, err)
			}
		}
		m.virtuales.Delete(n.MachineID)
	}
	for _, p := range []string{m.rutaGrafo(gid), m.rutaSecretosGrafo(gid)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) && primero == nil {
			primero = err
		}
	}
	// Las plantillas temporales de un fork del que salió este grafo: si ya
	// no las usa nadie, fuera ahora (el vigilante lo haría en su siguiente
	// vuelta de todos modos, ver barrerForks).
	for _, p := range plantillas {
		if esSnapshotDeFork(m.snapDir(p)) && !m.forkEnUso(p) {
			if err := m.removeSnapshot(p, false); err != nil {
				log.Printf("graph %s: fork template %s: %v", shortID(gid), p, err)
			}
		}
	}
	log.Printf("graph %s (%s): removed", g.Name, shortID(gid))
	return primero
}

// ── red y credenciales de un nodo ────────────────────────────────────────────

// especRedGrafoLocked dice qué hay que montar en el netns de mc si es la
// máquina de un nodo con aristas salientes. Con m.mu tomado.
func (m *Manager) especRedGrafoLocked(mc *api.Machine) (knet.GraphSpec, bool) {
	gid, nodo := mc.Labels[api.LabelGraph], mc.Labels[api.LabelGraphNode]
	if gid == "" || nodo == "" {
		return knet.GraphSpec{}, false
	}
	g := m.grafos[gid]
	if g == nil || g.Nodes[nodo].MachineID != mc.ID {
		return knet.GraphSpec{}, false
	}
	egress, _ := knet.ParseEgress(mc.Egress)
	spec := knet.GraphSpec{Egress: egress, Domains: mc.AllowDomains, AuditPath: m.credAuditPath(mc.ID)}
	hay := false
	for _, e := range g.Edges {
		if e.From != nodo {
			continue
		}
		switch e.Kind {
		case api.GraphEdgeLink:
			spec.Links = append(spec.Links, knet.LinkSpec{
				Host:    e.Host(),
				Port:    e.Port,
				Target:  idVirtual(gid, e.To),
				Resolve: m.resolverEnlace(mc.ID, gid, nodo, e.To, e.Port),
			})
			hay = true
		case api.GraphEdgeCredential:
			spec.Credentials = true
			hay = true
		}
	}
	return spec, hay
}

// especRedGrafo es especRedGrafoLocked tomando m.mu.
func (m *Manager) especRedGrafo(mc *api.Machine) (knet.GraphSpec, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.especRedGrafoLocked(mc)
}

// conectarNodo monta las aristas salientes del nodo nombre en su máquina (si
// la tiene y corre): la red y, para las credential, sus credenciales. creds,
// si no es nil, son las credenciales EXACTAS a entregar (un fork: los mismos
// marcadores que el invitado ya tiene en memoria); nil las genera de las
// claves del grafo, con marcadores nuevos.
func (m *Manager) conectarNodo(ctx context.Context, gid, nombre string, creds []credproxy.Credential) error {
	m.mu.RLock()
	g := m.grafos[gid]
	var id string
	var aristas []api.GraphEdge
	if g != nil {
		id = g.Nodes[nombre].MachineID
		for _, e := range g.Edges {
			if e.From == nombre && e.Kind == api.GraphEdgeCredential {
				aristas = append(aristas, e)
			}
		}
	}
	m.mu.RUnlock()
	if g == nil {
		return fmt.Errorf("%w: %s", ErrNoGraph, gid)
	}
	if id == "" {
		return nil // lazy sin instancia: se conecta al instanciarlo
	}
	mc, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("machine %s of node %s doesn't exist", shortID(id), nombre)
	}
	spec, hay := m.especRedGrafo(mc)
	if !hay {
		return nil
	}
	if mc.State != api.StateRunning {
		return nil // lo hará su thaw (Thaw monta las aristas)
	}
	if err := montarRedGrafo(knet.Plan(mc.NetIndex, mc.ID), spec); err != nil {
		return fmt.Errorf("mounting its graph edges: %w", err)
	}
	// En macOS, las aristas a su kling-vz (en Linux ya está todo montado).
	if err := enviarGrafo(ctx, m, id); err != nil {
		return err
	}
	if len(aristas) == 0 {
		return nil
	}
	if creds != nil {
		return m.entregarCredencialesExactas(ctx, id, creds)
	}
	secretos, err := m.cargarSecretosGrafo(gid)
	if err != nil {
		return err
	}
	var specs []api.CredentialSpec
	for _, e := range aristas {
		s := secretos[e.Key()]
		if s == "" {
			return fmt.Errorf("credential edge %s -> %s (%s): its secret is gone from the store", e.From, e.To, e.Env)
		}
		specs = append(specs, especCredencialArista(gid, e, s))
	}
	return m.entregarCredencialesNodo(ctx, id, specs)
}

// especCredencialArista es la credencial Postgres de una arista credential:
// su dominio es <destino>.graph (el resolver del nodo lo desvía al proxy),
// su "máquina" es el ID del nodo destino y su dueño el grafo.
func especCredencialArista(gid string, e api.GraphEdge, secreto string) api.CredentialSpec {
	return api.CredentialSpec{
		Domain: e.Host(), Env: e.Env, Secret: secreto,
		Type: credproxy.KindPostgres, Port: e.Port, User: e.User, Database: e.Database,
		UpstreamTLS:     credproxy.UpstreamTLSDisable,
		UpstreamMachine: idVirtual(gid, e.To),
		UpstreamOwner:   gid,
	}
}

// entregarCredencialesNodo es SetCredentials para el nodo de un grafo: sin
// exigir egress allowlist (conectarNodo ya le montó resolver y reglas).
func (m *Manager) entregarCredencialesNodo(ctx context.Context, id string, specs []api.CredentialSpec) error {
	defer m.lock(id)()
	cur, ok := m.Get(id)
	if !ok || cur.State != api.StateRunning {
		return fmt.Errorf("machine %s is not running", shortID(id))
	}
	m.mu.RLock()
	sock := m.socket[id]
	m.mu.RUnlock()
	if sock == "" {
		return fmt.Errorf("no socket for %s", id)
	}
	creds, _, err := m.entregarCredenciales(ctx, id, knet.Plan(cur.NetIndex, cur.ID), fc.New(sock), specs)
	if err != nil {
		return err
	}
	m.anotarCredenciales(id, creds)
	return nil
}

// entregarCredencialesExactas entrega creds tal cual (marcadores incluidos):
// guarda el almacén, repone MMDS y registra en el proxy.
func (m *Manager) entregarCredencialesExactas(ctx context.Context, id string, creds []credproxy.Credential) error {
	defer m.lock(id)()
	cur, ok := m.Get(id)
	if !ok || cur.State != api.StateRunning {
		return fmt.Errorf("machine %s is not running", shortID(id))
	}
	m.mu.RLock()
	sock := m.socket[id]
	m.mu.RUnlock()
	if sock == "" {
		return fmt.Errorf("no socket for %s", id)
	}
	if err := credproxy.ValidarCredenciales(creds); err != nil {
		return err
	}
	if err := m.guardarCredenciales(id, creds); err != nil {
		return err
	}
	c := fc.New(sock)
	if err := ponerMarcadoresMMDS(ctx, c, creds); err != nil {
		return err
	}
	if err := registrarCredenciales(ctx, c, knet.Plan(cur.NetIndex, cur.ID), creds, m.credAuditPath(id), m.resolverCopia(id)); err != nil {
		return err
	}
	m.anotarCredenciales(id, creds)
	return nil
}

// anotarCredenciales deja en la máquina lo que `kling inspect` dice de sus
// credenciales.
func (m *Manager) anotarCredenciales(id string, creds []credproxy.Credential) {
	m.mu.Lock()
	if live := m.byID[id]; live != nil {
		live.CredentialDomains = dominiosDe(creds)
		live.CredentialAnyDatabase = anyDatabaseDe(creds)
		m.persist()
	}
	m.mu.Unlock()
}
