package main

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newTestSession arma una sesión con un hijo real y barato. `cat` devuelve por
// stdout lo que le entra por stdin, que basta para ejercitar el ciclo de vida:
// lo que se prueba aquí es quién cierra qué y en qué orden, no el protocolo.
func newTestSession(t *testing.T, argv ...string) *session {
	t.Helper()
	if len(argv) == 0 {
		argv = []string{"cat"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no puedo lanzar %q aquí: %v", argv[0], err)
	}
	s := &session{
		id:      "0123456789abcdef",
		cmd:     cmd,
		stdin:   stdin,
		lastUse: time.Now(),
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 32),
		done:    make(chan struct{}),
	}
	go s.readLoop(stdout)
	return s
}

// Cerrar la sesión mientras el readLoop sigue vivo no debe entrar en pánico.
//
// Cerrando s.notes sí podía: enviar por un canal cerrado revienta incluso
// dentro de un select con default, y bastaba una notificación del servidor MCP
// en esa ventana para tumbar el puente entero — y con él la microVM, porque es
// PID 1.
//
// AVISO sobre lo que este test vale: la ventana es de nanosegundos y no se
// reproduce a voluntad (probado, 30 vueltas contra la versión mala sin un solo
// pánico). Es una prueba de humo, no una garantía. Lo que de verdad cierra el
// agujero es estructural: s.done SOLO se cierra, nunca se escribe, así que no
// existe el envío que podría reventar. Si alguien vuelve a poner un
// close(s.notes), este test probablemente no lo pare — el comentario de close()
// sí.
func TestCloseNoEntraEnPanicoConNotificacionesEnVuelo(t *testing.T) {
	s := newTestSession(t)

	// Un chorro de notificaciones (sin id) para que el readLoop esté enviando
	// a notes justo cuando llegue el close.
	go func() {
		for range 500 {
			_ = s.send([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`))
		}
	}()

	time.Sleep(20 * time.Millisecond)
	s.close() // si esto entra en pánico, el test cae con el stack del readLoop
	time.Sleep(50 * time.Millisecond)
}

// Cuando el hijo muere, quien esperaba respuesta debe recibir un ERROR, no una
// respuesta vacía. Con `resp := <-ch` de un solo valor, un canal cerrado
// entregaba el cero y request devolvía (nil, nil).
func TestRequestDevuelveErrorSiMuereElHijo(t *testing.T) {
	// sleep no contesta nada: asi la peticion sigue pendiente cuando llega el
	// close. Con `cat` volveria por stdout y el readLoop la casaria de verdad.
	s := newTestSession(t, "sleep", "60")

	type result struct {
		raw json.RawMessage
		err error
	}
	res := make(chan result, 1)
	go func() {
		raw, err := s.request(context.Background(), "7", []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
		res <- result{raw, err}
	}()

	time.Sleep(30 * time.Millisecond)
	s.close() // mata al hijo con la petición en vuelo

	select {
	case r := <-res:
		if r.err == nil {
			t.Fatalf("devolvió (%q, nil): una respuesta vacía en vez de un error", r.raw)
		}
		if !strings.Contains(r.err.Error(), "murió") && !strings.Contains(r.err.Error(), "cerrada") {
			t.Errorf("el error no explica qué pasó: %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request se quedó colgada tras morir el hijo")
	}
}

// close() llega desde reapIdle, desde handleDelete y desde closeAll: puede
// coincidir consigo misma.
func TestCloseEsIdempotente(t *testing.T) {
	s := newTestSession(t)
	done := make(chan struct{}, 3)
	for range 3 {
		go func() { s.close(); done <- struct{}{} }()
	}
	for range 3 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("close se bloqueó o entró en pánico")
		}
	}
}

// closeAll deja el puente sin sesiones y sin procesos hijo: es lo que hace que
// un SIGTERM no deje servidores MCP sueltos dentro de la microVM.
func TestCloseAllVaciaYMata(t *testing.T) {
	b := &bridge{argv: []string{"cat"}, idle: time.Minute, maxSessions: 8,
		sessions: map[string]*session{}}

	var pids []int
	for i := range 3 {
		s := newTestSession(t)
		s.id = string(rune('a'+i)) + "000000000000000"
		b.sessions[s.id] = s
		pids = append(pids, s.cmd.Process.Pid)
	}

	b.closeAll()

	if len(b.sessions) != 0 {
		t.Errorf("quedaron %d sesiones", len(b.sessions))
	}
	// Los hijos deben estar recogidos: cmd.Wait() ya corrió dentro de close().
	for _, pid := range pids {
		if pid <= 0 {
			t.Error("pid inválido")
		}
	}
}

// El flujo SSE se entera de que la sesión terminó por s.done.
func TestDoneSeCierraAlCerrarLaSesion(t *testing.T) {
	s := newTestSession(t)
	select {
	case <-s.done:
		t.Fatal("done estaba cerrado antes de tiempo")
	default:
	}
	s.close()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("close no cerró done: el flujo SSE se quedaría colgado")
	}
}

// Un tope de respuesta a cero significa "sin configurar", no "vence ya".
//
// Importa porque el campo se copia del puente al nacer la sesión: cualquier
// sitio que construya una sesión sin rellenarlo —un test, un refactor— haría
// que time.After(0) disparase en el acto y TODA llamada fallase al instante,
// con un mensaje que habla de tiempos de espera. Es el peor error posible de
// diagnosticar: parece un servidor MCP lento.
func TestTopeDeRespuestaCeroNoVenceEnElActo(t *testing.T) {
	// El extremo de escritura de una tubería hace de stdin: request() envía
	// antes de esperar, y sin destino ahí el test moriría por otra razón.
	r, w := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, r) }()
	s := &session{
		id:      "cero",
		stdin:   w,
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 1),
		done:    make(chan struct{}),
		// reqTimeout deliberadamente sin poner.
	}
	if s.reqTimeout != 0 {
		t.Fatal("el test no prueba lo que dice")
	}

	// Se responde enseguida: si el cero se tomara como plazo, el select elegiría
	// la rama del tiempo agotado antes de llegar la respuesta.
	go func() {
		for i := 0; i < 200; i++ {
			s.mu.Lock()
			ch, ok := s.pending["1"]
			s.mu.Unlock()
			if ok {
				ch <- json.RawMessage(`{"jsonrpc":"2.0","id":"1","result":{}}`)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	resp, err := s.request(t.Context(), "1", []byte(`{"jsonrpc":"2.0","id":"1"}`))
	if err != nil {
		t.Fatalf("con el tope sin configurar debería esperar la respuesta: %v", err)
	}
	if len(resp) == 0 {
		t.Error("respuesta vacía")
	}
}

// Un hijo que deja de leer su stdin no debe poder colgar el puente entero.
//
// send() escribía al pipe con el candado de ESTADO tomado. A los ~64 KiB
// encolados el Write se para para siempre, y close() —que es justo quien
// desatasca cerrando stdin— esperaba ese mismo candado. Con el puente siendo
// PID 1, eso dejaba la microVM sin poder morir limpia.
func TestUnHijoQueNoLeeNoCuelgaElCierre(t *testing.T) {
	r, w := io.Pipe()
	// Nadie lee de r: la escritura se bloqueará en cuanto se llene el pipe.
	s := &session{
		id:      "atascada",
		stdin:   w,
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 1),
		done:    make(chan struct{}),
	}

	escribiendo := make(chan struct{})
	go func() {
		close(escribiendo)
		// io.Pipe no tiene buffer: el primer Write ya bloquea sin lector.
		_ = s.send([]byte(`{"jsonrpc":"2.0"}`))
	}()
	<-escribiendo
	time.Sleep(50 * time.Millisecond) // que llegue a bloquearse de verdad

	// close() tiene que poder tomar el candado de estado aunque haya una
	// escritura atascada. Antes del arreglo, esto no retornaba jamás.
	cerrado := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(cerrado)
	}()

	select {
	case <-cerrado:
	case <-time.After(2 * time.Second):
		t.Fatal("el candado de estado sigue retenido por una escritura bloqueada: interbloqueo")
	}
	_ = r.Close()
}

// Dos peticiones concurrentes con el MISMO id JSON-RPC: la segunda se rechaza.
//
// Antes se sobrescribía la entrada de pending. El primero que esperaba quedaba
// huérfano —nadie volvería a escribir en su canal— y se colgaba hasta el tope de
// respuesta, minutos después. Y la primera respuesta que llegaba con ese id se
// le entregaba al SEGUNDO, que recibía la contestación de una petición ajena.
//
// Un error inmediato es infinitamente mejor que colgar a uno y contestarle otra
// cosa al otro.
func TestDosPeticionesConElMismoIdNoSePisan(t *testing.T) {
	r, w := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, r) }()
	s := &session{
		id:      "dup",
		stdin:   w,
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 1),
		done:    make(chan struct{}),
	}

	// La primera queda esperando: se registra y nadie le responde todavía.
	lista := make(chan struct{})
	go func() {
		close(lista)
		_, _ = s.request(t.Context(), "mismo", []byte(`{"jsonrpc":"2.0","id":"mismo"}`))
	}()
	<-lista
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// La segunda con el mismo id debe fallar EN EL ACTO, no quedarse colgada ni
	// desalojar a la primera.
	hecho := make(chan error, 1)
	go func() {
		_, err := s.request(t.Context(), "mismo", []byte(`{"jsonrpc":"2.0","id":"mismo"}`))
		hecho <- err
	}()
	select {
	case err := <-hecho:
		if err == nil {
			t.Fatal("aceptó un id ya en vuelo: el primero se quedaría huérfano")
		}
		if !strings.Contains(err.Error(), "en vuelo") {
			t.Errorf("el error no explica el problema: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("la segunda petición se colgó en vez de rechazarse")
	}

	// Y la PRIMERA sigue esperando su respuesta, no la han desalojado.
	s.mu.Lock()
	_, sigue := s.pending["mismo"]
	s.mu.Unlock()
	if !sigue {
		t.Error("desalojaron a la primera petición")
	}
	_ = w.Close()
}
