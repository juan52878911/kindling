package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// En la ruta por servicio solo un initialize abre sesión: un POST suelto se
// sirve, pero el id que devuelva el invitado no se fija ni llega al cliente.
// Y en un servicio aislado, un POST que no es initialize no crea máquina.
func TestRutaPorServicioSoloAcunaEnInitialize(t *testing.T) {
	_, addrs := invitadosFalsos(t, 2)
	d := &daemonAislado{service: "notas", invitados: addrs, compartido: true}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	for i := 0; i < 5; i++ {
		rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoCall)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST sin sesión: %d %s", rec.Code, rec.Body)
		}
		if sid := rec.Header().Get(SessionHeader); sid != "" {
			t.Fatalf("un tools/call sin sesión acuñó la sesión %q", sid)
		}
	}
	if n := gw.sesionesDirectas("default"); n != 0 {
		t.Fatalf("quedan %d sesiones acuñadas sin initialize", n)
	}

	_, addrs = invitadosFalsos(t, 2)
	da := &daemonAislado{service: "notas", invitados: addrs}
	h = New(api.NewClient(levantarDaemonAislado(t, da)), 5*time.Minute, false, 0, "").Handler("")
	if rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoCall); rec.Code != http.StatusBadRequest {
		t.Fatalf("aislado, POST sin initialize: %d %s", rec.Code, rec.Body)
	}
	if da.creadas != 0 {
		t.Fatal("un POST que no es initialize creó una máquina aislada")
	}
}

// Un tenant no acuña más de MaxSesiones a la vez, sumando directas,
// aisladas y del agregador; cerrar una deja hueco.
func TestTopeDeSesionesPorTenant(t *testing.T) {
	_, addrs := invitadosFalsos(t, 2)
	d := &daemonAislado{service: "notas", invitados: addrs, compartido: true}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	gw.MaxSesiones = 3
	h := gw.Handler("")

	var sids []string
	for _, ruta := range []string{"/mcp/notas", "/mcp/notas", "/mcp/_all"} {
		rec := pedir(t, h, "POST", ruta, "", cuerpoInit)
		sid := rec.Header().Get(SessionHeader)
		if rec.Code != http.StatusOK || sid == "" {
			t.Fatalf("%s initialize: %d %s", ruta, rec.Code, rec.Body)
		}
		sids = append(sids, sid)
	}
	for _, ruta := range []string{"/mcp/notas", "/mcp/_all"} {
		rec := pedir(t, h, "POST", ruta, "", cuerpoInit)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get(SessionHeader) != "" {
			t.Fatalf("%s por encima del tope: %d sid=%q %s", ruta, rec.Code, rec.Header().Get(SessionHeader), rec.Body)
		}
	}
	if rec := pedir(t, h, "DELETE", "/mcp/notas", sids[0], ""); rec.Code >= 300 {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	if rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit); rec.Code != http.StatusOK || rec.Header().Get(SessionHeader) == "" {
		t.Fatalf("tras cerrar una: %d %s", rec.Code, rec.Body)
	}
}

// El router de varios hosts: solo initialize abre conversación, y con tope.
func TestRouterSoloAcunaEnInitializeYConTope(t *testing.T) {
	sock := mockDaemon(t, nil, nil)
	r := NewRouter([]RouterHost{{Name: "uno", GW: New(api.NewClient(sock), 5*time.Minute, false, 0, "")}})
	r.MaxSesiones = 2
	h := r.Handler("")

	if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoCall); rec.Code != http.StatusBadRequest || rec.Header().Get(SessionHeader) != "" {
		t.Fatalf("tools/call sin sesión: %d sid=%q", rec.Code, rec.Header().Get(SessionHeader))
	}
	for i := 0; i < 2; i++ {
		if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoInit); rec.Code != http.StatusOK || rec.Header().Get(SessionHeader) == "" {
			t.Fatalf("initialize %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoInit); rec.Code != http.StatusTooManyRequests || rec.Header().Get(SessionHeader) != "" {
		t.Fatalf("por encima del tope: %d sid=%q", rec.Code, rec.Header().Get(SessionHeader))
	}
}

// Podar los dueños de sesiones acuñadas (en cada tick) mira las rutas sin
// contarlo como uso: si no, ninguna sesión directa caducaba ni su microVM se
// congelaba, y el tenant acababa en 429 al llegar a MaxSesiones.
func TestPodarDuenosNoCuentaComoUso(t *testing.T) {
	_, addrs := invitadosFalsos(t, 2)
	d := &daemonAislado{service: "notas", invitados: addrs, compartido: true}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	if rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit); rec.Code != http.StatusOK || rec.Header().Get(SessionHeader) == "" {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	time.Sleep(60 * time.Millisecond)
	antes := gw.Status("notas").Idle
	if antes < 50*time.Millisecond {
		t.Fatalf("la instancia no estaba ociosa: %v", antes)
	}
	gw.podarDuenos()
	if despues := gw.Status("notas").Idle; despues < antes {
		t.Fatalf("podar los dueños refrescó el último uso: ociosa %v antes, %v después", antes, despues)
	}
	if n := gw.sesionesDirectas("default"); n != 1 {
		t.Fatalf("podar quitó una sesión viva: quedan %d", n)
	}
}
