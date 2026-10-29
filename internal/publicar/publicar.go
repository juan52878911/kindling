// Package publicar son los puertos que el daemon publica en 127.0.0.1 del host
// hacia una máquina: `kling run -p`, los `ports:` de `kling compose up`.
//
// Antes los publicaba el CLI en primer plano, y por eso no había `compose up
// -d`: al cerrar el CLI se iban los puertos. Ahora los tiene el daemon, que
// los guarda en su raíz (sobreviven a su reinicio) y los quita cuando la
// máquina desaparece.
//
// Cada conexión resuelve la dirección de la máquina en ese momento: en el Mac
// cada máquina se alcanza por un puerto de loopback que cambia con cada
// arranque o descongelación (Machine.Forwards). Si la máquina está congelada
// o en pausa, la primera conexión la despierta.
//
// Con daemon.idle_freeze, una máquina con puertos publicados que lleva el
// plazo sin conexiones (ni por sus puertos, ni peticiones al daemon contra
// ella, ni aristas de su grafo) se CONGELA, con todo su grafo; la siguiente
// conexión la despierta. kling.idle=keep la excluye.
//
// Solo 127.0.0.1: Docker publica en todas las interfaces por defecto; aquí no
// se abre nada hacia la red.
package publicar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Maquinas es lo que el publicador necesita del gestor de máquinas.
type Maquinas interface {
	Get(ref string) (*api.Machine, bool)
	List() []*api.Machine
	Thaw(ctx context.Context, ref string) (*api.Machine, error)
	Freeze(ctx context.Context, ref string) (*api.Machine, error)
	GraphFreeze(ctx context.Context, ref string) (*api.Graph, error)
}

// Actividad es lo que el daemon sabe del uso de una máquina sin pasar por sus
// puertos publicados: peticiones en curso contra ella (exec, shell, logs que
// se siguen...), cuándo acabó la última, y sesiones vivas por aristas de su
// grafo (hacia ella o desde ella).
type Actividad struct {
	EnCurso  int
	Ultima   time.Time
	Sesiones int
}

// Plazos por defecto (cada publicador tiene los suyos: los tests los cambian).
const (
	// plazoMarcar: lo que se reintenta conectar con un servicio que aún no
	// escucha (acaba de arrancar o de descongelarse).
	plazoMarcar = 15 * time.Second
	pasoMarcar  = 100 * time.Millisecond
	// udpOcioso: un flujo UDP sin paquetes durante esto se cierra.
	udpOcioso = 60 * time.Second
	// barrido: cada cuánto se quitan las publicaciones de máquinas que ya
	// no existen y se mira la ociosidad.
	barrido = 2 * time.Second
)

// Publicador publica los puertos de las máquinas.
type Publicador struct {
	m    Maquinas
	ruta string // fichero donde se guardan; "" = no se guardan
	logf func(string, ...any)
	// actividad: el uso de la máquina fuera de sus puertos; nil = ninguno.
	actividad func(id string) Actividad
	// escuchar y escucharUDP abren los puertos (los tests ponen otros).
	escuchar    func(addr string) (net.Listener, error)
	escucharUDP func(addr string) (net.PacketConn, error)
	ahora       func() time.Time
	// plazoMarcar y barrido: ver las constantes.
	plazoMarcar, barrido time.Duration

	guardando sync.Mutex
	guardados sync.WaitGroup // guardados en segundo plano aún en curso
	mu        sync.Mutex
	por       map[string]*maquina // id de máquina → sus puertos
	ctx       context.Context
}

type maquina struct {
	id      string
	nombre  string
	puertos []*puerto
	// activas: conexiones abiertas por sus puertos; ultima: la última vez
	// que se abrió o cerró una (UnixNano).
	activas atomic.Int64
	ultima  atomic.Int64
	// despertando evita dos Thaw a la vez de la misma máquina.
	despertando sync.Mutex
}

type puerto struct {
	api.PortPublish
	ln net.Listener   // tcp
	pc net.PacketConn // udp
}

func (p *puerto) cerrar() {
	if p.ln != nil {
		p.ln.Close()
	}
	if p.pc != nil {
		p.pc.Close()
	}
}

