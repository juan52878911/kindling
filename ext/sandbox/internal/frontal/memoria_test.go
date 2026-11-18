package frontal

import (
	"sync"
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

	// Sin decir nada no hay tope total; con el del frontal, cada tenant sin el
	// suyo hereda ese, y el suyo manda sobre él.
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "x", Token: tokenAlice}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.tenants[0].MaxMemTotalMiB; got != 0 {
		t.Fatalf("total limit without configuration %d, want 0 (off)", got)
	}
	s, err = Nuevo(Opciones{Hosts: hosts.Nuevo(nil), MaxMemTotalMiB: 8192, Tenants: []Tenant{
		{Nombre: "x", Token: tokenAlice}, {Nombre: "y", Token: tokenBob, MaxMemTotalMiB: 2048}}})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := s.tenants[0].MaxMemTotalMiB, s.tenants[1].MaxMemTotalMiB; a != 8192 || b != 2048 {
		t.Fatalf("total limits %d and %d, want 8192 (frontal) and 2048 (own)", a, b)
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

// Con imagen y sin mem_mib el daemon da 256 MiB: la cuota cuenta eso y no el
// 0 de la petición. Antes, con un tope total de 512, veinte a la vez cabían.
func TestTopeDeMemoriaCuentaElCeroDeLaImagen(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemTotalMiB: 512})

	var wg sync.WaitGroup
	codigos := make([]int, 20)
	for i := range codigos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"})
			codigos[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	creados := 0
	for _, c := range codigos {
		if c == 201 {
			creados++
		}
	}
	if creados != 2 {
		t.Fatalf("%d sandboxes of 256 MiB created under a 512 MiB limit (codes %v)", creados, codigos)
	}
}

// Los grafos suman al tope total: reservan lo de sus nodos y, una vez
// creados, sus máquinas cuentan. Antes reservaban 0 y no sumaban nada.
func TestTopeDeMemoriaCuentaLosGrafos(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(0))
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemTotalMiB: 768})

	// Dos nodos sin mem_mib: 512 MiB.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 201)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 429)
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base", MemMiB: 256})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 1}), 429)
}

// Un host vivo cuya lista falla ahora no se da por caído: no se sabe lo que
// el tenant tiene ahí y el tope total falla cerrado (503). Un host que no
// contesta ni a su sonda sí se salta, como antes.
func TestTopeDeMemoriaHostVivoSinListaFallaCerrado(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemTotalMiB: 4096})

	b.mu.Lock()
	b.sandboxesFalla = true
	b.mu.Unlock()
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 503)
	if n := a.creadas + b.creadas; n != 0 {
		t.Fatalf("%d sandboxes created without knowing what host b holds", n)
	}

	b.srv.Close()
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
}
