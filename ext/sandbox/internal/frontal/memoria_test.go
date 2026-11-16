package frontal

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// Con plantilla y sin mem_mib, el sandbox tiene la memoria del snapshot: el
// tope por sandbox mira esa, no el 0 de la petición.
func TestTopeDeMemoriaMiraElSnapshotDeLaPlantilla(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-grande"}
	f.snapMem = map[string]int{"sbx-grande": 2048}
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemMiB: 1024})

	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "grande"}), 400)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "grande", MemMiB: 512}), 400)
	if f.creadas != 0 {
		t.Fatalf("%d machines created over the limit", f.creadas)
	}
}

// La suma de lo vivo del tenant tiene tope, aunque no lo tenga el número de
// sandboxes (el tenant "default" no lo trae).
func TestTopeDeMemoriaAgregadoPorTenant(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-py"}
	f.snapMem = map[string]int{"sbx-py": 600}
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemTotalMiB: 1024},
		Tenant{Nombre: "bob", Token: tokenBob, MaxMemTotalMiB: 1024})

	crea(t, srv, tokenAlice, CrearPeticion{Image: "base", MemMiB: 512})
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base", MemMiB: 512})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 1}), 429)
	// La plantilla cuenta por lo que ocupa su snapshot.
	crea(t, srv, tokenBob, CrearPeticion{Image: "base", MemMiB: 512})
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes", CrearPeticion{Template: "py"}), 429)

	// Sin decir nada, a cada tenant le toca el del frontal.
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "x", Token: tokenAlice}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.tenants[0].MaxMemTotalMiB != MaxMemTotalMiBPorDefecto {
		t.Fatalf("default total limit %d, want %d", s.tenants[0].MaxMemTotalMiB, MaxMemTotalMiBPorDefecto)
	}
}

// Un token corto o repetido entre tenants no arranca.
func TestTokensCortosORepetidosNoArrancan(t *testing.T) {
	for _, ts := range [][]Tenant{
		{{Nombre: "a", Token: "corto"}},
		{{Nombre: "a", Token: tokenAlice}, {Nombre: "b", Token: tokenAlice}},
	} {
		if _, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: ts}); err == nil {
			t.Errorf("tenants %+v accepted", ts)
		}
	}
}

// Una precalentada de un snapshot anterior (un rebuild lo reemplazó con el
// mismo nombre) no se entrega: se restaura una del snapshot de ahora.
func TestPrecalentadaDeOtroSnapshotNoSeEntrega(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	ahora := time.Now().UTC()
	f.snapHecho = map[string]time.Time{"sbx-node": ahora}
	vieja := plantilla.VersionDe(&api.Snapshot{CreatedAt: ahora.Add(-time.Hour)})
	pre := f.sembrar(&api.Machine{From: "sbx-node",
		Labels: map[string]string{LabelTemplate: "node", plantilla.LabelVersion: vieja}})
	srv, _ := montar(t, map[string]*falso{"a": f})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if sb.ID == "a/"+pre.ID || f.creadas != 1 {
		t.Fatalf("got the stale prewarmed %s (created %d)", sb.ID, f.creadas)
	}

	// La de la versión de ahora sí.
	buena := f.sembrar(&api.Machine{From: "sbx-node",
		Labels: map[string]string{LabelTemplate: "node", plantilla.LabelVersion: plantilla.VersionDe(&api.Snapshot{CreatedAt: ahora})}})
	if sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"}); sb.ID != "a/"+buena.ID {
		t.Fatalf("expected the current prewarmed %s, got %s", buena.ID, sb.ID)
	}
}