// New devuelve un publicador sobre m que guarda sus publicaciones en ruta.
func New(m Maquinas, ruta string, logf func(string, ...any)) *Publicador {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Publicador{
		m: m, ruta: ruta, logf: logf,
		escuchar:    func(a string) (net.Listener, error) { return net.Listen("tcp", a) },
		escucharUDP: func(a string) (net.PacketConn, error) { return net.ListenPacket("udp", a) },
		ahora:       time.Now,
		plazoMarcar: plazoMarcar, barrido: barrido,
		por: map[string]*maquina{},
		ctx: context.Background(),
	}
}

// SetActividad fija de dónde se lee el uso de una máquina fuera de sus
// puertos (ver Actividad). Antes de Arrancar.
func (p *Publicador) SetActividad(f func(id string) Actividad) { p.actividad = f }

// ErrOcupado: el puerto del host ya lo publica otra máquina o lo tiene otro
// proceso.
var ErrOcupado = errors.New("host port in use")

// ErrNoSoportado: el VMM de la máquina no alcanza ese puerto así (UDP en el
// Mac).
var ErrNoSoportado = errors.New("not supported")

// Arrancar recupera las publicaciones guardadas (las de máquinas que ya no
// existen se olvidan) y lanza el barrido: quitar las de máquinas que
// desaparecen y, con ocioso > 0, congelar las ociosas. Vuelve en seguida; todo
// se para con ctx.
func (p *Publicador) Arrancar(ctx context.Context, ocioso func() time.Duration) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	for _, pub := range p.leer() {
		mc, ok := p.m.Get(pub.Machine)
		if !ok {
			continue
		}
		if _, err := p.Publicar(mc, pub.Ports); err != nil {
			p.logf("publish: %s: can't publish again after the restart: %v", mc.Name, err)
		}
	}
	p.guardar()
	go func() {
		t := time.NewTicker(p.barrido)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				p.cerrarTodo()
				return
			case <-t.C:
			}
			p.barrer()
			if ocioso != nil {
				if d := ocioso(); d > 0 {
					p.congelarOciosas(ctx, d)
				}
			}
		}
	}()
}

