package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// postJSON manda un POST al manejador del puente y devuelve el código y el id
// de sesión de la respuesta.
func postJSON(b *bridge, sid, body string) (int, string) {
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	rec := httptest.NewRecorder()
	b.handlePost(rec, req)
	return rec.Code, rec.Header().Get(SessionHeader)
}

// La respuesta del cliente a una petición del servidor (sampling, elicitation,
// roots) lleva id y no method. No espera nada: antes se trataba como petición y
// se quedaba colgada hasta el tope de respuesta.
func TestRespuestaDelClienteSeEntregaSinEsperar(t *testing.T) {
	b := testBridge("cat")
	b.reqTimeout = 3 * time.Second
	defer b.closeAll()

	code, sid := postJSON(b, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if code != 200 || sid == "" {
		t.Fatalf("initialize devolvió %d (sesión %q)", code, sid)
	}

	t0 := time.Now()
	code, _ = postJSON(b, sid, `{"jsonrpc":"2.0","id":"srv-7","result":{"roots":[]}}`)
	if code != 202 {
		t.Fatalf("la respuesta del cliente devolvió %d, se esperaba 202", code)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("la respuesta del cliente tardó %s: se trató como petición", d)
	}

	// Y con error en lugar de result, igual.
	code, _ = postJSON(b, sid, `{"jsonrpc":"2.0","id":"srv-8","error":{"code":-1,"message":"no"}}`)
	if code != 202 {
		t.Fatalf("un error del cliente devolvió %d, se esperaba 202", code)
	}

	b.mu.Lock()
	inflight := b.sessions[sid].inflight
	b.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("quedaron %d peticiones contadas en vuelo tras contestar", inflight)
	}
}

// Un stream SSE abierto más tiempo que b.idle no se cosecha ni se recicla: el
// cliente está conectado esperando notificaciones.
func TestStreamAbiertoNoSeCosechaNiSeRecicla(t *testing.T) {
	b := testBridge("cat")
	b.idle = 30 * time.Millisecond
	defer b.closeAll()

	s, _, err := b.resolve("", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b.release(s)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/mcp", nil).WithContext(ctx)
	req.Header.Set(SessionHeader, s.id)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); b.handleGet(httptest.NewRecorder(), req) }()

	esperar(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return s.streams == 1 })

	// Muy por encima de idle y de la gracia de reciclado.
	b.mu.Lock()
	s.lastUse = time.Now().Add(-time.Hour)
	b.mu.Unlock()
	b.reapOnce()
	b.mu.Lock()
	_, sigue := b.sessions[s.id]
	candidata := b.oldestReclaimableLocked()
	b.mu.Unlock()
	if !sigue {
		t.Fatal("la cosecha se llevó una sesión con su stream SSE abierto")
	}
	if candidata != nil {
		t.Fatal("una sesión con stream abierto salió como candidata a reciclado")
	}

	cancel()
	wg.Wait()
	b.mu.Lock()
	streams := s.streams
	s.lastUse = time.Now().Add(-time.Hour)
	b.mu.Unlock()
	if streams != 0 {
		t.Fatalf("streams=%d tras cerrar el GET", streams)
	}
	b.reapOnce()
	b.mu.Lock()
	_, sigue = b.sessions[s.id]
	b.mu.Unlock()
	if sigue {
		t.Fatal("cerrado el stream, la sesión ociosa no se cosechó")
	}
}

// Una petición en curso (una tools/call larga) protege a su sesión del reciclado
// por tope y de la cosecha, y al terminar deja lastUse en ese momento.
func TestPeticionEnCursoNoSeReciclaNiSeCosecha(t *testing.T) {
	b := testBridge("cat")
	b.idle = 30 * time.Millisecond
	b.maxSessions = 1
	defer b.closeAll()

	s, _, err := b.resolve("", true) // queda contada en vuelo
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b.mu.Lock()
	s.lastUse = time.Now().Add(-time.Hour)
	b.mu.Unlock()

	if _, _, err := b.resolve("", true); err == nil {
		t.Fatal("con tope 1, un initialize recicló la sesión que tiene una petición en curso")
	}
	b.reapOnce()
	b.mu.Lock()
	_, sigue := b.sessions[s.id]
	b.mu.Unlock()
	if !sigue {
		t.Fatal("la cosecha se llevó una sesión con una petición en curso")
	}

	b.release(s)
	b.mu.Lock()
	reciente := time.Since(s.lastUse) < time.Second
	candidata := b.oldestReclaimableLocked()
	b.mu.Unlock()
	if !reciente {
		t.Fatal("release no refrescó lastUse al terminar la petición")
	}
	if candidata != nil {
		t.Fatal("recién terminada la petición, la sesión ya era reciclable")
	}
}

// El lanzamiento va fuera del candado, pero la plaza se reserva dentro: N
// initialize simultáneos no superan el tope entre todos.
func TestInitializeConcurrentesRespetanElTope(t *testing.T) {
	b := testBridge("cat")
	b.maxSessions = 2
	defer b.closeAll()

	var (
		wg sync.WaitGroup
		mu sync.Mutex
		ok int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Sin release: siguen en vuelo y nadie puede reciclarlas.
			if _, _, err := b.resolve("", true); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("%d initialize entraron con tope 2", ok)
	}
	b.mu.Lock()
	n, sp := len(b.sessions), b.spawning
	b.mu.Unlock()
	if n != 2 || sp != 0 {
		t.Fatalf("sesiones=%d spawning=%d, se esperaba 2 y 0", n, sp)
	}
}

func esperar(t *testing.T, cond func() bool) {
	t.Helper()
	plazo := time.Now().Add(3 * time.Second)
	for time.Now().Before(plazo) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("la condición no se cumplió a tiempo")
}
