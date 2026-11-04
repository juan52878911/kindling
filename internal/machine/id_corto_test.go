package machine

import (
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Dos máquinas cuyo id empieza igual comparten netns, veth y cgroup: la segunda
// tiraría la red de la primera al hacer Setup. Al acuñar, un prefijo ocupado se
// sortea otra vez.
func TestIDMaquinaNoRepitePrefijoDeUnaViva(t *testing.T) {
	olvidarAcunados(t, "abcdef00", "01234567")
	m := newTestManager(t)
	viva := "abcdef0011111111"
	m.mu.Lock()
	m.byID[viva] = &api.Machine{ID: viva}
	m.mu.Unlock()

	sorteos := []string{"abcdef0022222222", "abcdef0033333333", "0123456744444444"}
	i := 0
	gen := func() string { s := sorteos[i]; i++; return s }

	id := m.idLibre(gen)
	if id != "0123456744444444" {
		t.Fatalf("id = %s, quería el primero con prefijo libre", id)
	}
	if i != 3 {
		t.Fatalf("sorteos = %d, quería 3", i)
	}
}

// Dos arranques a la vez no se ven en byID hasta publicar: el segundo no puede
// recibir el prefijo que se acaba de dar al primero.
func TestIDMaquinaNoRepitePrefijoRecienAcunado(t *testing.T) {
	olvidarAcunados(t, "feedc0de", "feedc0df")
	m := newTestManager(t)
	sorteos := []string{"feedc0de00000001", "feedc0de00000002", "feedc0df00000003"}
	i := 0
	gen := func() string { s := sorteos[i]; i++; return s }

	a := m.idLibre(gen)
	b := m.idLibre(gen)
	if knet.Corto(a) == knet.Corto(b) {
		t.Fatalf("dos ids con el mismo prefijo: %s y %s", a, b)
	}
	if b != "feedc0df00000003" {
		t.Fatalf("b = %s", b)
	}
}

// olvidarAcunados quita del registro global los prefijos que usa un test, antes
// y después, para que -count=N no herede los de la pasada anterior.
func olvidarAcunados(t *testing.T, prefijos ...string) {
	borrar := func() {
		acuñados.Lock()
		for _, p := range prefijos {
			delete(acuñados.m, p)
		}
		acuñados.Unlock()
	}
	borrar()
	t.Cleanup(borrar)
}
