package credproxy

// Proxy de ENLACE: la arista link de un grafo de microVMs (docs/grafos.md).
// Un nodo A abre TCP a <b>.graph:P; su resolver le da la IP del host en su
// veth, un DNAT de su netns lo trae aquí, y este proxy lo lleva al puerto P de
// la máquina B. TCP crudo: no mira dentro, no marca claves, no reintenta.
//
// LO QUE NO SE HACE, igual que con UpstreamMachine (maquina.go): fijar la
// dirección de B al crear el enlace. B puede estar congelada, no existir aún
// (un nodo lazy) o haberse borrado y su índice de red haber pasado a otra
// máquina. La dirección se pide a Resolve en CADA conexión aceptada, y Resolve
// (el manager, bajo su candado) comprueba el grafo, el nodo, la arista y el
// puerto; y aun así lo resuelto tiene que ser un destino de kindling
// (destinoMaquinaValido).
//
// CORTAR LO QUE YA ESTÁ: Invalidar(id) cierra las sesiones vivas hacia la
// máquina id (congelada, pausada, parada, borrada). Una sesión se registra
// ANTES de resolver, "pendiente" hasta saber a qué máquina va; Invalidar con
// el Target del enlace (el nodo destino, que no cambia aunque cambie su
// máquina) corta también las pendientes: no se sabe todavía a qué máquina
// iban, y es preferible cortar de más a dejar pasar una que resolvió justo
// antes de un freeze. El daemon invalida siempre con los dos: la máquina y
// su nodo.
//
// ACOTADO: MaxConns conexiones a la vez (las que esperan a que el destino
// despierte también cuentan); la siguiente se cierra en el acto y queda en la
// auditoría como busy.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// KindLink es un registro de auditoría del proxy de enlace: una línea por
// conexión.
const KindLink = "link"

// Motivos propios del enlace (Record.Reason). Los de siempre también valen:
// ReasonBusy (tope de conexiones o de despertares), ReasonMachineUnavailable
// (el destino no se puede usar) y ReasonUpstreamError (no se pudo conectar).
const (
	// ReasonNoCapacity: el destino dormía y despertarlo no cabe en el host.
	ReasonNoCapacity = "no_capacity"
	// ReasonInvalidated: la sesión se cortó porque el destino se congeló,
	// pausó, paró o borró (o el origen dejó de tener la arista).
	ReasonInvalidated = "invalidated"
)

// Errores que Resolve puede envolver para que la auditoría diga el motivo.
var (
	// ErrEnlaceOcupado: demasiadas conexiones esperando a que el destino
	// despierte.
	ErrEnlaceOcupado = errors.New("too many connections waiting for the node to wake up")
	// ErrSinCapacidad: el destino no cabe en el host.
	ErrSinCapacidad = errors.New("the node doesn't fit on the host right now")
)

// ResolveLinkFunc resuelve el destino de una conexión: la dirección
// ("ip:puerto") y el ID de la máquina a la que pertenece (para Invalidar). Un
// error es "no conectes".
type ResolveLinkFunc func(ctx context.Context) (addr, machine string, err error)

