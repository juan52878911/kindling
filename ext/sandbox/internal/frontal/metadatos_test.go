package frontal

import (
	"fmt"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestMetadatosAnadeYQuitaEtiquetas(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Labels: map[string]string{"env": "dev", "equipo": "x"}})
	url := "/v1/sandboxes/" + sb.ID + "/metadata"

	resp := pide(t, srv, tokenAlice, "PATCH", url, MetadatosPeticion{Labels: map[string]string{"env": "prod", "nueva": "1"}, Remove: []string{"equipo"}})
	esperaCodigo(t, resp, 200)
	got := decodifica[Sandbox](t, resp)
	if got.Labels["env"] != "prod" || got.Labels["nueva"] != "1" || got.Labels["equipo"] != "" {
		t.Errorf("etiquetas tras el PATCH: %v", got.Labels)
	}
	// Lo que se ve después en GET y en el daemon.
	resp = pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID, nil)
	esperaCodigo(t, resp, 200)
	ver := decodifica[Sandbox](t, resp)
	if _, hay := ver.Labels["equipo"]; hay || ver.Labels["env"] != "prod" {
		t.Errorf("GET tras el PATCH: %v", ver.Labels)
	}
	if mc := f.maquina(sb.ID[len("a/"):]); mc.Labels[LabelTenant] != "alice" {
		t.Errorf("el PATCH tocó el dueño: %v", mc.Labels)
	}
}

func TestMetadatosRechazaReservadasYMalas(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	url := "/v1/sandboxes/" + sb.ID + "/metadata"
	casos := []MetadatosPeticion{
		{Labels: map[string]string{"tenant": "bob"}},
		{Labels: map[string]string{"kind": "x"}},
		{Labels: map[string]string{"template": "x"}},
		{Labels: map[string]string{LabelPublico: "3000:1"}},
		{Labels: map[string]string{"kling.owner": "bob"}},
		{Remove: []string{"tenant"}},
		{Remove: []string{LabelRevocados}},
		{Labels: map[string]string{"A B": "x"}},
		{Labels: map[string]string{"k": ""}},
		{Labels: map[string]string{"k": "v"}, Remove: []string{"k"}},
		{},
	}
	for i, c := range casos {
		esperaCodigo(t, pide(t, srv, tokenAlice, "PATCH", url, c), 400)
		_ = i
	}
	// Demasiadas.
	muchas := map[string]string{}
	for i := 0; i <= maxEtiquetas; i++ {
		muchas[fmt.Sprintf("k%d", i)] = "v"
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "PATCH", url, MetadatosPeticion{Labels: muchas}), 400)
	// Ajeno y desconocido: 404; método: 405.
	esperaCodigo(t, pide(t, srv, tokenBob, "PATCH", url, MetadatosPeticion{Labels: map[string]string{"k": "v"}}), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "PATCH", "/v1/sandboxes/a/nada/metadata", MetadatosPeticion{Labels: map[string]string{"k": "v"}}), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", url, nil), 405)
	esperaCodigo(t, pide(t, srv, "", "PATCH", url, MetadatosPeticion{Labels: map[string]string{"k": "v"}}), 401)
}

func TestMetricasFiltraPorSandbox(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	otro := crea(t, srv, tokenBob, CrearPeticion{Image: "base"})
	idMio, idOtro := sb.ID[len("a/"):], otro.ID[len("a/"):]
	f.mu.Lock()
	f.procMaquinas = []api.ProcStat{
		{ID: idMio, State: api.StateRunning, PSSMiB: 123, CPUSeconds: 4.5},
		{ID: idOtro, State: api.StateRunning, PSSMiB: 999, CPUSeconds: 77},
	}
	f.mu.Unlock()
	resp := pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/metrics", nil)
	esperaCodigo(t, resp, 200)
	m := decodifica[Metricas](t, resp)
	if m.MemoryMiB != 123 || m.CPUSeconds != 4.5 || m.MemoryKind != "pss" || m.ID != sb.ID {
		t.Errorf("métricas: %+v", m)
	}
	// Sin dato (parado): sin memoria, no un 0 inventado ni lo de otro.
	f.mu.Lock()
	f.procMaquinas = f.procMaquinas[1:]
	f.mu.Unlock()
	m = decodifica[Metricas](t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/metrics", nil))
	if m.MemoryMiB != 0 || m.CPUSeconds != 0 {
		t.Errorf("sin lectura del daemon: %+v", m)
	}
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes/"+sb.ID+"/metrics", nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/metrics", nil), 405)
}
