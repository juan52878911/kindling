// Package grafo son las aristas de un grafo de microVMs vistas desde kling-vz
// (docs/grafos.md): lo que en Linux hacen el resolver, los DNAT y los proxies
// de enlace que el daemon monta en el netns de cada nodo.
//
// El invitado resuelve <nodo>.graph a la pasarela (egress.Policy.GraphHost) y
// conecta a ella; la pila de red (vnet) trae aquí cada conexión a un puerto
// de arista link. Aquí NO se resuelve nada: se pide al daemon, por su socket
// (pkg/linkbroker), una conexión ya abierta hacia esa arista, y el daemon
// comprueba grafo, nodo, arista y puerto bajo su candado, despierta al
// destino si duerme, marca él mismo y entrega el socket. Este proceso nunca
// ve ni elige una dirección: aunque un invitado hostil lo comprometiera, solo
// podría pedir las aristas que el grafo ya le da a su máquina (el daemon sabe
// qué máquina pregunta por el PID del socket, no por lo que diga la
// petición).
//
// Lo mismo para las credenciales Postgres hacia otra máquina (aristas
// credential, kling db attach): DialMachine es el credproxy.DialMachineFunc
// del proxy de la máquina.
//
// CADA SESIÓN tiene su arrendamiento: la conexión Unix con el daemon queda
// abierta mientras dura, y si se cierra (el daemon la invalida porque el
// destino se congela, o el daemon se reinicia) la sesión se corta aquí.
package grafo

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

const (
	// MaxEnlaces es api.GraphMaxEdges: aristas que puede tener un nodo.
	MaxEnlaces = 64
	// MaxConnsEnlace es api.GraphMaxConnsPerEdge: conexiones a la vez por
	// arista link (las que esperan a que el destino despierte también
	// cuentan); la siguiente se rechaza y queda en la auditoría como busy.
	MaxConnsEnlace = 16
	// PlazoResolver acota la petición al daemon, despertar incluido.
	PlazoResolver = 2 * time.Minute
	// puertoAgente es api.GuestPort: ninguna arista llega nunca al agente del
	// invitado. El daemon lo rechaza; aquí se rechaza también al configurar.
	puertoAgente = 8080
)

// Enlace es una arista link saliente del nodo: el invitado conecta a
// Host:Port (Host resuelve a la pasarela).
type Enlace struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Grafo son las aristas de la máquina de este kling-vz.
type Grafo struct {
	dial func(ctx context.Context) (*net.UnixConn, error)
	aud  *credproxy.Auditor
	logf func(string, ...any)

	mu      sync.RWMutex
	enlaces map[uint16]string        // puerto -> host
	sems    map[uint16]chan struct{} // puerto -> conexiones a la vez
}

// New devuelve el grafo de una máquina que pregunta al daemon por el socket
// broker. aud es el registro de auditoría de la máquina (el del proxy de
// credenciales), o nil.
func New(broker string, aud *credproxy.Auditor, logf func(string, ...any)) *Grafo {
	return NewConDial(func(ctx context.Context) (*net.UnixConn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", broker)
		if err != nil {
			return nil, err
		}
		return c.(*net.UnixConn), nil
	}, aud, logf)
}

// NewConDial es New con otra forma de llegar al daemon (las pruebas).
func NewConDial(dial func(ctx context.Context) (*net.UnixConn, error), aud *credproxy.Auditor, logf func(string, ...any)) *Grafo {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Grafo{dial: dial, aud: aud, logf: logf, enlaces: map[uint16]string{}, sems: map[uint16]chan struct{}{}}
}