// LinkOptions configura un Enlace.
type LinkOptions struct {
	// Name es lo que la auditoría dice del destino: "api.graph:8080".
	Name string
	// Target identifica el nodo destino (no su máquina): Invalidar(Target)
	// corta también las sesiones que aún resuelven.
	Target string
	// Resolve se llama en cada conexión aceptada.
	Resolve ResolveLinkFunc
	// MaxConns: conexiones a la vez; 0 = 16.
	MaxConns int
	// ResolveTimeout acota Resolve (despertar un nodo incluido); 0 = 2 min.
	ResolveTimeout time.Duration
	// Auditor, si no es nil, recibe una línea por conexión.
	Auditor *Auditor
	// Logf recibe los fallos que no son de una conexión concreta.
	Logf func(string, ...any)

	// Para los tests: la comprobación del destino y el dialer.
	Destino func(netip.AddrPort) error
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Enlace es el proxy de una arista link.
type Enlace struct {
	o   LinkOptions
	sem chan struct{}

	mu       sync.Mutex
	ln       net.Listener
	cerrado  bool
	sesiones map[*sesionEnlace]struct{}
	wg       sync.WaitGroup
}

// sesionEnlace es una conexión en curso: la del invitado y, cuando la hay, la
// del destino. maquina vacía = aún resolviendo.
type sesionEnlace struct {
	mu      sync.Mutex
	maquina string
	cortada bool
	conns   []net.Conn
	// cancel corta la resolución en curso (un despertar que ya no hace falta).
	cancel context.CancelFunc
}

// cerrar cierra las dos mitades y la resolución en curso. Idempotente.
func (s *sesionEnlace) cerrar() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cortada = true
	if s.cancel != nil {
		s.cancel()
	}
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// fijar anota a qué máquina va la sesión, si no la han cortado ya.
func (s *sesionEnlace) fijar(maquina string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cortada {
		return false
	}
	s.maquina = maquina
	return true
}

// unir añade la conexión con el destino; si la sesión ya se cortó, la cierra
// y devuelve false.
func (s *sesionEnlace) unir(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cortada {
		_ = c.Close()
		return false
	}
	s.conns = append(s.conns, c)
	return true
}

func (s *sesionEnlace) estaCortada() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cortada
}

// NuevoEnlace crea el proxy de una arista. Resolve es obligatorio.
func NuevoEnlace(o LinkOptions) *Enlace {
	if o.MaxConns <= 0 {
		o.MaxConns = 16
	}
	if o.ResolveTimeout <= 0 {
		o.ResolveTimeout = 2 * time.Minute
	}
	if o.Destino == nil {
		o.Destino = destinoMaquinaValido
	}
	if o.Dial == nil {
		o.Dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: pgKeepAlive}).DialContext
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Enlace{o: o, sem: make(chan struct{}, o.MaxConns), sesiones: map[*sesionEnlace]struct{}{}}
}

// SetResolve cambia el resolvedor (el manager lo rehace al descongelar o
// reiniciar el daemon). Las conexiones en curso siguen con el suyo.
func (e *Enlace) SetResolve(r ResolveLinkFunc) {
	e.mu.Lock()
	e.o.Resolve = r
	e.mu.Unlock()
}

// Serve acepta conexiones de ln hasta Close.
func (e *Enlace) Serve(ln net.Listener) error {
	e.mu.Lock()
	if e.cerrado {
		e.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	e.ln = ln
	e.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			e.mu.Lock()
			cerrado := e.cerrado
			e.mu.Unlock()
			if cerrado || errors.Is(err, net.ErrClosed) {
				return net.ErrClosed
			}
			// Un error pasajero (demasiados ficheros abiertos): sin bucle
			// caliente.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		select {
		case e.sem <- struct{}{}:
		default:
			_ = c.Close()
			e.auditar(Record{Kind: KindLink, Host: e.o.Name, Reason: ReasonBusy})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), e.o.ResolveTimeout)
		s := &sesionEnlace{conns: []net.Conn{c}, cancel: cancel}
		if !e.registrar(s) {
			cancel()
			<-e.sem
			_ = c.Close()
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer func() { <-e.sem }()
			defer e.quitar(s)
			defer cancel()
			e.atender(ctx, s, c)
		}()
	}
}

func (e *Enlace) registrar(s *sesionEnlace) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cerrado {
		return false
	}
	e.sesiones[s] = struct{}{}
	return true
}

func (e *Enlace) quitar(s *sesionEnlace) {
	e.mu.Lock()
	delete(e.sesiones, s)
	e.mu.Unlock()
}

