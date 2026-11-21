package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestReclamoAPI(t *testing.T) {
	s, h := testServer(t)

	var info api.Info
	if err := json.Unmarshal(call(t, h, "GET", "/info", "").Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityClaim) {
		t.Fatalf("sin la capacidad %s: %v", api.CapabilityClaim, info.Capabilities)
	}

	// Sin candidata: 404 con su propio código.
	rr := call(t, h, "POST", "/sandboxes/claim", `{"match":{"pool":"ok"}}`)
	var e api.Error
	_ = json.Unmarshal(rr.Body.Bytes(), &e)
	if rr.Code != 404 || e.Code != api.CodeNoClaimable {
		t.Fatalf("sin candidata = %d %s", rr.Code, rr.Body)
	}
	// Match vacío, JSON roto, etiqueta de grafo: 400.
	for _, body := range []string{`{}`, `{`, `{"match":{"pool":"ok"},"labels":{"kling.graph":"x"}}`} {
		if rr := call(t, h, "POST", "/sandboxes/claim", body); rr.Code != 400 {
			t.Fatalf("%s = %d %s", body, rr.Code, rr.Body)
		}
	}

	for _, id := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		s.mgr.SembrarParaPruebas(&api.Machine{ID: id, Name: id, State: api.StateRunning, AllowExec: true,
			Labels: map[string]string{"pool": "ok"}})
	}
	rr = call(t, h, "POST", "/sandboxes/claim", `{"match":{"pool":"ok"},"name":"uno","labels":{"kling.sandbox.claim":"n1"},"ttl_seconds":30}`)
	var mc api.Machine
	if err := json.Unmarshal(rr.Body.Bytes(), &mc); err != nil || rr.Code != 200 || mc.Name != "uno" ||
		mc.Labels[api.LabelClaim] != "n1" || mc.TTLSeconds != 30 {
		t.Fatalf("reclamo = %d %s", rr.Code, rr.Body)
	}
	// Nombre ocupado: 409 y la otra máquina sigue libre.
	rr = call(t, h, "POST", "/sandboxes/claim", `{"match":{"pool":"ok"},"name":"uno"}`)
	if rr.Code != 409 {
		t.Fatalf("nombre ocupado = %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/sandboxes/claim", `{"match":{"pool":"ok"},"labels":{"kling.sandbox.claim":"n2"}}`); rr.Code != 200 {
		t.Fatalf("segunda = %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/sandboxes/claim", `{"match":{"pool":"ok"},"labels":{"kling.sandbox.claim":"x"}}`); rr.Code != 404 {
		t.Fatalf("agotado = %d %s", rr.Code, rr.Body)
	}
}

// Un inquilino no reclama: las máquinas del admin no llevan kling.owner y el
// reclamo se las daría (con exec y su TTL). Solo el admin (el gateway) llega.
func TestReclamoSoloAdmin(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	if rr := como(t, h, uidA, "POST", "/sandboxes/claim", `{"match":{"app":"prod-db"}}`); rr.Code != 403 {
		t.Fatalf("un inquilino reclama: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "POST", "/sandboxes/claim", `{"match":{"app":"prod-db"}}`); rr.Code != 404 {
		t.Fatalf("admin sin nada que reclamar: %d %s", rr.Code, rr.Body)
	}
}
