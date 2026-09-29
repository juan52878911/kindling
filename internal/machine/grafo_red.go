package machine

// Las aristas de un grafo en cada conexión (docs/grafos.md).
//
// EL RESOLVEDOR: el proxy de enlace de un nodo (pkg/credproxy/enlace.go) y el
// de Postgres de una arista credential preguntan aquí, en CADA conexión, a
// dónde va. comprobarAristaLocked mira bajo m.mu, igual que
// comprobarCopiaLocked para kling db attach: la máquina de origen existe con
// ese ID exacto, lleva kling.graph=<grafo> y kling.graph.node=<origen> y es la
// máquina de ese nodo en el grafo; el grafo tiene la arista (origen -> destino,
// tipo, puerto); la máquina del destino es la que el grafo dice, lleva sus
// mismas etiquetas y expone el puerto (kling.ports). Solo entonces da la IP de
// su netns (direccionCopiaLocked). Nada se fija al crear ni se cachea: un
// fork tiene otro ID de grafo y otras máquinas, así que sus aristas no
// resuelven nunca al original.
//
// EL DESPERTAR: si el destino está congelado o pausado, o es un lazy sin
// instancia, la conexión espera (el invitado solo ve latencia) a que un
// despertar lo ponga en marcha y a que su puerto conteste. Un solo despertar
// en vuelo por nodo (los demás esperan el mismo), con un tope de esperas; por
// encima, la conexión se rechaza y la auditoría dice busy. Si no cabe en el
// host, no_capacity.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

const (
	// plazoDespertar acota un despertar (thaw o arranque de un lazy) y la
	// espera a su puerto.
	plazoDespertar = 2 * time.Minute
	// despertarMaxEspera: conexiones esperando al MISMO despertar.
	despertarMaxEspera = 64
)

