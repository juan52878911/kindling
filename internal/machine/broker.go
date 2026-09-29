package machine

// El broker de enlaces: cómo llega en macOS la conexión de una arista (link o
// credential) o de kling db attach a otra máquina (docs/grafos.md,
// SECURITY.md, pkg/linkbroker).
//
// En Linux el proxy de cada arista es del daemon y marca al netns del destino.
// En macOS la red del invitado vive en su kling-vz, que no conoce a las
// demás máquinas. Aquí se decidió INVERTIR el flujo en vez de darle una
// dirección: kling-vz dice qué arista quiere, el daemon hace todo lo que
// haría el proxy de Linux (comprueba bajo m.mu con comprobarAristaLocked o
// comprobarCopiaLocked, despierta al destino si duerme, marca al reenvío del
// destino) y le entrega el socket YA CONECTADO. Así:
//
//   - kling-vz no ve ni elige nunca una dirección. Si se la diéramos, entre
//     la respuesta y su dial el destino podría congelarse y otra máquina
//     heredar su puerto del rango reservado (el TOCTOU que el diseño evita),
//     y habría que abrirle a kling-vz marcar a los reenvíos de otros, que hoy
//     upstream.go le prohíbe.
//   - Tras marcar, se vuelve a mirar bajo m.mu que el destino sigue siendo
//     ese y su reenvío el mismo: lo que se entrega es a lo que el grafo dice.
//   - Cortar no necesita un canal nuevo hacia kling-vz: el daemon guarda su
//     copia del socket y hace shutdown, que corta los dos lados.
//
// QUIÉN PREGUNTA no lo dice la petición: el daemon lo sabe por el PID del otro
// extremo del socket Unix (broker_vz.go), que tiene que ser el kling-vz de
// una máquina. Desde ahí, solo las aristas de ESA máquina.
//
// Este fichero es la parte común (y la que prueban los tests en los dos
// sistemas); escuchar e identificar al que llama es de broker_vz.go.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

const (
	// maxSesionesBroker: sesiones a la vez de UNA máquina de origen (todas sus
	// aristas llenas). Un kling-vz comprometido no puede agotar al daemon.
	maxSesionesBroker = api.GraphMaxEdges * api.GraphMaxConnsPerEdge
	// plazoPeticionBroker acota lo que tarda en llegar la petición.
	plazoPeticionBroker = 5 * time.Second
)

