package guest

import (
	"net"
	"testing"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// Congelar con una carpeta viva: el daemon cierra en orden y una escritura
// que salió justo cuando dejaba de leer se quedaba sin respuesta. Antes
// recibía EIO al cortarse la sesión; con share.Bye el agente sabe que no se
// ejecutó y la repite (errnoRetry). Sin Bye, un corte seco sigue siendo EIO:
// ahí no se sabe si el daemon la hizo.
func TestShareSessionByeDevuelveRetry(t *testing.T) {
	for _, tc := range []struct {
		nombre string
		bye    bool
		want   uint32
	}{
		{"con despedida", true, errnoRetry},
		{"corte seco", false, share.EIO},
	} {
		t.Run(tc.nombre, func(t *testing.T) {
			a, b := net.Pipe()
			s := newShareSession(b)
			defer s.close()
			got := make(chan uint32, 1)
			go func() {
				e, _ := s.call(share.OpMkdir, func(e *share.Enc) { e.Str("x"); e.U32(0o755) })
				got <- e
			}()
			// El daemon lee la petición y no la ejecuta.
			if _, err := share.ReadFrame(a); err != nil {
				t.Fatal(err)
			}
			if tc.bye {
				if err := share.WriteFrame(a, share.Bye()); err != nil {
					t.Fatal(err)
				}
			}
			a.Close()
			select {
			case e := <-got:
				if e != tc.want {
					t.Fatalf("errno %d, want %d", e, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("la llamada no volvió")
			}
		})
	}
}

// Un aviso con un tipo de evento que el agente no conoce (de un daemon más
// nuevo) se salta y la sesión sigue: cerrarla dejaría al agente reconectando
// cada 200 ms. Uno mal formado sí la cierra.
func TestShareSessionSaltaEventoDesconocido(t *testing.T) {
	a, b := net.Pipe()
	s := newShareSession(b)
	defer s.close()
	defer a.Close()
	for _, f := range [][]byte{share.Push(share.EventDelete+7, "nuevo"), share.Push(share.EventCreate, "a.txt")} {
		if err := share.WriteFrame(a, f); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case ev := <-s.events:
		if ev.ev != share.EventCreate || ev.path != "a.txt" {
			t.Fatalf("aviso %+v", ev)
		}
	case <-s.done:
		t.Fatal("la sesión se cerró por un evento desconocido")
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó el aviso conocido")
	}

	if err := share.WriteFrame(a, share.Push(share.EventCreate, "../fuera")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("un aviso mal formado no cerró la sesión")
	}
}