// Publicar publica ps para mc: se suman a los que ya tuviera, y un puerto del
// host que ya publicaba para ella se sustituye. Todos o ninguno. Devuelve lo
// que queda publicado (con el puerto que tocó a los Host 0).
func (p *Publicador) Publicar(mc *api.Machine, ps []api.PortPublish) (*api.Publication, error) {
	vistos := map[string]bool{}
	for _, q := range ps {
		if err := q.Validate(); err != nil {
			return nil, err
		}
		if q.Host != 0 {
			k := q.ProtoOr() + "/" + strconv.Itoa(q.Host)
			if vistos[k] {
				return nil, fmt.Errorf("host port %d twice", q.Host)
			}
			vistos[k] = true
		}
		if q.ProtoOr() == api.ProtoUDP && len(mc.Forwards) > 0 {
			if _, ok := mc.Forwards[strconv.Itoa(q.Guest)+"/udp"]; !ok {
				return nil, fmt.Errorf("udp %d: %w by this VMM yet (only tcp on macOS)", q.Guest, ErrNoSoportado)
			}
		}
		if q.ProtoOr() == api.ProtoTCP && len(mc.Forwards) > 0 && !mc.Exposes(q.Guest) {
			return nil, fmt.Errorf("guest port %d is not exposed by %s (add it to %s)", q.Guest, mc.Name, api.LabelPorts)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mq := p.por[mc.ID]
	nueva := mq == nil
	if nueva {
		mq = &maquina{id: mc.ID, nombre: mc.Name}
		mq.ultima.Store(p.ahora().UnixNano())
	}
	// Un puerto del host de otra máquina es de ella.
	for id, otra := range p.por {
		if id == mc.ID {
			continue
		}
		for _, q := range ps {
			for _, o := range otra.puertos {
				if q.Host != 0 && o.Host == q.Host && o.ProtoOr() == q.ProtoOr() {
					return nil, fmt.Errorf("127.0.0.1:%d: %w (published for %s)", q.Host, ErrOcupado, otra.nombre)
				}
			}
		}
	}
	// Los que se sustituyen se cierran ANTES de abrir los nuevos: es el
	// mismo puerto del host.
	var quitados []*puerto
	quedan := mq.puertos[:0:0]
	for _, o := range mq.puertos {
		if slices.ContainsFunc(ps, func(q api.PortPublish) bool { return q.Host == o.Host && q.ProtoOr() == o.ProtoOr() }) {
			o.cerrar()
			quitados = append(quitados, o)
			continue
		}
		quedan = append(quedan, o)
	}
	var abiertos []*puerto
	for _, q := range ps {
		pt, err := p.abrir(mq, q)
		if err != nil {
			for _, a := range abiertos {
				a.cerrar()
			}
			// Los sustituidos vuelven a escuchar (mejor esfuerzo).
			for _, o := range quitados {
				if pt2, err2 := p.abrir(mq, o.PortPublish); err2 == nil {
					quedan = append(quedan, pt2)
				}
			}
			mq.puertos = quedan
			if len(mq.puertos) > 0 && nueva {
				p.por[mc.ID] = mq
			}
			return nil, err
		}
		abiertos = append(abiertos, pt)
	}
	mq.puertos = append(quedan, abiertos...)
	sort.Slice(mq.puertos, func(i, j int) bool { return mq.puertos[i].Host < mq.puertos[j].Host })
	if len(mq.puertos) > 0 {
		p.por[mc.ID] = mq
	}
	p.guardarFondo()
	return pubDe(mq), nil
}

// Comprobar dice, sin publicar nada, si ps se podrían publicar para una
// máquina nueva: válidos, sin repetir, ninguno de otra máquina y libres en el
// host (se abren y se cierran). Es la comprobación de antes de crearla.
func (p *Publicador) Comprobar(ps []api.PortPublish) error {
	vistos := map[string]bool{}
	for _, q := range ps {
		if err := q.Validate(); err != nil {
			return err
		}
		if q.Host == 0 {
			continue
		}
		k := q.ProtoOr() + "/" + strconv.Itoa(q.Host)
		if vistos[k] {
			return fmt.Errorf("host port %d twice", q.Host)
		}
		vistos[k] = true
	}
	p.mu.Lock()
	for _, otra := range p.por {
		for _, q := range ps {
			for _, o := range otra.puertos {
				if q.Host != 0 && o.Host == q.Host && o.ProtoOr() == q.ProtoOr() {
					p.mu.Unlock()
					return fmt.Errorf("127.0.0.1:%d: %w (published for %s)", q.Host, ErrOcupado, otra.nombre)
				}
			}
		}
	}
	p.mu.Unlock()
	for _, q := range ps {
		if q.Host == 0 {
			continue
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(q.Host))
		if q.ProtoOr() == api.ProtoUDP {
			pc, err := p.escucharUDP(addr)
			if err != nil {
				return fmt.Errorf("127.0.0.1:%d/udp: %w (%v)", q.Host, ErrOcupado, err)
			}
			pc.Close()
			continue
		}
		ln, err := p.escuchar(addr)
		if err != nil {
			return fmt.Errorf("127.0.0.1:%d: %w (%v)", q.Host, ErrOcupado, err)
		}
		ln.Close()
	}
	return nil
}

func (p *Publicador) abrir(mq *maquina, q api.PortPublish) (*puerto, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(q.Host))
	pt := &puerto{PortPublish: q}
	if q.ProtoOr() == api.ProtoUDP {
		pc, err := p.escucharUDP(addr)
		if err != nil {
			return nil, fmt.Errorf("127.0.0.1:%d/udp: %w (%v)", q.Host, ErrOcupado, err)
		}
		pt.pc = pc
		pt.Host = pc.LocalAddr().(*net.UDPAddr).Port
		go p.servirUDP(mq, pt)
		return pt, nil
	}
	ln, err := p.escuchar(addr)
	if err != nil {
		return nil, fmt.Errorf("127.0.0.1:%d: %w (%v)", q.Host, ErrOcupado, err)
	}
	pt.ln = ln
	pt.Host = ln.Addr().(*net.TCPAddr).Port
	pt.Proto = ""
	go p.servirTCP(mq, pt)
	return pt, nil
}

// Quitar deja de publicar los puertos del host hosts de la máquina id
// (ninguno: todos). Devuelve si había algo.
func (p *Publicador) Quitar(id string, hosts ...int) bool {
	p.mu.Lock()
	mq := p.por[id]
	if mq == nil {
		p.mu.Unlock()
		return false
	}
	var quedan []*puerto
	algo := false
	for _, pt := range mq.puertos {
		if len(hosts) == 0 || slices.Contains(hosts, pt.Host) {
			pt.cerrar()
			algo = true
			continue
		}
		quedan = append(quedan, pt)
	}
	mq.puertos = quedan
	if len(quedan) == 0 {
		delete(p.por, id)
	}
	p.mu.Unlock()
	if algo {
		p.guardar()
	}
	return algo
}

// De son los puertos publicados de la máquina id (nil si ninguno).
func (p *Publicador) De(id string) []api.PortPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mq := p.por[id]; mq != nil {
		return pubDe(mq).Ports
	}
	return nil
}