// marcarBroker abre la conexión con el destino: m.marcarBrokerPrueba si los
// tests la ponen.
func (m *Manager) marcarBroker(ctx context.Context, addr string) (net.Conn, error) {
	if m.marcarBrokerPrueba != nil {
		return m.marcarBrokerPrueba(ctx, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// sesionBroker es una conexión entregada (o en camino) a un kling-vz.
type sesionBroker struct {
	origen string // máquina que la pidió
	kind   string // linkbroker.KindLink o KindMachine
	// objetivo es lo que nombra la petición: el ID virtual del nodo destino
	// (aristas) o el de la copia (attach). No cambia aunque cambie la
	// máquina del nodo: invalidar el nodo corta también las que aún
	// resuelven.
	objetivo string

	mu      sync.Mutex
	maquina string // la máquina a la que va; "" mientras se resuelve
	cortada bool
	tc      *net.TCPConn // la copia del daemon del socket entregado
	arr     *net.UnixConn
	cancel  context.CancelFunc
}

// cortar la termina en los dos lados. Idempotente.
func (s *sesionBroker) cortar() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cortada = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.tc != nil {
		linkbroker.Cortar(s.tc)
	}
	if s.arr != nil {
		_ = s.arr.Close()
	}
}

// fijar anota el destino y el socket, si no la han cortado ya.
func (s *sesionBroker) fijar(maquina string, tc *net.TCPConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cortada {
		return false
	}
	s.maquina, s.tc = maquina, tc
	return true
}

// registroBroker son las sesiones vivas de todos los kling-vz. Es del paquete,
// como los proxies de enlace de internal/net en Linux: quien invalida
// (invalidarCopia, invalidarEnlaces) no tiene el manager.
type registroBroker struct {
	mu        sync.Mutex
	sesiones  map[*sesionBroker]struct{}
	porOrigen map[string]int
}

var sesionesBroker = &registroBroker{sesiones: map[*sesionBroker]struct{}{}, porOrigen: map[string]int{}}

func (r *registroBroker) registrar(s *sesionBroker) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.porOrigen[s.origen] >= maxSesionesBroker {
		return false
	}
	r.sesiones[s] = struct{}{}
	r.porOrigen[s.origen]++
	return true
}

func (r *registroBroker) quitar(s *sesionBroker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sesiones[s]; !ok {
		return
	}
	delete(r.sesiones, s)
	if r.porOrigen[s.origen]--; r.porOrigen[s.origen] <= 0 {
		delete(r.porOrigen, s.origen)
	}
}

// invalidar corta las sesiones de tipo kind hacia la máquina id y las que
// aún resuelven hacia el objetivo id. Devuelve cuántas cortó.
func (r *registroBroker) invalidar(kind, id string) int {
	if id == "" {
		return 0
	}
	r.mu.Lock()
	var cortar []*sesionBroker
	for s := range r.sesiones {
		if s.kind != kind {
			continue
		}
		s.mu.Lock()
		va := !s.cortada && (s.maquina == id || (s.maquina == "" && s.objetivo == id))
		s.mu.Unlock()
		if va {
			cortar = append(cortar, s)
		}
	}
	r.mu.Unlock()
	for _, s := range cortar {
		s.cortar()
	}
	return len(cortar)
}

// invalidarCopiaBroker corta las sesiones de credenciales hacia la máquina
// (o nodo) id.
func invalidarCopiaBroker(id string) int {
	return sesionesBroker.invalidar(linkbroker.KindMachine, id)
}

// invalidarEnlacesBroker corta las sesiones de enlace hacia cada uno de ids.
func invalidarEnlacesBroker(ids ...string) int {
	n := 0
	for _, id := range ids {
		n += sesionesBroker.invalidar(linkbroker.KindLink, id)
	}
	return n
}

// invalidarOrigenBroker corta todas las sesiones que pidió la máquina id: su
// grafo o su dueño de kling db cambió.
func invalidarOrigenBroker(id string) int {
	r := sesionesBroker
	r.mu.Lock()
	var cortar []*sesionBroker
	for s := range r.sesiones {
		if s.origen == id {
			cortar = append(cortar, s)
		}
	}
	r.mu.Unlock()
	for _, s := range cortar {
		s.cortar()
	}
	return len(cortar)
}

// errBroker es un rechazo con su motivo de auditoría.
type errBroker struct {
	motivo string
	err    error
}

func (e *errBroker) Error() string { return e.err.Error() }
func (e *errBroker) Unwrap() error { return e.err }

// motivoBroker da el motivo de auditoría de un error de resolución.
func motivoBroker(err error) string {
	var eb *errBroker
	switch {
	case errors.As(err, &eb):
		return eb.motivo
	case errors.Is(err, credproxy.ErrEnlaceOcupado):
		return linkbroker.ReasonBusy
	case errors.Is(err, credproxy.ErrSinCapacidad):
		return linkbroker.ReasonNoCapacity
	}
	return linkbroker.ReasonMachineUnavailable
}

// atenderBroker atiende UNA conexión de un kling-vz ya identificado como el
// de la máquina origen: lee la petición, abre el destino, lo entrega y guarda
// la sesión hasta que el arrendamiento acabe. Cierra c.
func (m *Manager) atenderBroker(c *net.UnixConn, origen string) {
	defer c.Close()
	req, err := linkbroker.LeerPeticion(c, plazoPeticionBroker)
	if err != nil {
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: err.Error(), Reason: linkbroker.ReasonMachineUnavailable})
		return
	}
	objetivo := req.Machine
	if req.Kind == linkbroker.KindLink {
		m.mu.RLock()
		var gid string
		if src := m.byID[origen]; src != nil {
			gid = src.Labels[api.LabelGraph]
		}
		m.mu.RUnlock()
		nodo, _ := linkbroker.NodoDeHost(req.Host)
		objetivo = idVirtual(gid, nodo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), plazoDespertar)
	defer cancel()
	s := &sesionBroker{origen: origen, kind: req.Kind, objetivo: objetivo, arr: c, cancel: cancel}
	if !sesionesBroker.registrar(s) {
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: fmt.Sprintf("machine %s has %d link sessions open", shortID(origen), maxSesionesBroker), Reason: linkbroker.ReasonBusy})
		return
	}
	defer sesionesBroker.quitar(s)
	// El arrendamiento: si kling-vz cuelga (se rinde mientras el destino
	// despierta, o termina la sesión), se cancela lo que quede y se corta.
	fin := make(chan struct{})
	go func() {
		linkbroker.EsperarFin(c)
		close(fin)
		s.cortar()
	}()

	tc, maquina, err := m.abrirDestinoBroker(ctx, origen, req)
	if err != nil {
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: err.Error(), Reason: motivoBroker(err)})
		s.cortar()
		<-fin
		return
	}
	if !s.fijar(maquina, tc) {
		tc.Close()
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: "the session was invalidated while it was being opened", Reason: linkbroker.ReasonMachineUnavailable})
		<-fin
		return
	}
	if err := linkbroker.Entregar(c, linkbroker.Response{Machine: maquina}, tc); err != nil {
		s.cortar()
		<-fin
		return
	}
	<-fin
}

