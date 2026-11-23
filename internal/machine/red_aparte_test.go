package machine

import (
	"errors"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
)

// A-1 paso 2: si la comprobación de las barreras del host (que va en paralelo
// con el montaje) falla, la espera lo dice y desmonta la red. runFromUna la
// ejecuta justo antes de Resume: no hay camino a Resume sin ella.
func TestMontarRedAparteFalloAlEsperarDesmonta(t *testing.T) {
	m := newTestManager(t)
	oldA, oldD := montarRedHostAparte, desmontarRedHost
	oldM := montarRedHost
	t.Cleanup(func() { montarRedHostAparte, desmontarRedHost, montarRedHost = oldA, oldD, oldM })
	montarRedHost = nil
	montarRedHostAparte = func(*knet.Net, knet.Egress, []string, int) (func() error, error) {
		return func() error { return errors.New("barrera caída") }, nil
	}
	desmontadas := 0
	desmontarRedHost = func(*knet.Net) { desmontadas++ }
	n := knet.Plan(40, "ab0000000000a001")
	esperar, err := m.montarRedAparte(n, "ab0000000000a001", knet.EgressNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if desmontadas != 0 {
		t.Fatal("no debe desmontar antes de esperar")
	}
	if err := esperar(); err == nil {
		t.Fatal("la espera debía devolver el fallo de la comprobación")
	}
	if err := esperar(); err == nil || desmontadas != 1 {
		t.Fatalf("segunda espera: err=%v desmontadas=%d (quería el mismo fallo y un solo desmontaje)", err, desmontadas)
	}
	if _, ok := m.redMontada.Load("ab0000000000a001"); ok {
		t.Fatal("la red fallida no debe constar como montada")
	}
}

// montarRed (sincrónico) conserva la semántica de antes: la comprobación va
// dentro del montaje y, si falla, no queda nada montado ni marcado.
func TestMontarRedEsperaLaComprobacion(t *testing.T) {
	m := newTestManager(t)
	oldA, oldD, oldM := montarRedHostAparte, desmontarRedHost, montarRedHost
	t.Cleanup(func() { montarRedHostAparte, desmontarRedHost, montarRedHost = oldA, oldD, oldM })
	montarRedHostAparte = func(*knet.Net, knet.Egress, []string, int) (func() error, error) {
		t.Fatal("montarRed no debe usar el montaje aparte")
		return nil, nil
	}
	montarRedHost = func(*knet.Net, knet.Egress, []string, int) error { return errors.New("barrera caída") }
	desmontadas := 0
	desmontarRedHost = func(*knet.Net) { desmontadas++ }
	if err := m.montarRed(knet.Plan(41, "ab0000000000a002"), "ab0000000000a002", knet.EgressNone, nil); err == nil {
		t.Fatal("montarRed debe fallar si la comprobación falla")
	}
	if desmontadas != 1 {
		t.Fatalf("desmontadas=%d, quería 1", desmontadas)
	}
	if _, ok := m.redMontada.Load("ab0000000000a002"); ok {
		t.Fatal("la red fallida no debe constar como montada")
	}
}