func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// esperarPuertoGrafo espera a que addr acepte conexiones: un nodo recién
// despertado tarda un poco en volver a escuchar. Sustituible en los tests.
var esperarPuertoGrafo = func(ctx context.Context, addr string) error {
	var d net.Dialer
	for {
		intento, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		c, err := d.DialContext(intento, "tcp", addr)
		cancel()
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("port %s didn't answer after waking the node: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// despertar es un despertar en vuelo de un nodo.
type despertar struct {
	hecho     chan struct{}
	err       error
	esperando int
}

// resolverEnlace es el Resolve del proxy de la arista link origen -> hacia:P.
func (m *Manager) resolverEnlace(origen, gid, desde, hacia string, port int) credproxy.ResolveLinkFunc {
	return func(ctx context.Context) (string, string, error) {
		return m.resolverArista(ctx, origen, gid, desde, hacia, port, api.GraphEdgeLink)
	}
}

// aristaVirtual dice si id (el UpstreamMachine de una credencial de la
// máquina agente, con dueño owner) es el ID de un nodo del grafo del agente,
// y de cuál: el grafo, el nodo del agente y el destino.
func (m *Manager) aristaVirtual(agente, id, owner string) (gid, desde, hacia string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ag := m.byID[agente]
	if ag == nil {
		return "", "", "", false
	}
	gid, desde = ag.Labels[api.LabelGraph], ag.Labels[api.LabelGraphNode]
	if gid == "" || gid != owner {
		return "", "", "", false
	}
	g := m.grafos[gid]
	if g == nil {
		return "", "", "", false
	}
	for nombre := range g.Nodes {
		if idVirtual(gid, nombre) == id {
			return gid, desde, nombre, true
		}
	}
	return "", "", "", false
}

// resolverArista da la dirección del puerto port del nodo hacia para una
// conexión del nodo desde (máquina origen) por una arista de tipo kind,
// despertando al destino si hace falta.
func (m *Manager) resolverArista(ctx context.Context, origen, gid, desde, hacia string, port int, kind string) (addr, id string, err error) {
	despertado := false
	for intento := 0; ; intento++ {
		m.mu.RLock()
		dest, dormido, err := m.comprobarAristaLocked(origen, gid, desde, hacia, port, kind)
		if err == nil && !dormido {
			addr, err = direccionCopiaLocked(dest, port)
			id = dest.ID
		}
		m.mu.RUnlock()
		if err != nil {
			return "", "", err
		}
		if !dormido {
			break
		}
		if intento > 0 {
			return "", "", fmt.Errorf("node %s didn't stay awake", hacia)
		}
		if err := m.asegurarNodo(ctx, gid, hacia); err != nil {
			return "", "", err
		}
		despertado = true
	}
	if despertado {
		if err := esperarPuertoGrafo(ctx, addr); err != nil {
			return "", "", err
		}
	}
	return addr, id, nil
}

// comprobarAristaLocked es la puerta de cada conexión por una arista. Con
// m.mu tomado. Devuelve la máquina del destino si corre, o dormido=true si
// hay que despertarlo (congelado, pausado o lazy sin instancia).
func (m *Manager) comprobarAristaLocked(origen, gid, desde, hacia string, port int, kind string) (dest *api.Machine, dormido bool, err error) {
	src := m.byID[origen]
	if src == nil {
		return nil, false, fmt.Errorf("source machine %s no longer exists", shortID(origen))
	}
	if src.Labels[api.LabelGraph] != gid || src.Labels[api.LabelGraphNode] != desde {
		return nil, false, fmt.Errorf("machine %s is not node %s of graph %s", src.Name, desde, shortID(gid))
	}
	g := m.grafos[gid]
	if g == nil {
		return nil, false, fmt.Errorf("graph %s no longer exists", shortID(gid))
	}
	if g.Nodes[desde].MachineID != origen {
		return nil, false, fmt.Errorf("machine %s is not the machine of node %s", src.Name, desde)
	}
	arista := false
	for _, e := range g.Edges {
		if e.From == desde && e.To == hacia && e.Kind == kind && e.Port == port {
			arista = true
			break
		}
	}
	if !arista {
		return nil, false, fmt.Errorf("graph %s has no %s edge %s -> %s:%d", g.Name, kind, desde, hacia, port)
	}
	// Defensa en profundidad para grafos guardados antes de que la validación
	// lo rechazara: nunca se marca al puerto del agente de invitado.
	if port == api.GuestPort {
		return nil, false, fmt.Errorf("graph %s: port %d is the guest agent and is never reachable by an edge", g.Name, port)
	}
	nd, ok := g.Nodes[hacia]
	if !ok {
		return nil, false, fmt.Errorf("graph %s has no node %s", g.Name, hacia)
	}
	if nd.MachineID == "" {
		return nil, true, nil // lazy sin instancia
	}
	dest = m.byID[nd.MachineID] // el ID exacto que dice el grafo
	if dest == nil {
		return nil, false, fmt.Errorf("the machine of node %s no longer exists", hacia)
	}
	if dest.Labels[api.LabelGraph] != gid || dest.Labels[api.LabelGraphNode] != hacia {
		return nil, false, fmt.Errorf("machine %s is not node %s of graph %s", dest.Name, hacia, shortID(gid))
	}
	if !puertoExpuesto(dest.Labels[api.LabelPorts], port) {
		return nil, false, fmt.Errorf("node %s does not expose port %d (%s)", hacia, port, api.LabelPorts)
	}
	switch dest.State {
	case api.StateRunning:
		return dest, false, nil
	case api.StateWarm, api.StatePaused:
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("node %s is %s", hacia, dest.State)
}

// asegurarNodo espera a que el nodo esté en marcha, lanzando su despertar si
// no hay uno en vuelo.
func (m *Manager) asegurarNodo(ctx context.Context, gid, nodo string) error {
	k := gid + "/" + nodo
	m.despMu.Lock()
	if m.desp == nil {
		m.desp = map[string]*despertar{}
	}
	d := m.desp[k]
	if d == nil {
		d = &despertar{hecho: make(chan struct{})}
		m.desp[k] = d
		go m.despertarNodo(k, gid, nodo, d)
	}
	if d.esperando >= despertarMaxEspera {
		m.despMu.Unlock()
		return fmt.Errorf("%w (node %s)", credproxy.ErrEnlaceOcupado, nodo)
	}
	d.esperando++
	m.despMu.Unlock()
	select {
	case <-d.hecho:
		return d.err
	case <-ctx.Done():
		// La conexión se fue: deja de contar. Si no, un despertar largo con
		// clientes que abandonan acabaría rechazando por "ocupado" a los que
		// llegan aunque ya no espere nadie.
		m.despMu.Lock()
		d.esperando--
		m.despMu.Unlock()
		return ctx.Err()
	}
}

// despertarNodo es el despertar en vuelo de un nodo: con su propio plazo (no
// el de la conexión que lo pidió, que puede irse), y con el cerrojo del grafo.
func (m *Manager) despertarNodo(k, gid, nodo string, d *despertar) {
	ctx, cancel := context.WithTimeout(context.Background(), plazoDespertar)
	defer cancel()
	err := m.despertarNodoLocked(ctx, gid, nodo)
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == api.StatusInsufficientMemory {
		err = fmt.Errorf("%w: %v", credproxy.ErrSinCapacidad, err)
	}
	m.despMu.Lock()
	delete(m.desp, k)
	d.err = err
	close(d.hecho)
	m.despMu.Unlock()
}

// despertarNodoLocked pone en marcha el nodo: instancia un lazy, descongela o
// reanuda. Toma el cerrojo del grafo.
func (m *Manager) despertarNodoLocked(ctx context.Context, gid, nodo string) error {
	defer m.lock(claveCerrojoGrafo(gid))()
	m.mu.RLock()
	g := m.grafos[gid]
	var id string
	var existe bool
	if g != nil {
		var nd api.GraphNode
		nd, existe = g.Nodes[nodo]
		id = nd.MachineID
	}
	m.mu.RUnlock()
	if g == nil || !existe {
		return fmt.Errorf("node %s of graph %s no longer exists", nodo, shortID(gid))
	}
	if id == "" {
		if err := m.instanciarNodo(ctx, gid, nodo, ""); err != nil {
			return err
		}
		return m.conectarNodo(ctx, gid, nodo, nil)
	}
	mc, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("the machine of node %s no longer exists", nodo)
	}
	switch mc.State {
	case api.StateRunning:
		return nil
	case api.StateWarm, api.StatePaused:
		return despertarNodoGrafo(ctx, m, id)
	}
	return fmt.Errorf("node %s is %s", nodo, mc.State)
}