// Lista son todas las publicaciones, por nombre de máquina.
func (p *Publicador) Lista() []api.Publication {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]api.Publication, 0, len(p.por))
	for _, mq := range p.por {
		out = append(out, *pubDe(mq))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pubDe(mq *maquina) *api.Publication {
	pub := &api.Publication{Machine: mq.id, Name: mq.nombre}
	for _, pt := range mq.puertos {
		pub.Ports = append(pub.Ports, pt.PortPublish)
	}
	return pub
}

// barrer quita las publicaciones de máquinas que ya no existen.
func (p *Publicador) barrer() {
	p.mu.Lock()
	var idos []string
	for id := range p.por {
		if _, ok := p.m.Get(id); !ok {
			idos = append(idos, id)
		}
	}
	p.mu.Unlock()
	for _, id := range idos {
		if p.Quitar(id) {
			p.logf("publish: machine %s is gone; its ports are no longer published", corto(id))
		}
	}
}

func (p *Publicador) cerrarTodo() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, mq := range p.por {
		for _, pt := range mq.puertos {
			pt.cerrar()
		}
	}
}

// ── conexiones ───────────────────────────────────────────────────────────────

func (mq *maquina) abrirConexion(ahora time.Time) {
	mq.activas.Add(1)
	mq.ultima.Store(ahora.UnixNano())
}

func (mq *maquina) cerrarConexion(ahora time.Time) {
	mq.activas.Add(-1)
	mq.ultima.Store(ahora.UnixNano())
}

func (p *Publicador) servirTCP(mq *maquina, pt *puerto) {
	for {
		c, err := pt.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go func() {
			defer c.Close()
			mq.abrirConexion(p.ahora())
			defer func() { mq.cerrarConexion(p.ahora()) }()
			up, err := p.marcar(mq, pt.Guest)
			if err != nil {
				p.logf("publish: 127.0.0.1:%d -> %s:%d: %v", pt.Host, mq.nombre, pt.Guest, err)
				abortar(c)
				return
			}
			defer up.Close()
			splice(c, up)
		}()
	}
}

// abortar cierra con RST: quien conectó ve "connection reset" en seguida, no
// un cierre limpio que parezca una respuesta vacía.
func abortar(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	c.Close()
}

// despertar devuelve la máquina en marcha: la descongela o reanuda si hace
// falta.
func (p *Publicador) despertar(mq *maquina) (*api.Machine, error) {
	mc, ok := p.m.Get(mq.id)
	if !ok {
		return nil, fmt.Errorf("machine %s no longer exists", corto(mq.id))
	}
	if mc.State == api.StateWarm || mc.State == api.StatePaused {
		mq.despertando.Lock()
		defer mq.despertando.Unlock()
		if mc, ok = p.m.Get(mq.id); !ok {
			return nil, fmt.Errorf("machine %s no longer exists", corto(mq.id))
		}
		if mc.State == api.StateWarm || mc.State == api.StatePaused {
			t0 := time.Now()
			out, err := p.m.Thaw(p.contexto(), mq.id)
			if err != nil {
				return nil, fmt.Errorf("waking it up: %w", err)
			}
			p.logf("publish: woke %s up for a connection in %d ms", mc.Name, time.Since(t0).Milliseconds())
			mc = out
		}
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("%s is %s", mc.Name, mc.State)
	}
	return mc, nil
}

