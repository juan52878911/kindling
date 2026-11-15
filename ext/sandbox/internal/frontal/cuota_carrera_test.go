package frontal

import (
	"sync/atomic"
	"testing"
	"time"
)

// La cuota no se supera aunque una creación acabe mientras otra cuenta: X
// lista (nada), Y crea y termina entera, y X comprueba. Sin el cerrojo del
// tenant, la de Y no salía ni en la lista de X (leída antes) ni en enVuelo
// (ya desapuntada), y nacían dos con MaxSandboxes 1.
func TestCuotaNoSeSuperaConUnaCreacionQueAcabaMientrasOtraCuenta(t *testing.T) {
	f := nuevoFalso(t)
	srv, s := montar(t, map[string]*falso{"a": f}, Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 1})

	contando := make(chan struct{})
	seguir := make(chan struct{})
	// Un CAS y no un sync.Once: Once haría esperar a Y a que X saliera.
	var primera atomic.Bool
	s.trasContar = func() {
		if primera.CompareAndSwap(false, true) {
			close(contando)
			<-seguir
		}
	}

	codigos := make(chan int, 2)
	go func() { // X: se para entre contar y comprobar
		resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"})
		resp.Body.Close()
		codigos <- resp.StatusCode
	}()
	<-contando
	go func() { // Y: entera mientras X está parada (o esperando su turno)
		resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"})
		resp.Body.Close()
		codigos <- resp.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	close(seguir)

	creados := 0
	for i := 0; i < 2; i++ {
		c := <-codigos
		if c == 201 {
			creados++
		}
	}
	f.mu.Lock()
	n := len(f.maquinas)
	f.mu.Unlock()
	if creados != 1 || n != 1 {
		t.Fatalf("%d creaciones aceptadas y %d máquinas con MaxSandboxes 1", creados, n)
	}
}
