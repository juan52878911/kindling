package frontal

import (
	"net/http/httptest"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/telemetria"
)

// El traceparent de una petición al gateway llega al daemon: el trace-id es el
// mismo y el span-id es el del gateway (uno nuevo, hijo del entrante).
func TestTraceparentLlegaAlDaemon(t *testing.T) {
	f := nuevoFalso(t)
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}, hosts.ConTransporte(telemetria.Transporte)),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}}, Log: testLog(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(telemetria.Medio(s, telemetria.Config{}))
	t.Cleanup(srv.Close)

	const entrante = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	f.mu.Lock()
	f.trazas = nil
	f.mu.Unlock()

	req := peticion(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Traceparent", entrante)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	esperaCodigo(t, resp, 200)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.trazas) == 0 {
		t.Fatal("ninguna petición al daemon llevó traceparent")
	}
	for _, tp := range f.trazas {
		tr, ok := telemetria.ParsearTraceparent(tp)
		if !ok || tr.TraceID != "0af7651916cd43dd8448eb211c80319c" {
			t.Errorf("traceparent al daemon: %q", tp)
		}
		if tr.SpanID == "b7ad6b7169203331" {
			t.Errorf("el daemon recibió el span del cliente, no el del gateway: %q", tp)
		}
	}
}

// Sin la opción, el cliente de hosts no propaga nada (OTLP apagado).
func TestSinTransporteNoHayTraceparent(t *testing.T) {
	f := nuevoFalso(t)
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}}, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(telemetria.Medio(s, telemetria.Config{}))
	t.Cleanup(srv.Close)
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.trazas) != 0 {
		t.Fatalf("traceparent sin OTLP: %v", f.trazas)
	}
}
