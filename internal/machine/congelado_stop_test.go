package machine

import (
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// M1: una warm tiene PID 0 aunque su último Freeze dejara vivo el VMM
// (vmmDeCongelado). Stop y Remove lo daban por muerto, y el Start siguiente
// lanzaba un segundo VMM sobre el mismo overlay.

func congeladaConVMMVivo(t *testing.T, m *Manager, id string) (int, <-chan struct{}) {
	t.Helper()
	m.bus = events.New()
	congeladaParaThaw(t, m, id, 64)
	cmd, fin := vmmVivoDe(t, m, id)
	m.apuntarVMMDeCongelado(id, cmd.Process.Pid)
	return cmd.Process.Pid, fin
}

func TestStopDeUnaWarmMataElVMMDeSuCongelado(t *testing.T) {
	m := newTestManager(t)
	id := "a1a1000000000001"
	_, fin := congeladaConVMMVivo(t, m, id)

	out, err := m.Stop(id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != api.StateStopped {
		t.Fatalf("Stop = %s", out.State)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop dejó vivo el VMM del congelado: el Start siguiente lanzaría otro")
	}
	if _, ok := m.vmmDeCongelado.Load(id); ok {
		t.Fatal("la marca sobrevivió a su VMM")
	}
}

// Si no muere, Stop se niega como con cualquier VMM vivo (M4): parada con
// PID 0 dejaba arrancar otro encima.
func TestStopDeUnaWarmSeNiegaSiSuVMMNoMuere(t *testing.T) {
	m := newTestManager(t)
	id := "a1a1000000000002"
	congeladaConVMMVivo(t, m, id)
	prevSenal, prevPlazo := senalMatar, plazoMuerte
	senalMatar, plazoMuerte = func(int) {}, 20*time.Millisecond
	t.Cleanup(func() { senalMatar, plazoMuerte = prevSenal, prevPlazo })

	if _, err := m.Stop(id); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("Stop con el VMM del congelado vivo: %v", err)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateWarm {
		t.Fatalf("estado %s; quería warm, como estaba", viva.State)
	}
}

// rm: la tumba apunta el PID del VMM que sobrevivió para rematarlo.
func TestRemoveDeUnaWarmApuntaElVMMDeSuCongelado(t *testing.T) {
	m := newTestManager(t)
	id := "a1a1000000000003"
	pid, _ := congeladaConVMMVivo(t, m, id)
	prevSenal, prevPlazo := senalMatar, plazoMuerte
	senalMatar, plazoMuerte = func(int) {}, 20*time.Millisecond
	t.Cleanup(func() { senalMatar, plazoMuerte = prevSenal, prevPlazo })

	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	m.EsperarBorrados(5 * time.Second)
	m.mu.RLock()
	tumba := m.byID[id]
	m.mu.RUnlock()
	if tumba == nil || tumba.PID != pid {
		t.Fatalf("tumba = %+v; quería que conservara el pid %d", tumba, pid)
	}
}

// M5: un rm cuyo VMM sobrevive al SIGKILL no suelta sus volúmenes: otro run
// montaría en escritura un ext4 que ese VMM sigue escribiendo.
func TestRemoveConElVMMVivoConservaSusVolumenes(t *testing.T) {
	m := conVolumen(t, "datos")
	m.bus = events.New()
	id := "a1a1000000000004"
	maquinaCorriendo(t, m, id)
	m.mu.Lock()
	m.byID[id].Volumes = []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
	m.mu.Unlock()
	prevSenal, prevPlazo := senalMatar, plazoMuerte
	senalMatar, plazoMuerte = func(int) {}, 20*time.Millisecond
	t.Cleanup(func() { senalMatar, plazoMuerte = prevSenal, prevPlazo })

	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	m.EsperarBorrados(5 * time.Second)
	req := api.RunRequest{Volume: "datos"}
	if _, err := m.reservarVolumenes(req, newID(), "otra"); err == nil {
		t.Fatal("otro run tomó en escritura el volumen de un rm cuyo VMM sigue vivo")
	}

	// Muerto el VMM, la tumba se limpia y el volumen queda libre.
	senalMatar, plazoMuerte = prevSenal, prevPlazo
	m.reintentarTumbas()
	if _, err := m.reservarVolumenes(req, newID(), "otra"); err != nil {
		t.Fatalf("con el VMM ya muerto el volumen debería estar libre: %v", err)
	}
}
