package machine

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// Pruebas del broker de enlaces (broker.go) sin kling-vz: el "kling-vz" es la
// prueba, al otro lado de un socketpair, hablando pkg/linkbroker; y el
// destino, un eco en el loopback al que marcarBroker lleva todo lo que el
// daemon marca (apuntando a qué dirección quería ir). Corre en los dos
// sistemas: la puerta es la misma.

// ecoBroker sustituye el dial del broker: marca siempre al eco y apunta la
// dirección que el daemon resolvió. antes, si no es nil, corre antes de
// marcar (para cambiar el mundo entre resolver y marcar).
type ecoBroker struct {
	mu      sync.Mutex
	pedidas []string
	antes   func()
}

func nuevoEcoBroker(t *testing.T, m *Manager) *ecoBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	e := &ecoBroker{}
	m.marcarBrokerPrueba = func(ctx context.Context, addr string) (net.Conn, error) {
		e.mu.Lock()
		e.pedidas = append(e.pedidas, addr)
		antes := e.antes
		e.mu.Unlock()
		if antes != nil {
			antes()
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	return e
}

// lista es lo que se pidió marcar hasta ahora.
func (e *ecoBroker) lista() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.pedidas...)
}

// parUnix son los dos extremos de un socketpair: kling-vz y el daemon.
func parUnix(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conv := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "par")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	return conv(fds[0]), conv(fds[1])
}

// pedirBroker hace de kling-vz de la máquina origen: pide req al broker del
// manager y devuelve el socket entregado y el arrendamiento.
func pedirBroker(t *testing.T, m *Manager, origen string, req linkbroker.Request) (*net.TCPConn, *net.UnixConn, linkbroker.Response, error) {
	t.Helper()
	req.V = linkbroker.Version
	vz, daemon := parUnix(t)
	go m.atenderBroker(daemon, origen)
	tc, resp, err := linkbroker.Pedir(vz, req, 5*time.Second)
	if err != nil {
		return nil, nil, resp, err
	}
	t.Cleanup(func() { tc.Close(); vz.Close() })
	return tc, vz, resp, nil
}

func ecoPor(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("eco: %q %v", b, err)
	}
	_ = c.SetReadDeadline(time.Time{})
}

// cortadaTCP dice si c dejó de funcionar (EOF o error) en poco tiempo.
func cortadaTCP(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return err != nil
}

func sesionesDe(origen string) int {
	sesionesBroker.mu.Lock()
	defer sesionesBroker.mu.Unlock()
	return sesionesBroker.porOrigen[origen]
}