// Validar comprueba las aristas que manda el daemon.
func Validar(enlaces []Enlace) error {
	if len(enlaces) > MaxEnlaces {
		return fmt.Errorf("%d links; the limit is %d", len(enlaces), MaxEnlaces)
	}
	vistos := map[int]bool{}
	for _, e := range enlaces {
		if _, err := linkbroker.NodoDeHost(e.Host); err != nil {
			return err
		}
		switch {
		case e.Port < 1 || e.Port > 65535:
			return fmt.Errorf("link %s: port %d out of range", e.Host, e.Port)
		case e.Port == 53 || e.Port == 80 || e.Port == 443:
			return fmt.Errorf("link %s: port %d is reserved on the graph address", e.Host, e.Port)
		case e.Port == puertoAgente:
			return fmt.Errorf("link %s: port %d is the guest agent and is never reachable by an edge", e.Host, e.Port)
		case vistos[e.Port]:
			return fmt.Errorf("two links on port %d", e.Port)
		}
		vistos[e.Port] = true
	}
	return nil
}

// Set fija las aristas link (sustituye las anteriores). Las de un puerto que
// sigue conservan su cuenta de conexiones.
func (g *Grafo) Set(enlaces []Enlace) error {
	if err := Validar(enlaces); err != nil {
		return err
	}
	nuevos := make(map[uint16]string, len(enlaces))
	sems := make(map[uint16]chan struct{}, len(enlaces))
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range enlaces {
		p := uint16(e.Port)
		nuevos[p] = e.Host
		if s, ok := g.sems[p]; ok {
			sems[p] = s
		} else {
			sems[p] = make(chan struct{}, MaxConnsEnlace)
		}
	}
	g.enlaces, g.sems = nuevos, sems
	return nil
}

// Link dice si port de la pasarela es una arista link.
func (g *Grafo) Link(port uint16) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.enlaces[port]
	return ok
}

func (g *Grafo) enlace(port uint16) (string, chan struct{}, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	h, ok := g.enlaces[port]
	return h, g.sems[port], ok
}

// Serve atiende una conexión del invitado a la arista link del puerto port.
// Pide al daemon la conexión con el destino ANTES de completar la del
// invitado: si no hay, rechazar() le manda un RST. Con ella, aceptar() completa
// la del invitado y se copian los dos sentidos hasta que uno cierre o el
// daemon corte el arrendamiento. Una línea de auditoría por conexión.
func (g *Grafo) Serve(ctx context.Context, port uint16, aceptar func() (net.Conn, error), rechazar func()) {
	inicio := time.Now()
	host, sem, ok := g.enlace(port)
	rec := credproxy.Record{Kind: credproxy.KindLink, Host: net.JoinHostPort(host, strconv.Itoa(int(port)))}
	defer func() {
		rec.MS = time.Since(inicio).Milliseconds()
		g.auditar(rec)
	}()
	if !ok {
		rechazar()
		rec.Denied, rec.Reason = true, credproxy.ReasonMachineUnavailable
		return
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		rechazar()
		rec.Reason = credproxy.ReasonBusy
		return
	}
	up, resp, err := g.pedir(ctx, linkbroker.Request{V: linkbroker.Version, Kind: linkbroker.KindLink, Host: host, Port: int(port)})
	if err != nil {
		rechazar()
		rec.Denied, rec.Reason = true, motivo(resp.Reason)
		g.logf("link %s: %v", rec.Host, err)
		return
	}
	defer up.Close()
	rec.Upstream = "machine:" + resp.Machine
	guest, err := aceptar()
	if err != nil {
		rec.Reason = credproxy.ReasonUpstreamError
		return
	}
	defer guest.Close()
	rec.ReqBytes, rec.RespBytes = copiar(guest, up)
	if up.cortada.Load() {
		rec.Reason = credproxy.ReasonInvalidated
	}
}

// motivo traduce el motivo del daemon al de la auditoría (vacío: el destino
// no se pudo usar).
func motivo(r string) string {
	switch r {
	case linkbroker.ReasonBusy:
		return credproxy.ReasonBusy
	case linkbroker.ReasonNoCapacity:
		return credproxy.ReasonNoCapacity
	case linkbroker.ReasonUpstreamError:
		return credproxy.ReasonUpstreamError
	}
	return credproxy.ReasonMachineUnavailable
}

