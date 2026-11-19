package frontal

import (
	"net/http"
	"testing"
)

// El 429 de cuota al crear (uno suelto o un lote) lleva Retry-After; el que
// no es de cuota, no.
func TestCuotaSuperadaTraeRetryAfter(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "bob", Token: tokenBob, MaxSandboxes: 1})
	crea(t, srv, tokenBob, CrearPeticion{Image: "base"})

	n := 1
	for _, pet := range []CrearPeticion{{Image: "base"}, {Image: "base", Count: &n}} {
		resp := pide(t, srv, tokenBob, "POST", "/v1/sandboxes", pet)
		esperaCodigo(t, resp, http.StatusTooManyRequests)
		if ra := resp.Header.Get("Retry-After"); ra != retryAfterCuota {
			t.Fatalf("Retry-After = %q, quería %q", ra, retryAfterCuota)
		}
	}
	// Un 404 no pide esperar.
	resp := pide(t, srv, tokenBob, "GET", "/v1/sandboxes/a%2Fno-existe", nil)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After en un no-429: %q", ra)
	}
}
