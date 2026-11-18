package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	topesGuest(t, 13<<20, 0) // caben dos de 2 MiB (6 MiB de memoria cada una), no tres
	var res [3]*reservaGuest
	for i := range res {
		res[i] = s.reservaGuest("")
		_, err := res[i].lector(strings.NewReader(""), 2<<20)
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

// R8: un inquilino tiene un tope de exec, shell y ficheros abiertos a la vez;
// el admin no. Cada uno devuelve el suyo al acabar.
func TestSesionesGuestPorInquilino(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	exec := `{"cmd":["true"]}`
	if !s.sesionesGuest.tomar("a", maxSesionesInquilino, 0, maxSesionesInquilino) {
		t.Fatal("no se pudo llenar el cupo de a")
	}
	for _, c := range []struct {
		uid          int
		metodo, ruta string
	}{
		{uidA, "POST", "/machines/caja-a/exec"},
		{uidA, "GET", "/machines/caja-a/files?path=/etc/hostname"},
	} {
		if rr := como(t, h, c.uid, c.metodo, c.ruta, exec); rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s con el cupo lleno: %d %s", c.metodo, c.ruta, rr.Code, rr.Body)
		}
	}
	// Lo de a no frena a b ni al admin (sus máquinas no arrancan aquí: basta
	// con que no sea un 429).
	for _, c := range []struct {
		uid  int
		ruta string
	}{{uidB, "/machines/caja-b/exec"}, {uidAdmin, "/machines/caja-a/exec"}} {
		if rr := como(t, h, c.uid, "POST", c.ruta, exec); rr.Code == http.StatusTooManyRequests {
			t.Errorf("%s: 429 por el cupo de otro: %s", c.ruta, rr.Body)
		}
	}
	s.sesionesGuest.soltar("a", maxSesionesInquilino)
	if rr := como(t, h, uidA, "POST", "/machines/caja-a/exec", exec); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("con el cupo libre: %s", rr.Body)
	}
	if total, _ := s.sesionesGuest.enUso("a"); total != 0 {
		t.Fatalf("quedaron %d sesiones apartadas", total)
	}
}

// Un cuerpo declarado que cabe justo no falla al leer su EOF: leído lo
// declarado, no se aparta otro trozo para la última lectura.
func TestGuestDeclaradoJustoLeeElEOF(t *testing.T) {
	const n = 200 << 10
	topesGuest(t, factorMemoriaGuest*n, 0)
	var c cupo
	res := &reservaGuest{c: &c}
	rd, err := res.lector(strings.NewReader(strings.Repeat("x", n)), n)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(rd); err != nil || len(b) != n {
		t.Fatalf("ReadAll: %d bytes, %v", len(b), err)
	}
	res.soltar()
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

// R2 (ronda 5): la reserva de una llamada dura lo que tarde el invitado, y
// dos inquilinos con una llamada grande cada uno ante un invitado lento
// llenaban el presupuesto: desde ahí, 429 para todos, también para las
// llamadas de pocos KiB y para el admin. Las pequeñas van ahora por un carril
// aparte que las grandes no tocan.
func TestGuestCarrilDeLasPequenas(t *testing.T) {
	s, _ := testServer(t)
	topesGuest(t, 12<<20, 6<<20)
	for _, q := range []string{"a", "b"} {
		r := s.reservaGuest(q)
		if err := r.tomar(6 << 20); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.soltar)
	}
	// Otra grande no cabe...
	if rr := llamadaGuest(t, s, "c", 2<<20); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("grande con el presupuesto lleno: %d %s", rr.Code, rr.Body)
	}
	// ...pero las pequeñas de cualquiera, sí.
	for _, q := range []string{"c", "", "a"} {
		if rr := llamadaGuest(t, s, q, 1<<10); rr.Code != http.StatusNotFound {
			t.Fatalf("pequeña de %q con el presupuesto general lleno: %d %s", q, rr.Code, rr.Body)
		}
	}
	if total, _ := s.vueloGuestPeq.enUso(""); total != 0 {
		t.Fatalf("quedaron %d bytes apartados en el carril", total)
	}
	// Una que empieza pequeña y crece pasa entera al general (aquí lleno: no
	// cabe) y sigue donde estaba; al soltar no deja nada en el carril.
	r := s.reservaGuest("c")
	if err := r.tomar(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := r.tomar(guestPequena); err == nil {
		t.Fatal("al crecer tenía que pasar al general, que está lleno")
	}
	if err := r.tomar(1 << 10); err != nil {
		t.Fatal(err)
	}
	r.soltar()
	if pt, _ := s.vueloGuestPeq.enUso(""); pt != 0 {
		t.Fatalf("carril tras soltar: %d", pt)
	}
}

// R2 (ronda 5): decodificada la petición, solo sigue en memoria su cadena;
// lo que se apartó por el búfer del decodificador se devuelve en vez de
// retenerlo mientras el invitado contesta.
func TestGuestPeticionLeidaDevuelveElDecodificador(t *testing.T) {
	s, _ := testServer(t)
	topesGuest(t, 64<<20, 0)
	const n = 4 << 20
	res := s.reservaGuest("")
	rd, err := res.lector(strings.NewReader(strings.Repeat("x", n)), n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatal(err)
	}
	if total, _ := s.vueloGuest.enUso(""); total != factorMemoriaGuest*n {
		t.Fatalf("leída: %d apartados", total)
	}
	res.peticionLeida()
	if total, _ := s.vueloGuest.enUso(""); total != n {
		t.Fatalf("decodificada: %d apartados; quería %d", total, n)
	}
	res.soltar()
	if total, _ := s.vueloGuest.enUso(""); total != 0 {
		t.Fatalf("quedaron %d", total)
	}
}
