package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// topesGuest baja los topes de /guest para el test.
func topesGuest(t *testing.T, total, inquilino int64) {
	t.Helper()
	vt, vi := guestVueloMax, guestVueloInquilino
	guestVueloMax, guestVueloInquilino = total, inquilino
	t.Cleanup(func() { guestVueloMax, guestVueloInquilino = vt, vi })
}

// llamadaGuest manda a handleGuest una llamada con un cuerpo de n bytes, como
// admin (tenant "") o como el inquilino t. La máquina no existe: si el cuerpo
// cupo, el handler llega a buscarla y contesta 404.
func llamadaGuest(t *testing.T, s *Server, tenant string, n int) *httptest.ResponseRecorder {
	t.Helper()
	cuerpo, _ := json.Marshal(api.GuestRequest{Path: "/mcp", Body: strings.Repeat("a", n)})
	r := httptest.NewRequest("POST", "/machines/nadie/guest", bytes.NewReader(cuerpo))
	r.SetPathValue("ref", "nadie")
	if tenant != "" {
		r = r.WithContext(context.WithValue(r.Context(), claveRol{}, Rol{Tenant: tenant}))
	}
	rr := httptest.NewRecorder()
	s.handleGuest(rr, r)
	return rr
}

// R2: /guest admitía cuerpos de 128 MiB sin tope en vuelo, y unas pocas
// llamadas así en paralelo llevaban al daemon al OOM. Ahora los bytes en vuelo
// tienen un tope global y otro por inquilino; lo que no cabe es un 429 antes
// de llegar al invitado, y al acabar cada llamada devuelve lo suyo.
func TestGuestPresupuestoEnVuelo(t *testing.T) {
	s, _ := testServer(t)
	topesGuest(t, 12<<20, 6<<20) // de memoria: factorMemoriaGuest por byte

	// Una llamada normal cabe y no deja nada apartado.
	if rr := llamadaGuest(t, s, "a", 1<<10); rr.Code != http.StatusNotFound {
		t.Fatalf("llamada pequeña: %d %s", rr.Code, rr.Body)
	}
	// Más que la parte de un inquilino: 429, con Retry-After.
	rr := llamadaGuest(t, s, "a", 3<<20)
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" ||
		!strings.Contains(rr.Body.String(), "tenant's share") {
		t.Fatalf("inquilino por encima de su parte: %d %q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body)
	}
	// El admin solo tiene el tope global: 3 MiB caben...
	if rr := llamadaGuest(t, s, "", 3<<20); rr.Code != http.StatusNotFound {
		t.Fatalf("admin, 3 MiB: %d %s", rr.Code, rr.Body)
	}
	// ...salvo que otras llamadas en vuelo ya tengan el presupuesto.
	otra := s.reservaGuest("b")
	if err := otra.tomar(6 << 20); err != nil {
		t.Fatal(err)
	}
	if rr := llamadaGuest(t, s, "", 3<<20); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("admin con el presupuesto ocupado: %d %s", rr.Code, rr.Body)
	}
	// Lo de b no frena a a dentro de su parte.
	if rr := llamadaGuest(t, s, "a", 1<<20); rr.Code != http.StatusNotFound {
		t.Fatalf("a con b ocupando lo suyo: %d %s", rr.Code, rr.Body)
	}
	otra.soltar()
	if rr := llamadaGuest(t, s, "", 3<<20); rr.Code != http.StatusNotFound {
		t.Fatalf("admin con el presupuesto libre: %d %s", rr.Code, rr.Body)
	}
	if total, _ := s.vueloGuest.enUso(""); total != 0 {
		t.Fatalf("quedaron %d bytes apartados tras acabar todas", total)
	}
}

// La respuesta del invitado también cuenta. Si no cabe, el error dice que la
// llamada ya llegó al invitado: repetirla no es inocuo.
func TestGuestPresupuestoRespuesta(t *testing.T) {
	topesGuest(t, 1<<20, 1<<20)
	addr, _ := invitado(t, strings.Repeat("r", 2<<20))
	var c cupo
	res := &reservaGuest{c: &c}
	_, code, err := proxyGuestCon(context.Background(), addr, api.GuestRequest{Path: "/mcp", Body: "{}"}, porDial(addr), res)
	if err == nil || code != http.StatusBadGateway || !strings.Contains(err.Error(), "the guest answered") {
		t.Fatalf("respuesta de 2 MiB con 1 MiB de presupuesto: %d %v", code, err)
	}
	res.soltar()
	if total, _ := c.enUso(""); total != 0 {
		t.Fatalf("quedaron %d bytes apartados", total)
	}
	// Con sitio, la misma respuesta pasa.
	topesGuest(t, 8<<20, 8<<20)
	out, _, err := proxyGuestCon(context.Background(), addr, api.GuestRequest{Path: "/mcp", Body: "{}"}, porDial(addr), res)
	if err != nil || len(out.Body) != 2<<20 {
		t.Fatalf("con sitio: %v, %d bytes", err, len(out.Body))
	}
	res.soltar()
}

// Con Content-Length, la petición se aparta entera al empezar: de varias
// grandes a la vez entran las que caben y las demás se rechazan sin leer
// nada. Contando solo según se leía, todas avanzaban a medias y, con el
// presupuesto agotado, no acababa ninguna.
func TestGuestPresupuestoDeclarado(t *testing.T) {
	s, _ := testServer(t)
	topesGuest(t, 7<<20, 0) // caben dos de 1 MiB (3 MiB de memoria cada una), no tres
	var res [3]*reservaGuest
	for i := range res {
		res[i] = s.reservaGuest("")
		_, err := res[i].lector(strings.NewReader(""), 1<<20)
		if (err == nil) != (i < 2) {
			t.Fatalf("lector %d: %v", i, err)
		}
	}
	for _, r := range res {
		r.soltar()
	}
	if total, _ := s.vueloGuest.enUso(""); total != 0 {
		t.Fatalf("quedaron %d bytes apartados", total)
	}
}

func TestCupoPorInquilino(t *testing.T) {
	var c cupo
	if !c.tomar("a", 5, 10, 6) || c.tomar("a", 2, 10, 6) {
		t.Fatal("el tope por inquilino no manda")
	}
	if !c.tomar("b", 5, 10, 6) || c.tomar("", 1, 10, 6) {
		t.Fatal("el tope global no manda")
	}
	c.soltar("a", 5)
	if total, a := c.enUso("a"); total != 5 || a != 0 {
		t.Fatalf("tras soltar: total %d, a %d", total, a)
	}
	if !c.tomar("", 5, 10, 6) {
		t.Fatal("el admin no cabe con sitio libre")
	}
}