// marcar conecta con el puerto del invitado, reintentando un rato si el
// servicio aún no escucha.
func (p *Publicador) marcar(mq *maquina, port int) (net.Conn, error) {
	limite := time.Now().Add(p.plazoMarcar)
	for {
		mc, err := p.despertar(mq)
		if err != nil {
			return nil, err
		}
		c, err := net.DialTimeout("tcp", mc.Addr(port), 2*time.Second)
		if err == nil {
			// En el Mac se conecta a un reenvío de loopback, que acepta
			// siempre y corta con un RST si en el invitado nadie escucha: si
			// ese RST ya está, también es "aún no escucha".
			pre, reset := asomar(c)
			if !reset {
				return &conPrefijo{Conn: c, pre: pre}, nil
			}
			c.Close()
			err = syscall.ECONNRESET
		}
		if time.Now().After(limite) || p.contexto().Err() != nil {
			return nil, err
		}
		time.Sleep(pasoMarcar)
	}
}

// asomar mira, sin esperar, si c ya llegó cortada con un RST; lo que ya
// hubiera llegado se devuelve para entregarlo.
func asomar(c net.Conn) (pre []byte, reset bool) {
	if err := c.SetReadDeadline(time.Now()); err != nil {
		return nil, false
	}
	defer c.SetReadDeadline(time.Time{})
	b := make([]byte, 4096)
	n, err := c.Read(b)
	return b[:n], err != nil && errors.Is(err, syscall.ECONNRESET)
}

// conPrefijo es una conexión con unos bytes ya leídos delante.
type conPrefijo struct {
	net.Conn
	pre []byte
}

