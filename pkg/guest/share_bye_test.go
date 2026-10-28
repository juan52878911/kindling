package guest

import (
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
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
