package machine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// La recogida de failed no se lleva lo que no existe en otro sitio: una failed
// con su volcado completo (un Thaw que falló: es el único estado de esa warm),
// una con volúmenes, o una arrancada en frío que murió corriendo (su overlay es
// su único disco). La inservible de verdad sí se recoge.
func TestGCFailedNoBorraMaquinasConDatos(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	vieja := time.Now().Add(-2 * defaultFailedRetention)

	conVolcado := "c0da7a0000000001"
	if err := os.MkdirAll(m.dir(conVolcado), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(m.dir(conVolcado), f), []byte("estado"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	conVolumen := "c0da7a0000000002"
	enFrio := "c0da7a0000000003"
	inservible := "c0da7a0000000004"

	m.mu.Lock()
	m.byID[conVolcado] = &api.Machine{ID: conVolcado, Name: "volcado", State: api.StateFailed,
		FailedAt: &vieja, From: "svc", LastErr: "thaw failed"}
	m.byID[conVolumen] = &api.Machine{ID: conVolumen, Name: "volumen", State: api.StateFailed,
		FailedAt: &vieja, From: "svc", Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}}
	m.byID[enFrio] = &api.Machine{ID: enFrio, Name: "frio", State: api.StateFailed,
		FailedAt: &vieja, LastErr: errProcesoDesaparecido}
	m.byID[inservible] = &api.Machine{ID: inservible, Name: "rota", State: api.StateFailed,
		FailedAt: &vieja, From: "svc", LastErr: "boot failed"}
	m.mu.Unlock()

	m.gcFailed()

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range []string{conVolcado, conVolumen, enFrio} {
		if _, sigue := m.byID[id]; !sigue {
			t.Errorf("recogió %s, que tenía datos propios", id)
		}
	}
	if _, err := os.Stat(filepath.Join(m.dir(conVolcado), "mem.file")); err != nil {
		t.Errorf("el volcado de la failed desapareció: %v", err)
	}
	if _, sigue := m.byID[inservible]; sigue {
		t.Error("no recogió la failed sin nada propio")
	}
}

// Una warm con volúmenes no es "recreable desde su dorado": no es candidata
// del GC de disco.
func TestGCDiscoNoEsCandidataUnaWarmConVolumenes(t *testing.T) {
	m := newTestManager(t)
	mc := &api.Machine{ID: "c0da7a0000000005", State: api.StateWarm, From: "svc",
		Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}}
	if m.retieneDatos(mc) == "" {
		t.Error("una warm con volúmenes cuenta como sin datos propios")
	}
	if m.retieneDatos(&api.Machine{ID: "c0da7a0000000006", State: api.StateWarm, From: "svc"}) != "" {
		t.Error("una warm sin volúmenes de un dorado debería poder recrearse")
	}
}
