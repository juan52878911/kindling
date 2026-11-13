package credproxy

// El proxy en un proceso aparte (macOS, docs/proxy-macos-separado.md): las
// claves viven en kling-credproxy, uno por máquina con credenciales, y no en
// el VMM (kling-vz o kling-hv), que es quien procesa la red del invitado.
// Aquí va lo que comparten los dos lados sin cgo: los nombres de los ficheros
// y el protocolo del socket de reenvío, por el que el VMM pasa cada conexión
// TCP del invitado a la pasarela con una cabecera fija delante.
//
//	invitado ──TCP──▶ VMM (gVisor) ──unix, una por conexión──▶ kling-credproxy ──▶ proveedor
//
// La cabecera es de 4 bytes: 'K', la versión (1) y el puerto de la pasarela al
// que conectó el invitado (big endian). Nada más: el VMM no sabe de claves ni
// de dominios más allá de qué desviar en su DNS, y el proxy no se fía del
// puerto más de lo que se fiaba del de la pila: con uno que no es de ninguna
// credencial, cierra.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// CapSplit es la capacidad que anuncia un VMM (GET /kling/info,
	// credential_kinds) que sabe reenviar las conexiones al proxy aparte
	// (PUT /kling/credproxy). Sin ella el daemon le da las claves a él, como
	// antes.
	CapSplit = "credproxy-split"
	// SplitBinary es el ejecutable del proxy aparte, junto al VMM.
	SplitBinary = "kling-credproxy"
	// SplitDir es el subdirectorio del de la máquina que es solo del proxy:
	// sus dos sockets, su registro de auditoría y los cuerpos derramados. El
	// perfil del VMM le niega escribir ahí (ni plantar un socket falso, ni
	// tocar el registro) y leer los derramados, que llevan la clave.
	SplitDir = "credproxy"
	// SplitAPISock es el socket del API del proxy (en SplitDir): solo el
	// daemon, que le da las credenciales y el modo de salida.
	SplitAPISock = "api.sock"
	// SplitFwdSock es el socket de reenvío (en SplitDir): solo el VMM de la
	// máquina, una conexión por cada conexión del invitado a la pasarela.
	SplitFwdSock = "fwd.sock"
	// SplitTempDir es dónde derrama el proxy aparte los cuerpos grandes (en
	// SplitDir; ver PrepararTempDir).
	SplitTempDir = "tmp"
	// SplitLog es la salida del proceso (en el directorio de la máquina, no
	// en SplitDir: la escribe el daemon al lanzarlo).
	SplitLog = "credproxy.log"
	// AuditFileVMM es el registro de lo que atiende el propio VMM (aristas
	// link, puentes al host y las negativas de su proxy sin claves), en el
	// directorio de la máquina. El daemon lo lee junto al del proxy.
	AuditFileVMM = "vmmaudit.jsonl"

	// SplitHeaderLen es el tamaño de la cabecera de reenvío.
	SplitHeaderLen = 4
	// splitMagic y splitVersion son sus dos primeros bytes.
	splitMagic   = 'K'
	splitVersion = 1
	// splitHeaderTimeout es cuánto espera el proxy a la cabecera.
	splitHeaderTimeout = 5 * time.Second
	// MaxSplitConns acota las conexiones de reenvío a la vez en el proxy (los
	// topes de cada protocolo van después: MaxInFlight, MaxPGConns...).
	MaxSplitConns = 2048
)

// ErrSplitHeader es una cabecera de reenvío que no cuadra.
var ErrSplitHeader = errors.New("bad credential proxy forward header")

// SplitHeader es la cabecera de una conexión reenviada al puerto port.
func SplitHeader(port int) ([SplitHeaderLen]byte, error) {
	if port <= 0 || port > 65535 {
		return [SplitHeaderLen]byte{}, fmt.Errorf("%w: port %d", ErrSplitHeader, port)
	}
	return [SplitHeaderLen]byte{splitMagic, splitVersion, byte(port >> 8), byte(port)}, nil
}

