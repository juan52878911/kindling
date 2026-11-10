package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestAgenteViejo(t *testing.T) {
	casos := []struct {
		ag    *api.GuestAgent
		kling string
		viejo string // "" = no; si no, un trozo del motivo
	}{
		{nil, "0.21.0", ""},
		{&api.GuestAgent{}, "0.21.0", "announces nothing"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.21.0"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.3"}, "v0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.19.0"}, "0.21.0", "more than one minor"},
		{&api.GuestAgent{Version: "v0.17.0"}, "0.18.0", "older than v0.18: outside compatibility, degraded"},
		// 1.0 es la MINOR que sigue a 0.20: una por detrás, dentro.
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.0"}, "1.0.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.3"}, "v1.0.1", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.19.0"}, "1.0.0", "more than one minor"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.0"}, "1.1.0", "more than one minor"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "1.0.0"}, "1.1.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "1.0.0"}, "1.2.0", "more than one minor"},
		// Una MAJOR cuya última MINOR no consta: se juzga por la MAJOR.
		{&api.GuestAgent{Agent: "kling-guest", Version: "1.4.0"}, "2.0.0", "older MAJOR"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "1.1.0"}, "1.0.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "dev"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "dev+1a2b3c4d"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.19.0"}, "dev", ""},
		{&api.GuestAgent{Agent: "kling-bridge", Version: "0.1.0"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.21.0-rc1"}, "0.22.0", ""},
	}
	for _, c := range casos {
		got := agenteViejo(c.ag, c.kling)
		if (c.viejo == "") != (got == "") || !strings.Contains(got, c.viejo) {
			t.Errorf("agenteViejo(%+v, %s) = %q; quería %q", c.ag, c.kling, got, c.viejo)
		}
	}
}

func TestCheckAgentesViejos(t *testing.T) {
	if _, ok := checkAgentesViejos([]api.Image{{Name: "nueva", Agent: &api.GuestAgent{Version: "0.21.0"}}}, "0.21.0"); ok {
		t.Fatal("avisó de una imagen al día")
	}
	c, ok := checkAgentesViejos([]api.Image{
		{Name: "nueva", Agent: &api.GuestAgent{Version: "0.21.0"}},
		{Name: "vieja", Agent: &api.GuestAgent{Agent: "kling-guest", Version: "0.17.2"}},
	}, "0.21.0")
	if !ok || c.State != doctorWarn || !strings.Contains(c.Detail, "vieja") || !strings.Contains(c.Fix, "image import -replace") {
		t.Fatalf("check = %+v", c)
	}
	// Uno anterior a v0.18 está fuera de la compatibilidad: decir además
	// que "funciona" se contradice.
	if strings.Contains(c.Detail, "it works") || !strings.Contains(c.Detail, "outside compatibility") {
		t.Fatalf("detalle contradictorio: %q", c.Detail)
	}
	if !strings.Contains(c.Fix, "kling image refresh") {
		t.Fatalf("el arreglo no ofrece refrescar el agente: %q", c.Fix)
	}
}

// kling image refresh pone el kling-guest del lib del daemon en cada imagen
// (solo reemplazando), salta la de un agente de otra arquitectura sin
// tocarla, y dice cuándo una capa lo toma de su base.
func TestRefreshAgents(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.0.0","arch":"arm64"}`))
	})
	mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"vieja"},{"name":"emulada"},{"name":"capa","base":"vieja"}]`))
	})
	mux.HandleFunc("GET /images/{name}/recipe", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "emulada":
			_, _ = w.Write([]byte(`{"name":"emulada","built":{"arch":"amd64","emulated":true,"agent_arch":"amd64"}}`))
		default:
			_, _ = w.Write([]byte(`{"name":"` + r.PathValue("name") + `","built":{"arch":"arm64"}}`))
		}
	})
	var puestas []string
	mux.HandleFunc("PUT /images/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		var req api.PutImageFileRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Path != "/usr/local/bin/kling-guest" || req.FromHost != "kling-guest" || req.Create {
			t.Errorf("petición inesperada: %+v", req)
		}
		n := r.PathValue("name")
		puestas = append(puestas, n)
		if n == "capa" {
			_, _ = w.Write([]byte(`{"image":"capa","path":"/usr/local/bin/kling-guest","skipped":true,"error":"x"}`))
			return
		}
		_, _ = w.Write([]byte(`{"image":"` + n + `","path":"/usr/local/bin/kling-guest","updated":true}`))
	})
	res, err := refreshAgents(t.Context(), fakeDaemon(t, mux), nil)
	if err != nil {
		t.Fatal(err)
	}
	por := map[string]api.ImageFileResult{}
	for _, r := range res {
		por[r.Image] = r
	}
	if !por["vieja"].Updated {
		t.Errorf("vieja: %+v", por["vieja"])
	}
	if e := por["emulada"]; !e.Skipped || !strings.Contains(e.Error, "amd64") {
		t.Errorf("emulada: %+v", e)
	}
	if c := por["capa"]; !c.Skipped || !strings.Contains(c.Error, "base") {
		t.Errorf("capa: %+v", c)
	}
	if slices.Contains(puestas, "emulada") {
		t.Error("le puso a una imagen amd64 el agente arm64 del daemon")
	}
}
