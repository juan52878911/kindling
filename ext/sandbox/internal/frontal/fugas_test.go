package frontal

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

// Lo que un tenant cualquiera NO debe ver: los nombres de los demás tenants,
// los endpoints de los daemons ni los errores crudos de la red interna.

const tokenAdmin = "tok-admin-5555555555"

func tenantsConAdmin() []Tenant {
	return []Tenant{
		{Nombre: "alice", Token: tokenAlice},
		{Nombre: "bob", Token: tokenBob},
		{Nombre: "ops", Token: tokenAdmin, Admin: true},
	}
}

func leerTexto(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMetricasDeUnTenantSoloEnsenanLoSuyo(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f}, tenantsConAdmin()...)
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	crea(t, srv, tokenBob, CrearPeticion{Image: "base"})

	resp := pide(t, srv, tokenBob, "GET", "/v1/metrics", nil)
	esperaCodigo(t, resp, 200)
	texto := leerTexto(t, resp)
	if strings.Contains(texto, `"alice"`) || strings.Contains(texto, `"ops"`) {
		t.Fatalf("bob sees other tenants:\n%s", texto)
	}
	for _, quiero := range []string{
		`kling_sandbox_machines{tenant="bob",state="running"} 1`,
		`kling_sandbox_shell_sessions{tenant="bob"} 0`,
	} {
		if !strings.Contains(texto, quiero) {
			t.Errorf("bob's metrics miss %q:\n%s", quiero, texto)
		}
	}

	resp = pide(t, srv, tokenAdmin, "GET", "/v1/metrics", nil)
	esperaCodigo(t, resp, 200)
	texto = leerTexto(t, resp)
	for _, quiero := range []string{
		`kling_sandbox_machines{tenant="alice",state="running"} 1`,
		`kling_sandbox_machines{tenant="bob",state="running"} 1`,
		`kling_sandbox_shell_sessions{tenant="alice"} 0`,
	} {
		if !strings.Contains(texto, quiero) {
			t.Errorf("the admin's metrics miss %q:\n%s", quiero, texto)
		}
	}
}

func TestHostsSoloParaAdmin(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f}, tenantsConAdmin()...)

	resp := pide(t, srv, tokenAlice, "GET", "/v1/hosts", nil)
	esperaCodigo(t, resp, http.StatusForbidden)
	if texto := leerTexto(t, resp); strings.Contains(texto, f.endpoint) {
		t.Fatalf("a tenant got the daemon's endpoint: %s", texto)
	}

	resp = pide(t, srv, tokenAdmin, "GET", "/v1/hosts", nil)
	esperaCodigo(t, resp, 200)
	ests := decodifica[[]hosts.Estado](t, resp)
	if len(ests) != 1 || ests[0].Endpoint != f.endpoint {
		t.Fatalf("hosts for the admin: %+v", ests)
	}
}

// Un host caído da un error de red con la ruta de su socket (o su dirección):
// el tenant recibe el código y un mensaje genérico, no esa ruta.
func TestErroresDeRedNoEnsenanElEndpoint(t *testing.T) {
	const muerto = "/nonexistent/kling.sock"
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f, "muerto": {endpoint: muerto}},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 3},
		Tenant{Nombre: "bob", Token: tokenBob})

	// Cuota sin poder contar: 503.
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"})
	esperaCodigo(t, resp, http.StatusServiceUnavailable)
	if texto := leerTexto(t, resp); strings.Contains(texto, "nonexistent") || !strings.Contains(texto, "quota") {
		t.Fatalf("quota error: %s", texto)
	}
	// Un sandbox en el host caído: 502.
	resp = pide(t, srv, tokenBob, "GET", "/v1/sandboxes/muerto/abcd1234", nil)
	esperaCodigo(t, resp, http.StatusBadGateway)
	if texto := leerTexto(t, resp); strings.Contains(texto, "nonexistent") || strings.Contains(texto, "dial") {
		t.Fatalf("lookup error: %s", texto)
	}
	// La lista parcial avisa del host, no de su socket.
	resp = pide(t, srv, tokenBob, "GET", "/v1/sandboxes", nil)
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	if p := resp.Header.Get("X-Kindling-Partial"); p == "" || strings.Contains(p, "nonexistent") || !strings.Contains(p, "muerto") {
		t.Fatalf("partial warning: %q", p)
	}
	// Los errores del cliente (4xx) siguen diciendo qué está mal.
	resp = pide(t, srv, tokenBob, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", OnTTL: "explode"})
	esperaCodigo(t, resp, http.StatusBadRequest)
	if texto := leerTexto(t, resp); !strings.Contains(texto, "explode") {
		t.Fatalf("validation error lost its detail: %s", texto)
	}
}