// Una arista link: el broker comprueba la arista de la máquina que pregunta,
// marca a la dirección del destino (la que el grafo dice) y entrega el
// socket; invalidar el destino lo corta en los dos lados y suelta la sesión.
func TestBrokerEnlace(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	eco := nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")

	tc, arr, resp, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Machine != apiID {
		t.Fatalf("entregó la máquina %s, no la de api (%s)", resp.Machine, apiID)
	}
	e.m.mu.RLock()
	want := direccionEsperada(e.m.byID[apiID], 8081)
	e.m.mu.RUnlock()
	if eco.lista()[0] != want {
		t.Fatalf("marcó a %s, quería %s", eco.lista()[0], want)
	}
	ecoPor(t, tc)
	if n := sesionesDe(web); n != 1 {
		t.Fatalf("%d sesiones de web", n)
	}
	// Congelar api: invalidarSesiones -> invalidarEnlaces (en la escena, un
	// espía), así que se llama al broker directamente.
	if n := invalidarEnlacesBroker(apiID, idVirtual(g.ID, "api")); n != 1 {
		t.Fatalf("invalidar cortó %d sesiones", n)
	}
	if !cortadaTCP(tc) {
		t.Fatal("la sesión siguió tras invalidar el destino")
	}
	fin := make(chan struct{})
	go func() { linkbroker.EsperarFin(arr); close(fin) }()
	select {
	case <-fin:
	case <-time.After(3 * time.Second):
		t.Fatal("el arrendamiento siguió abierto")
	}
	for i := 0; sesionesDe(web) != 0; i++ {
		if i > 300 {
			t.Fatal("la sesión no se soltó")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// La puerta: solo las aristas de la máquina que pregunta, nunca el agente del
// invitado, y un destino que cambia mientras se marca no se entrega.
func TestBrokerRechaza(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	eco := nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")

	casos := []struct {
		nombre string
		origen string
		req    linkbroker.Request
		error  string
	}{
		{"sin arista", web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "db.graph", Port: 5432}, "has no link edge"},
		{"otro puerto", web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 9090}, "has no link edge"},
		{"al revés", apiID, linkbroker.Request{Kind: linkbroker.KindLink, Host: "web.graph", Port: 8000}, "has no link edge"},
		{"el agente del invitado", web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: api.GuestPort}, "guest agent"},
		{"máquina fuera de un grafo", "ffffffffffffffff", linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081}, "not a node of a graph"},
		{"credencial que no tiene", web, linkbroker.Request{Kind: linkbroker.KindMachine, Machine: idVirtual(g.ID, "api"), Owner: g.ID, Port: 8081}, "has no credential"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			_, _, resp, err := pedirBroker(t, e.m, c.origen, c.req)
			if err == nil || !strings.Contains(err.Error(), c.error) {
				t.Fatalf("esperaba %q: %v", c.error, err)
			}
			if resp.Reason != linkbroker.ReasonMachineUnavailable {
				t.Fatalf("motivo %q", resp.Reason)
			}
		})
	}
	if len(eco.lista()) != 0 {
		t.Fatalf("un rechazo llegó a marcar: %v", eco.lista())
	}

	// El destino se congela entre resolver y marcar: no se entrega.
	eco.mu.Lock()
	eco.antes = func() {
		e.m.mu.Lock()
		e.m.byID[apiID].State = api.StateWarm
		e.m.mu.Unlock()
	}
	eco.mu.Unlock()
	_, _, _, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081})
	if err == nil || !strings.Contains(err.Error(), "changed while connecting") {
		t.Fatalf("un destino que cambió se entregó: %v", err)
	}
}

// Por encima del tope de sesiones de una máquina, busy sin resolver nada.
func TestBrokerTopePorOrigen(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	eco := nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web := e.maquina(g.ID, "web")
	sesionesBroker.mu.Lock()
	sesionesBroker.porOrigen[web] = maxSesionesBroker
	sesionesBroker.mu.Unlock()
	t.Cleanup(func() {
		sesionesBroker.mu.Lock()
		delete(sesionesBroker.porOrigen, web)
		sesionesBroker.mu.Unlock()
	})
	_, _, resp, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081})
	if err == nil || resp.Reason != linkbroker.ReasonBusy || len(eco.lista()) != 0 {
		t.Fatalf("pasó el tope: %+v %v", resp, err)
	}
}

