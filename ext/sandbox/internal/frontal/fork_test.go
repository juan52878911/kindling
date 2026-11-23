package frontal

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestForkCreaCopiasDelInquilino(t *testing.T) {
	f := nuevoFalso(t)
	f.forkHereda = true
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Labels: map[string]string{"env": "dev"}, TTLSeconds: 600})
	idMio := sb.ID[len("a/"):]
	f.mu.Lock()
	f.maquinas[idMio].Labels[LabelPublico] = "3000:9999999999999999"
	f.mu.Unlock()

	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/fork", ForkPeticion{Count: 3, TTLSeconds: 120})
	esperaCodigo(t, resp, 201)
	r := decodifica[ForkRespuesta](t, resp)
	if len(r.Sandboxes) != 3 {
		t.Fatalf("copias: %d", len(r.Sandboxes))
	}
	f.mu.Lock()
	pedido := f.forks[0]
	f.mu.Unlock()
	if pedido.Count != 3 || pedido.TTLSeconds != 120 || pedido.Labels[LabelTenant] != "alice" {
		t.Errorf("lo pedido al daemon: %+v", pedido)
	}
	for _, c := range r.Sandboxes {
		mc := f.maquina(c.ID[len("a/"):])
		if mc == nil || mc.Labels[LabelTenant] != "alice" || mc.Labels[api.LabelKind] != api.KindSandbox {
			t.Errorf("copia sin dueño o marca: %+v", mc)
			continue
		}
		if mc.Labels[LabelPublico] != "" {
			t.Errorf("la copia heredó los puertos públicos del original: %q", mc.Labels[LabelPublico])
		}
		if c.Labels["env"] != "dev" || c.TTLSeconds != 120 {
			t.Errorf("copia %+v", c)
		}
	}
	// Las copias son suyas: las ve Alice y no Bob.
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+r.Sandboxes[0].ID, nil), 200)
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes/"+r.Sandboxes[0].ID, nil), 404)
}

func TestForkCuentaParaLaCuotaYValida(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 3, MaxLote: 4}, Tenant{Nombre: "bob", Token: tokenBob})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	url := "/v1/sandboxes/" + sb.ID + "/fork"
	// 1 vivo + 3 copias = 4 > 3: se rechaza antes de pausar nada.
	resp := pide(t, srv, tokenAlice, "POST", url, ForkPeticion{Count: 3})
	esperaCodigo(t, resp, 429)
	resp.Body.Close()
	f.mu.Lock()
	n := len(f.forks)
	f.mu.Unlock()
	if n != 0 {
		t.Errorf("el daemon recibió %d forks pese a la cuota", n)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", url, ForkPeticion{Count: 2}), 201)
	// Ya hay 3: otro, no.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", url, ForkPeticion{}), 429)
	// Topes de la petición.
	for _, c := range []ForkPeticion{{Count: -1}, {Count: 5}, {Count: 1, TTLSeconds: -1}, {Count: 1, TTLSeconds: api.SandboxMaxTTL + 1}} {
		esperaCodigo(t, pide(t, srv, tokenAlice, "POST", url, c), 400)
	}
	// Ajeno: 404 sin tocar al daemon.
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", url, ForkPeticion{}), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", url, nil), 405)
}

// Dos forks a la vez cuentan los dos para la cuota: el hueco se reserva antes
// de llamar al daemon.
func TestForkReservaCuotaMientrasEspera(t *testing.T) {
	f := nuevoFalso(t)
	f.forkEspera, f.forkEmpezado = make(chan struct{}), make(chan struct{})
	srv, _ := montar(t, map[string]*falso{"a": f}, Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 3})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	url := "/v1/sandboxes/" + sb.ID + "/fork"
	cod := make(chan int, 1)
	go func() {
		r := pide(t, srv, tokenAlice, "POST", url, ForkPeticion{Count: 2})
		r.Body.Close()
		cod <- r.StatusCode
	}()
	select {
	case <-f.forkEmpezado:
	case <-time.After(5 * time.Second):
		t.Fatal("el fork no llegó al daemon")
	}
	r := pide(t, srv, tokenAlice, "POST", url, ForkPeticion{})
	r.Body.Close()
	if r.StatusCode != 429 {
		t.Errorf("un fork más mientras otro está en curso: %d, quería 429", r.StatusCode)
	}
	close(f.forkEspera)
	if c := <-cod; c != 201 {
		t.Errorf("el primer fork: %d", c)
	}
}

func TestForkErrorDelDaemon(t *testing.T) {
	f := nuevoFalso(t)
	f.forkCodigo = 409
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/fork", ForkPeticion{}), 409)
}