func (c *conPrefijo) Read(b []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(b, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

func (c *conPrefijo) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (p *Publicador) contexto() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctx
}

type closeWriter interface{ CloseWrite() error }

// splice copia en los dos sentidos respetando el medio cierre.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// servirUDP reenvía los datagramas de cada cliente por su propio flujo
// hacia el invitado, como el conntrack de un DNAT.
func (p *Publicador) servirUDP(mq *maquina, pt *puerto) {
	var mu sync.Mutex
	flujos := map[string]net.Conn{}
	defer func() {
		mu.Lock()
		for _, f := range flujos {
			f.Close()
		}
		mu.Unlock()
	}()
	buf := make([]byte, 65536)
	for {
		n, de, err := pt.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		k := de.String()
		mu.Lock()
		f := flujos[k]
		mu.Unlock()
		if f == nil {
			mc, err := p.despertar(mq)
			if err != nil {
				p.logf("publish: 127.0.0.1:%d/udp -> %s:%d: %v", pt.Host, mq.nombre, pt.Guest, err)
				continue
			}
			addr := mc.Addr(pt.Guest)
			if a, ok := mc.Forwards[strconv.Itoa(pt.Guest)+"/udp"]; ok {
				addr = a
			}
			f, err = net.Dial("udp", addr)
			if err != nil {
				continue
			}
			mu.Lock()
			flujos[k] = f
			mu.Unlock()
			mq.abrirConexion(p.ahora())
			go func(f net.Conn, de net.Addr) {
				defer func() {
					mu.Lock()
					delete(flujos, k)
					mu.Unlock()
					f.Close()
					mq.cerrarConexion(p.ahora())
				}()
				b := make([]byte, 65536)
				for {
					_ = f.SetReadDeadline(time.Now().Add(udpOcioso))
					n, err := f.Read(b)
					if err != nil {
						return
					}
					mq.ultima.Store(p.ahora().UnixNano())
					if _, err := pt.pc.WriteTo(b[:n], de); err != nil {
						return
					}
				}
			}(f, de)
		}
		mq.ultima.Store(p.ahora().UnixNano())
		_, _ = f.Write(buf[:n])
	}
}

// ── ociosidad ────────────────────────────────────────────────────────────────

// congelarOciosas congela los grupos (un grafo, o una máquina suelta) cuyas
// máquinas publicadas llevan d sin conexiones y en los que nadie usa ninguna
// máquina.
func (p *Publicador) congelarOciosas(ctx context.Context, d time.Duration) {
	ahora := p.ahora()
	p.mu.Lock()
	pubs := map[string]estadoPub{}
	for id, mq := range p.por {
		pubs[id] = estadoPub{time.Unix(0, mq.ultima.Load()), mq.activas.Load()}
	}
	p.mu.Unlock()
	if len(pubs) == 0 {
		return
	}
	todas := p.m.List()
	grupoDe := func(mc *api.Machine) string {
		if g := mc.Labels[api.LabelGraph]; g != "" {
			return "g:" + g
		}
		return "m:" + mc.ID
	}
	grupos := map[string][]*api.Machine{}
	conPub := map[string]bool{}
	for _, mc := range todas {
		k := grupoDe(mc)
		grupos[k] = append(grupos[k], mc)
		if _, ok := pubs[mc.ID]; ok {
			conPub[k] = true
		}
	}
	for k, miembros := range grupos {
		if !conPub[k] || !p.ocioso(miembros, pubs, ahora, d) {
			continue
		}
		var err error
		nombre := miembros[0].Name
		if g := miembros[0].Labels[api.LabelGraph]; g != "" {
			nombre = "graph " + corto(g)
			_, err = p.m.GraphFreeze(ctx, g)
		} else {
			_, err = p.m.Freeze(ctx, miembros[0].ID)
		}
		// Sea como sea, el reloj vuelve a empezar: un fallo no se reintenta en
		// cada vuelta.
		p.mu.Lock()
		for _, mc := range miembros {
			if mq := p.por[mc.ID]; mq != nil {
				mq.ultima.Store(ahora.UnixNano())
			}
		}
		p.mu.Unlock()
		if err != nil {
			p.logf("idle_freeze: %s: %v", nombre, err)
			continue
		}
		p.logf("idle_freeze: froze %s after %s without connections (the next connection wakes it)", nombre, d)
	}
}

// estadoPub es la foto de las conexiones por los puertos de una máquina.
type estadoPub struct {
	ultima  time.Time
	activas int64
}

func (p *Publicador) ocioso(miembros []*api.Machine, pubs map[string]estadoPub, ahora time.Time, d time.Duration) bool {
	corriendo := false
	for _, mc := range miembros {
		if mc.Labels[api.LabelIdle] == "keep" || mc.Transition != "" {
			return false
		}
		switch mc.State {
		case api.StateRunning:
			corriendo = true
		case api.StateWarm, api.StatePaused, api.StateStopped:
		default:
			// Arrancando, congelándose, fallida...: no se toca.
			return false
		}
		if e, ok := pubs[mc.ID]; ok && (e.activas > 0 || ahora.Sub(e.ultima) < d) {
			return false
		}
		if p.actividad != nil && mc.State == api.StateRunning {
			a := p.actividad(mc.ID)
			if a.EnCurso > 0 || a.Sesiones > 0 || (!a.Ultima.IsZero() && ahora.Sub(a.Ultima) < d) {
				return false
			}
		}
	}
	return corriendo
}

// ── persistencia ─────────────────────────────────────────────────────────────

// guardarFondo guarda sin bloquear a quien publica; Esperar aguarda a que
// acaben los guardados en curso.
func (p *Publicador) guardarFondo() {
	p.guardados.Add(1)
	go func() {
		defer p.guardados.Done()
		p.guardar()
	}()
}

// Esperar vuelve cuando no queda ningún guardado en segundo plano.
func (p *Publicador) Esperar() { p.guardados.Wait() }

func (p *Publicador) guardar() {
	if p.ruta == "" {
		return
	}
	// La foto, bajo el cerrojo: dos guardados seguidos no se cruzan y el
	// último en escribir es el último estado.
	p.guardando.Lock()
	defer p.guardando.Unlock()
	b, err := json.MarshalIndent(p.Lista(), "", "  ")
	if err != nil {
		return
	}
	tmp := p.ruta + ".tmp"
	if err := os.MkdirAll(filepath.Dir(p.ruta), 0o755); err != nil {
		p.logf("publish: saving %s: %v", p.ruta, err)
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		p.logf("publish: saving %s: %v", p.ruta, err)
		return
	}
	if err := os.Rename(tmp, p.ruta); err != nil {
		p.logf("publish: saving %s: %v", p.ruta, err)
	}
}

func (p *Publicador) leer() []api.Publication {
	if p.ruta == "" {
		return nil
	}
	b, err := os.ReadFile(p.ruta)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logf("publish: reading %s: %v", p.ruta, err)
		}
		return nil
	}
	var l []api.Publication
	if err := json.Unmarshal(b, &l); err != nil {
		p.logf("publish: %s is not valid (%v); starting with nothing published", p.ruta, err)
		return nil
	}
	return l
}

func corto(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
