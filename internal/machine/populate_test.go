package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Poblar un volumen usa la ruta de streaming y junta la salida de los dos
// flujos en orden de llegada.
func TestPoblarUsaElStreaming(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/stream", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		cero := 0
		_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: []byte("added 1 package\n")})
		_ = enc.Encode(api.ExecEvent{Stream: "stderr", Data: []byte("npm WARN deprecated\n")})
		_ = enc.Encode(api.ExecEvent{Exit: &cero})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := ejecutarEnInvitado(context.Background(), srv.URL, []string{"npm", "install"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || got.Output != "added 1 package\nnpm WARN deprecated\n" {
		t.Fatalf("%+v", got)
	}
}

// Con un agente anterior a v0.7 (sin /exec/stream) se cae a /exec, que es lo
// que hacía siempre: poblar no puede dejar de funcionar con imágenes viejas.
func TestPoblarCaeALaRutaAntiguaConAgentesViejos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 3, "output": "boom"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := ejecutarEnInvitado(context.Background(), srv.URL, []string{"false"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 3 || got.Output != "boom" {
		t.Fatalf("%+v", got)
	}
}