// abrirDestinoBroker resuelve la petición de la máquina origen como lo haría
// el proxy de Linux, marca el destino y comprueba que sigue siendo el que el
// grafo (o el attach) dice. Devuelve el socket y la máquina destino.
func (m *Manager) abrirDestinoBroker(ctx context.Context, origen string, req linkbroker.Request) (*net.TCPConn, string, error) {
	// Nunca el agente del invitado, sea cual sea la arista o la credencial.
	if req.Port == api.GuestPort {
		return nil, "", fmt.Errorf("port %d is the guest agent and is never reachable by an edge", req.Port)
	}
	var addr, id string
	var err error
	switch req.Kind {
	case linkbroker.KindLink:
		hacia, _ := linkbroker.NodoDeHost(req.Host)
		m.mu.RLock()
		src := m.byID[origen]
		var gid, desde string
		if src != nil {
			gid, desde = src.Labels[api.LabelGraph], src.Labels[api.LabelGraphNode]
		}
		m.mu.RUnlock()
		if gid == "" || desde == "" {
			return nil, "", fmt.Errorf("machine %s is not a node of a graph", shortID(origen))
		}
		addr, id, err = m.resolverArista(ctx, origen, gid, desde, hacia, req.Port, api.GraphEdgeLink)
	case linkbroker.KindMachine:
		// kling-vz solo pide lo que el daemon le entregó: una credencial
		// suya hacia esa máquina, ese dueño y ese puerto.
		if err := m.tieneCredencialAMaquina(origen, req.Machine, req.Owner, req.Port); err != nil {
			return nil, "", err
		}
		if gid, desde, hacia, ok := m.aristaVirtual(origen, req.Machine, req.Owner); ok {
			addr, id, err = m.resolverArista(ctx, origen, gid, desde, hacia, req.Port, api.GraphEdgeCredential)
		} else {
			m.mu.RLock()
			cp, e := m.comprobarCopiaLocked(origen, req.Machine, req.Owner, req.Port)
			if e == nil {
				addr, e = direccionCopiaLocked(cp, req.Port)
				id = cp.ID
			}
			m.mu.RUnlock()
			err = e
		}
	default:
		return nil, "", fmt.Errorf("unknown request kind %q", req.Kind)
	}
	if err != nil {
		return nil, "", err
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("machine %s resolved to %q, not ip:port", shortID(id), addr)
	}
	if err := credproxy.DestinoMaquinaValido(ap); err != nil {
		return nil, "", fmt.Errorf("machine %s: %w", shortID(id), err)
	}
	nc, err := m.marcarBroker(ctx, ap.String())
	if err != nil {
		return nil, "", &errBroker{motivo: linkbroker.ReasonUpstreamError, err: fmt.Errorf("connecting to machine %s: %w", shortID(id), err)}
	}
	tc, ok := nc.(*net.TCPConn)
	if !ok {
		nc.Close()
		return nil, "", fmt.Errorf("connection to machine %s is not TCP", shortID(id))
	}
	// Lo marcado tiene que seguir siendo el destino: la misma máquina, en
	// marcha, con el mismo reenvío para ese puerto.
	m.mu.RLock()
	vigente := false
	if dest := m.byID[id]; dest != nil && dest.State == api.StateRunning {
		if a, err := direccionCopiaLocked(dest, req.Port); err == nil && a == addr {
			vigente = true
		}
	}
	m.mu.RUnlock()
	if !vigente {
		tc.Close()
		return nil, "", fmt.Errorf("machine %s changed while connecting to it", shortID(id))
	}
	return tc, id, nil
}

// tieneCredencialAMaquina comprueba que la máquina origen tiene en su almacén
// una credencial Postgres hacia la máquina id, del dueño owner y en ese
// puerto.
func (m *Manager) tieneCredencialAMaquina(origen, id, owner string, port int) error {
	creds, err := m.cargarCredenciales(origen)
	if err != nil {
		return err
	}
	for _, c := range creds {
		p := c.Port
		if p == 0 {
			p = credproxy.PGDefaultPort
		}
		if c.Kind == credproxy.KindPostgres && c.UpstreamMachine == id && c.UpstreamOwner == owner && p == port {
			return nil
		}
	}
	return fmt.Errorf("machine %s has no credential for machine %s on port %d", shortID(origen), shortID(id), port)
}