// LeerSplitHeader lee la cabecera de r y devuelve el puerto.
func LeerSplitHeader(r io.Reader) (int, error) {
	var b [SplitHeaderLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSplitHeader, err)
	}
	if b[0] != splitMagic || b[1] != splitVersion {
		return 0, fmt.Errorf("%w: magic %#x version %d", ErrSplitHeader, b[0], b[1])
	}
	port := int(b[2])<<8 | int(b[3])
	if port == 0 {
		return 0, fmt.Errorf("%w: port 0", ErrSplitHeader)
	}
	return port, nil
}

// SplitServer atiende el socket de reenvío del proxy aparte: lee la cabecera
// de cada conexión y la pasa al proxy HTTP (puerto 80) o al de bases de datos
// (los demás, si hay alguna credencial de base de datos activa).
type SplitServer struct {
	p    *Proxy
	http *http.Server
	ch   chan net.Conn
	sem  chan struct{}
	// Permitir, si no es nil, decide si se atiende una conexión (quién está
	// al otro lado): el proceso la usa para admitir solo al VMM de su
	// máquina. Se mira antes de leer nada.
	Permitir func(net.Conn) bool

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	wg     sync.WaitGroup
}

// NewSplitServer crea el servidor de reenvío de p.
func NewSplitServer(p *Proxy) *SplitServer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SplitServer{
		p:      p,
		http:   NewServer(p),
		ch:     make(chan net.Conn),
		sem:    make(chan struct{}, MaxSplitConns),
		ctx:    ctx,
		cancel: cancel,
	}
	l := &listenerCanal{ch: s.ch, done: ctx.Done()}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.http.Serve(l)
	}()
	return s
}

// Serve acepta de l hasta que se cierre (o Close).
func (s *SplitServer) Serve(l net.Listener) error {
	stop := context.AfterFunc(s.ctx, func() { _ = l.Close() })
	defer stop()
	for {
		c, err := l.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
		default:
			// Por encima del tope: se cierra, como el RST de la pila.
			_ = c.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.atender(c)
		}()
	}
}

func (s *SplitServer) atender(c net.Conn) {
	if s.Permitir != nil && !s.Permitir(c) {
		_ = c.Close()
		return
	}
	// Close no espera a una cabecera que no llega: corta la conexión.
	parar := context.AfterFunc(s.ctx, func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(splitHeaderTimeout))
	port, err := LeerSplitHeader(c)
	_ = c.SetReadDeadline(time.Time{})
	if !parar() || err != nil {
		_ = c.Close()
		return
	}
	if port == 80 {
		// Al http.Server por el listener de canal; él la cierra. Hasta que
		// la recoja, Close de este servidor también la libera.
		hecho := make(chan struct{})
		cc := &connAvisa{Conn: c, hecho: hecho}
		select {
		case s.ch <- cc:
		case <-s.ctx.Done():
			_ = c.Close()
			return
		}
		select {
		case <-hecho:
		case <-s.ctx.Done():
			_ = c.Close()
		}
		return
	}
	if !s.p.DBActivo() {
		_ = c.Close()
		return
	}
	s.p.ServeDB(s.ctx, c, port)
}

// Close corta todo: el servidor HTTP, las sesiones de base de datos y lo que
// esté esperando cabecera.
func (s *SplitServer) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.http.Close()
	})
	s.wg.Wait()
	return nil
}

// listenerCanal es un net.Listener que entrega las conexiones de un canal.
type listenerCanal struct {
	ch   chan net.Conn
	done <-chan struct{}
}

func (l *listenerCanal) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listenerCanal) Close() error { return nil }

func (l *listenerCanal) Addr() net.Addr { return &net.UnixAddr{Name: "credproxy-split", Net: "unix"} }

// connAvisa avisa por hecho cuando la cierran (el http.Server, al acabar).
type connAvisa struct {
	net.Conn
	hecho chan struct{}
	once  sync.Once
}

func (c *connAvisa) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.hecho) })
	return err
}