// DialMachine es el credproxy.DialMachineFunc del proxy de la máquina: la
// conexión con la máquina de una credencial Postgres (UpstreamMachine), que
// entrega el daemon.
func (g *Grafo) DialMachine(ctx context.Context, id, owner string, port int) (net.Conn, error) {
	c, _, err := g.pedir(ctx, linkbroker.Request{V: linkbroker.Version, Kind: linkbroker.KindMachine, Machine: id, Owner: owner, Port: port})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// pedir pide una conexión al daemon y la envuelve con su arrendamiento.
func (g *Grafo) pedir(ctx context.Context, req linkbroker.Request) (*Conexion, linkbroker.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, PlazoResolver)
	defer cancel()
	uc, err := g.dial(ctx)
	if err != nil {
		return nil, linkbroker.Response{}, fmt.Errorf("can't reach the daemon: %w", err)
	}
	// Si la conexión del invitado se va (o el proceso cierra) mientras el
	// daemon despierta al destino, se deja de esperar.
	parar := context.AfterFunc(ctx, func() { uc.Close() })
	plazo := PlazoResolver
	if dl, ok := ctx.Deadline(); ok {
		plazo = time.Until(dl)
	}
	tc, resp, err := linkbroker.Pedir(uc, req, plazo)
	if !parar() {
		// El contexto venció justo entonces: uc está cerrada, sin
		// arrendamiento no hay sesión.
		if tc != nil {
			tc.Close()
		}
		if err == nil {
			err = ctx.Err()
		}
		return nil, resp, err
	}
	if err != nil {
		return nil, resp, err
	}
	c := &Conexion{TCPConn: tc, arr: uc, fin: make(chan struct{})}
	go c.vigilar()
	return c, resp, nil
}

// Conexion es la conexión con el destino y su arrendamiento: cerrarla lo
// suelta, y si el daemon lo suelta antes (invalidación, reinicio), se corta.
type Conexion struct {
	*net.TCPConn
	arr     *net.UnixConn
	fin     chan struct{} // se cierra al soltarla
	cortada atomic.Bool
	cerrada atomic.Bool
	una     sync.Once
}

func (c *Conexion) vigilar() {
	linkbroker.EsperarFin(c.arr)
	if !c.cerrada.Load() {
		c.cortada.Store(true)
	}
	c.soltar()
}

func (c *Conexion) soltar() {
	c.una.Do(func() {
		_ = c.TCPConn.Close()
		_ = c.arr.Close()
		close(c.fin)
	})
}

// Close cierra la conexión y suelta el arrendamiento.
func (c *Conexion) Close() error {
	c.cerrada.Store(true)
	c.soltar()
	return nil
}

// Cortada dice si la cortó el daemon (y no quien la usaba).
func (c *Conexion) Cortada() bool { return c.cortada.Load() }

type closeWriter interface{ CloseWrite() error }

// copiar copia en los dos sentidos respetando el medio cierre, y devuelve lo
// que fue del invitado al destino y lo que volvió.
func copiar(guest net.Conn, up *Conexion) (subida, bajada int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		subida, _ = io.Copy(up.TCPConn, guest)
		_ = up.TCPConn.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		bajada, _ = io.Copy(guest, up.TCPConn)
		if cw, ok := guest.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = guest.Close()
		}
	}()
	// Si el daemon corta, las dos copias tienen que acabar aunque el invitado
	// no cierre: cerrar el lado del invitado desbloquea la que lee de él.
	hecho := make(chan struct{})
	go func() {
		select {
		case <-hecho:
		case <-up.fin:
			_ = guest.Close()
		}
	}()
	wg.Wait()
	close(hecho)
	return subida, bajada
}

func (g *Grafo) auditar(r credproxy.Record) {
	if g.aud == nil {
		return
	}
	r.TS = time.Now()
	g.aud.Record(r)
}
