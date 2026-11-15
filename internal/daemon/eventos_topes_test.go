package daemon

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// R7: GET /events tiene tope por inquilino y en total; pasado, 429 sin
// suscribir nada.
func TestEventosConTope(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()

	s.eventosDe = map[string]int{"a": maxEventosInquilino}
	if rr := eventosComo(t, h, uidA); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("inquilino por encima de su tope: %d %s", rr.Code, rr.Body)
	}
	if s.eventosDe["a"] != maxEventosInquilino {
		t.Fatalf("un rechazo cambió la cuenta: %d", s.eventosDe["a"])
	}

	var bajas []func()
	for i := 0; i < maxSuscriptoresEventos; i++ {
		_, baja := s.bus.Subscribe()
		bajas = append(bajas, baja)
	}
	defer func() {
		for _, b := range bajas {
			b()
		}
	}()
	if rr := eventosComo(t, h, uidAdmin); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("por encima del tope total: %d %s", rr.Code, rr.Body)
	}
}

// eventosComo es como() para GET /events, con un plazo: si el tope no
// rechaza, el flujo se abre y se cerraría solo al cortar el cliente.
func eventosComo(t *testing.T, h http.Handler, uid int) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	ll := Llamante{UID: uid, GID: uid, Conocido: true}
	req = req.WithContext(context.WithValue(req.Context(), claveLlamante{}, ll))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// R7: un cliente de GET /events que deja de leer se corta por el plazo de
// escritura en vez de retener su conexión para siempre.
func TestEventosCortaAlQueNoLee(t *testing.T) {
	antes := eventosEscritura
	eventosEscritura = 200 * time.Millisecond
	t.Cleanup(func() { eventosEscritura = antes })

	s, _ := testServer(t)
	fin := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(fin)
		s.handleEvents(w, r)
	}))
	defer srv.Close()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /events HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Hasta la cabecera, y después ya no se lee nada.
	if l, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.Contains(l, "200") {
		t.Fatalf("respuesta: %q %v", l, err)
	}
	grande := strings.Repeat("x", 64<<10)
	parar := make(chan struct{})
	defer close(parar)
	go func() {
		for {
			select {
			case <-parar:
				return
			case <-fin:
				return
			default:
				s.bus.Publish(api.Event{Type: api.EvStarted, Message: grande})
				time.Sleep(time.Millisecond)
			}
		}
	}()
	select {
	case <-fin:
	case <-time.After(15 * time.Second):
		t.Fatal("GET /events sigue escribiendo a un cliente que no lee")
	}
}
