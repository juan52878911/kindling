package frontal

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
)

// conPool anota el snapshot sbx-<tpl> como lo hace plantilla.Construir: con
// una receta que pide pool instancias precalentadas.
func conPool(f *falso, tpl string, pool int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: tpl, Imagen: "base", Pool: pool},
	})
	if f.snapAnot == nil {
		f.snapAnot = map[string]map[string]json.RawMessage{}
	}
	f.snapAnot["sbx-"+tpl] = map[string]json.RawMessage{plantilla.AnotacionReceta: receta}
}

// Una creación en frío de una plantilla sin pool no lista los sandboxes de los
// hosts para buscar una precalentada: solo pide el snapshot y crea.
func TestCrearFrioSinPoolNoListaSandboxes(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	srv, _ := montar(t, map[string]*falso{"a": f})
	// La cuota ya lista una vez; lo que no debe haber es el listado del reclamo.
	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if f.snapUno != 1 || f.snapLista != 0 || f.creadas != 1 {
		t.Fatalf("cold creation: %d single snapshot, %d snapshot lists, %d created", f.snapUno, f.snapLista, f.creadas)
	}
	base := f.sandboxesLlamadas
	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	sinPool := f.sandboxesLlamadas - base

	conPool(f, "node", 2)
	base = f.sandboxesLlamadas
	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	con := f.sandboxesLlamadas - base
	if con != sinPool+1 {
		t.Fatalf("listings: %d without pool, %d with pool; want exactly one more", sinPool, con)
	}
}

// Averiguar la plantilla no baja la lista entera de snapshots; ante un daemon
// sin la ruta, o sin el snapshot, cae a la lista.
func TestSnapshotsDePideUno(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node", "otro"}
	_, s := montar(t, map[string]*falso{"a": f})
	got := s.snapshotsDe(t.Context(), "sbx-node")
	if got["a"] == nil || got["a"].Name != "sbx-node" || f.snapUno != 1 || f.snapLista != 0 {
		t.Fatalf("got %+v, %d single, %d list", got, f.snapUno, f.snapLista)
	}

	f.snapUnoViejo = true
	got = s.snapshotsDe(t.Context(), "sbx-node")
	if got["a"] == nil || f.snapLista != 1 {
		t.Fatalf("old daemon: got %+v, %d list", got, f.snapLista)
	}
	if got = s.snapshotsDe(t.Context(), "sbx-nada"); len(got) != 0 {
		t.Fatalf("missing snapshot found: %+v", got)
	}
}

// /metrics expone todos los tramos y la creación apunta los suyos.
func TestMetricasTramosDeCreacion(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	// Los tramos son series del operador: solo las ve un admin.
	srv, _ := montar(t, map[string]*falso{"a": f}, tenantsConAdmin()...)
	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	resp := pide(t, srv, tokenAdmin, "GET", "/v1/metrics", nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(b)
	for _, tr := range tramosCrear {
		if !strings.Contains(out, `kling_sandbox_create_stage_seconds_count{stage="`+tr+`"}`) {
			t.Errorf("missing stage %q", tr)
		}
	}
	for _, tr := range []string{tramoSnapshot, tramoPrepasos, tramoDaemon, tramoTotal} {
		if strings.Contains(out, `_count{stage="`+tr+`"} 0`) {
			t.Errorf("stage %q not observed", tr)
		}
	}
}
