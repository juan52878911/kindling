package frontal

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Dos frontales sobre el mismo daemon reclaman la misma precalentada: el
// primero se queda parado en su primera escritura de etiquetas mientras el
// segundo reclama entero; luego sigue. Solo uno se la queda, y con sus
// etiquetas y su TTL, sin nada del otro.
func TestDosFrontalesSoloUnoSeQuedaLaPrecalentada(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	pre := f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	f.etiquetasEspera = make(chan struct{})
	f.etiquetasEmpezado = make(chan struct{})
	srvA, _ := montar(t, map[string]*falso{"a": f})
	srvB, _ := montar(t, map[string]*falso{"a": f})

	petA := CrearPeticion{Template: "node", TTLSeconds: 111, Labels: map[string]string{"de": "alice", "solo-a": "1"}}
	petB := CrearPeticion{Template: "node", TTLSeconds: 222, Labels: map[string]string{"de": "bob"}}
	resA := make(chan Sandbox, 1)
	go func() {
		var sb Sandbox
		resp, err := http.DefaultClient.Do(peticion(t, srvA, tokenAlice, "POST", "/v1/sandboxes", petA))
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&sb)
			resp.Body.Close()
		}
		resA <- sb
	}()
	<-f.etiquetasEmpezado
	sbB := crea(t, srvB, tokenBob, petB)
	close(f.etiquetasEspera)
	sbA := <-resA

	id := "a/" + pre.ID
	if (sbA.ID == id) == (sbB.ID == id) {
		t.Fatalf("the prewarmed machine went to A=%v and B=%v", sbA.ID == id, sbB.ID == id)
	}
	if sbA.ID == "" || sbB.ID == "" {
		t.Fatalf("a request failed: A=%+v B=%+v", sbA, sbB)
	}
	tenant, pet := "alice", petA
	if sbB.ID == id {
		tenant, pet = "bob", petB
	}
	mc := f.maquina(pre.ID)
	if mc.Labels[LabelTenant] != tenant || mc.TTLSeconds != pet.TTLSeconds {
		t.Fatalf("claimed by %s but has tenant %q and ttl %d", tenant, mc.Labels[LabelTenant], mc.TTLSeconds)
	}
	for k := range mc.Labels {
		switch k {
		case api.LabelKind, LabelTenant, LabelTemplate, LabelReclamo:
			continue
		}
		if mc.Labels[k] != pet.Labels[k] {
			t.Errorf("label %s=%q does not belong to %s", k, mc.Labels[k], tenant)
		}
	}
}

// Si la segunda fase tarda más que la pausa, otro frontal pudo pasar también:
// la precalentada no se entrega, se destruye, y el sandbox sale nuevo.
func TestReclamoConSegundaFaseLentaNoSeEntrega(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	pre := f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	f.etiquetasLentas = 60 * time.Millisecond
	srv, s := montar(t, map[string]*falso{"a": f})
	s.esperaReclamo = 20 * time.Millisecond

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if sb.ID == "a/"+pre.ID || f.creadas != 1 {
		t.Fatalf("handed out the slow claim: %s (created %d)", sb.ID, f.creadas)
	}
	if f.existe(pre.ID) {
		t.Fatal("the half-claimed machine is still there")
	}
}