// atender resuelve, conecta y copia en los dos sentidos.
func (e *Enlace) atender(ctx context.Context, s *sesionEnlace, guest net.Conn) {
	inicio := time.Now()
	rec := Record{Kind: KindLink, Host: e.o.Name}
	defer func() {
		rec.MS = time.Since(inicio).Milliseconds()
		e.auditar(rec)
	}()
	defer s.cerrar()

	e.mu.Lock()
	resolve := e.o.Resolve
	e.mu.Unlock()
	// ctx lo cancela también cerrar(): si cortan la sesión mientras se
	// resuelve (un freeze del destino), la resolución no sigue esperando.
	addr, maquina, err := resolve(ctx)
	if err != nil {
		rec.Denied = true
		rec.Reason = motivoResolucion(err)
		e.o.Logf("link %s: %v", e.o.Name, err)
		return
	}
	rec.Upstream = "machine:" + maquina
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		rec.Reason, rec.Denied = ReasonMachineUnavailable, true
		e.o.Logf("link %s: machine %s resolved to %q, not ip:port", e.o.Name, maquina, addr)
		return
	}
	if err := e.o.Destino(ap); err != nil {
		rec.Reason, rec.Denied = ReasonMachineUnavailable, true
		e.o.Logf("link %s: machine %s: %v", e.o.Name, maquina, err)
		return
	}
	if !s.fijar(maquina) {
		rec.Reason = ReasonInvalidated
		return
	}
	up, err := e.o.Dial(ctx, "tcp", ap.String())
	if err != nil {
		rec.Reason = ReasonUpstreamError
		return
	}
	if !s.unir(up) {
		rec.Reason = ReasonInvalidated
		return
	}

	var subida, bajada atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(up, guest)
		subida.Store(n)
		cerrarEscritura(up)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(guest, up)
		bajada.Store(n)
		cerrarEscritura(guest)
	}()
	wg.Wait()
	rec.ReqBytes, rec.RespBytes = subida.Load(), bajada.Load()
	// Cortada antes del cierre normal (el defer de arriba): la cortó
	// Invalidar.
	if s.estaCortada() {
		rec.Reason = ReasonInvalidated
	}
}

// cerrarEscritura manda FIN por c si sabe (TCP), para que el otro lado vea
// el fin del flujo sin cerrar la lectura.
func cerrarEscritura(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// motivoResolucion traduce un error de Resolve a un motivo de auditoría.
func motivoResolucion(err error) string {
	switch {
	case errors.Is(err, ErrEnlaceOcupado):
		return ReasonBusy
	case errors.Is(err, ErrSinCapacidad):
		return ReasonNoCapacity
	}
	return ReasonMachineUnavailable
}

func (e *Enlace) auditar(r Record) {
	if e.o.Auditor == nil {
		return
	}
	r.TS = time.Now()
	e.o.Auditor.Record(r)
}

// Invalidar corta las sesiones vivas hacia la máquina id; si id es el Target
// del enlace, también las que aún no saben a qué máquina van; con id "" las
// corta todas. Devuelve cuántas cortó.
func (e *Enlace) Invalidar(id string) int {
	e.mu.Lock()
	ss := make([]*sesionEnlace, 0, len(e.sesiones))
	for s := range e.sesiones {
		ss = append(ss, s)
	}
	e.mu.Unlock()
	n := 0
	for _, s := range ss {
		s.mu.Lock()
		va := id == "" || s.maquina == id || (s.maquina == "" && id == e.o.Target)
		ya := s.cortada
		s.mu.Unlock()
		if va && !ya {
			s.cerrar()
			n++
		}
	}
	return n
}

// Close deja de aceptar, corta todas las sesiones y espera a que terminen.
func (e *Enlace) Close() error {
	e.mu.Lock()
	if e.cerrado {
		e.mu.Unlock()
		return nil
	}
	e.cerrado = true
	ln := e.ln
	e.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	e.Invalidar("")
	e.wg.Wait()
	return err
}

// String para los logs.
func (e *Enlace) String() string { return fmt.Sprintf("link %s", e.o.Name) }

// Auditor devuelve el registro de auditoría del proxy (nil sin AuditPath),
// para que otros proxies de la misma máquina (el de enlace) escriban en el
// mismo fichero con la misma cuenta de descartados.
func (p *Proxy) Auditor() *Auditor { return p.aud }