// kling db attach por el broker: la credencial tiene que estar en el almacén
// del agente, y la puerta es la del modelo A (comprobarCopiaLocked). Cambiar
// el dueño del agente corta lo que pidió.
func TestBrokerAttach(t *testing.T) {
	m := escenaModeloA(t)
	eco := nuevoEcoBroker(t, m)
	cred := credproxy.Credential{Env: "PGPASSWORD", Domain: "copia.db.internal", Placeholder: "kling-cred-a", Secret: "s",
		Kind: credproxy.KindPostgres, Port: 5432, User: "app", Database: "appdb",
		UpstreamMachine: idCopia, UpstreamOwner: "local", UpstreamTLS: credproxy.UpstreamTLSDisable}
	req := linkbroker.Request{Kind: linkbroker.KindMachine, Machine: idCopia, Owner: "local", Port: 5432}
	if _, _, _, err := pedirBroker(t, m, idAgente, req); err == nil || !strings.Contains(err.Error(), "has no credential") {
		t.Fatalf("sin la credencial en el almacén: %v", err)
	}
	if err := m.guardarCredenciales(idAgente, []credproxy.Credential{cred}); err != nil {
		t.Fatal(err)
	}
	for _, mal := range []linkbroker.Request{
		{Kind: linkbroker.KindMachine, Machine: idCopia, Owner: "otro", Port: 5432},
		{Kind: linkbroker.KindMachine, Machine: idCopia, Owner: "local", Port: 5433},
		{Kind: linkbroker.KindMachine, Machine: idOtra, Owner: "local", Port: 5432},
	} {
		if _, _, _, err := pedirBroker(t, m, idAgente, mal); err == nil {
			t.Fatalf("aceptada: %+v", mal)
		}
	}
	tc, _, resp, err := pedirBroker(t, m, idAgente, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Machine != idCopia {
		t.Fatalf("máquina %s", resp.Machine)
	}
	m.mu.RLock()
	want := direccionEsperada(m.byID[idCopia], 5432)
	m.mu.RUnlock()
	if l := eco.lista(); l[len(l)-1] != want {
		t.Fatalf("marcó a %v, quería %s", l, want)
	}
	ecoPor(t, tc)
	// La copia deja de estar lista: la puerta ya no deja pasar.
	m.mu.Lock()
	m.byID[idCopia].Labels[api.LabelDBState] = "preparing"
	m.mu.Unlock()
	if _, _, _, err := pedirBroker(t, m, idAgente, req); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("copia no lista: %v", err)
	}
	// Lo que pidió el agente se corta si el agente cambia. La petición
	// rechazada de arriba ya tiene su respuesta, pero su sesión sale del
	// registro con un defer del servidor que puede no haber corrido aún:
	// esperar a que solo quede la que sigue viva, o se contarían dos.
	esperarSesionesBroker(t, idAgente, 1)
	if n := invalidarOrigenBroker(idAgente); n != 1 || !cortadaTCP(tc) {
		t.Fatalf("invalidarOrigen cortó %d", n)
	}
}

// Invalidar por el objetivo corta también la sesión que aún resuelve (el
// destino dormido despertando).
func TestBrokerInvalidaMientrasResuelve(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	e.m.mu.Lock()
	e.m.byID[apiID].State = api.StateWarm
	e.m.mu.Unlock()
	soltar := make(chan struct{})
	despertarNodoGrafo = func(ctx context.Context, m *Manager, id string) error {
		<-soltar
		m.mu.Lock()
		m.byID[id].State = api.StateRunning
		m.mu.Unlock()
		return nil
	}
	hecho := make(chan error, 1)
	go func() {
		_, _, _, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081})
		hecho <- err
	}()
	for i := 0; sesionesDe(web) == 0; i++ {
		if i > 300 {
			t.Fatal("la petición no llegó al broker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := invalidarEnlacesBroker(idVirtual(g.ID, "api")); n != 1 {
		t.Fatalf("invalidar el nodo cortó %d", n)
	}
	close(soltar)
	select {
	case err := <-hecho:
		if err == nil {
			t.Fatal("una sesión invalidada mientras resolvía se entregó")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la petición no terminó")
	}
	// El despertar sigue en segundo plano: se espera a que acabe antes de
	// que la escena devuelva sus ganchos.
	for i := 0; ; i++ {
		e.m.despMu.Lock()
		n := len(e.m.desp)
		e.m.despMu.Unlock()
		if n == 0 {
			break
		}
		if i > 300 {
			t.Fatal("el despertar no terminó")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// esperarSesionesBroker espera (con plazo) a que el origen id tenga exactamente
// n sesiones registradas en el broker.
func esperarSesionesBroker(t *testing.T, id string, n int) {
	t.Helper()
	limite := time.Now().Add(5 * time.Second)
	for {
		sesionesBroker.mu.Lock()
		hay := sesionesBroker.porOrigen[id]
		sesionesBroker.mu.Unlock()
		if hay == n {
			return
		}
		if time.Now().After(limite) {
			t.Fatalf("el origen %s tiene %d sesiones en el broker, esperaba %d", id, hay, n)
		}
		time.Sleep(time.Millisecond)
	}
}
