package machine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// C9-19: el volcado de un dorado tiene más plazo que el de un congelado. Un
// dorado de un modelo grande llega al volcado tras pagar toda la carga, y un
// volumen degradado o compartido (20-50 MiB/s) lo tiraba por plazo; esperar
// más a un VMM que sigue escribiendo cuesta menos que repetir la carga. Nunca
// menos que plazoVolcado, y crece con la memoria.
func TestPlazoVolcadoDoradoGrande(t *testing.T) {
	for mib := 0; mib <= 64<<10; mib += 256 {
		d := plazoVolcadoDorado(mib)
		if d < plazoVolcado(mib) {
			t.Fatalf("plazoVolcadoDorado(%d) = %v, menor que el del congelado %v", mib, d, plazoVolcado(mib))
		}
		if plazoVolcadoDorado(mib+256) < d {
			t.Fatalf("el plazo del dorado baja al pasar de %d a %d MiB", mib, mib+256)
		}
	}
	// 8 GiB a 20 MiB/s son ~410 s: un disco malo pero vivo tiene que caber.
	if d := plazoVolcadoDorado(8192); d < 410*time.Second {
		t.Errorf("plazoVolcadoDorado(8 GiB) = %v: no caben 8 GiB a 20 MiB/s", d)
	}
	// Y el de un modelo pequeño, más que los 30 s de suelo de hoy.
	if d := plazoVolcadoDorado(1600); d <= plazoVolcado(1600) {
		t.Errorf("plazoVolcadoDorado(1600 MiB) = %v, igual que el del congelado", d)
	}
}

// M15: el volcado temporal de un commit cortado (.commit-mem.file, del tamaño
// de la RAM) no se queda en el directorio de la máquina: lo barren reconcile
// al arrancar el daemon y Stop.
func TestVolcadoTemporalDeCommitSeBarre(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	plantar := func(id string) {
		t.Helper()
		for _, n := range []string{volcadoTmpMem, volcadoTmpSnap} {
			if err := os.WriteFile(filepath.Join(m.dir(id), n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	quedan := func(id string) bool {
		_, err1 := os.Lstat(filepath.Join(m.dir(id), volcadoTmpMem))
		_, err2 := os.Lstat(filepath.Join(m.dir(id), volcadoTmpSnap))
		return err1 == nil || err2 == nil
	}

	// reconcile: una parada con el temporal de antes del reinicio.
	a := newID()
	m.byID[a] = &api.Machine{ID: a, Name: "a", State: api.StateStopped}
	if err := os.MkdirAll(m.dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	plantar(a)
	m.reconcile()
	if quedan(a) {
		t.Fatal("reconcile dejó el volcado temporal del commit")
	}

	// Stop: una running sin VMM (el kill no tiene a quién matar).
	b := newID()
	m.byID[b] = &api.Machine{ID: b, Name: "b", State: api.StateRunning}
	if err := os.MkdirAll(m.dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	plantar(b)
	if _, err := m.Stop(b); err != nil {
		t.Fatal(err)
	}
	if quedan(b) {
		t.Fatal("Stop dejó el volcado temporal del commit")
	}
	if _, err := os.Stat(m.dir(b)); err != nil {
		t.Fatalf("Stop se llevó el directorio: %v", err)
	}
}

// Un commit cortado por un reinicio, con el VMM aún volcando: reconcile le
// espera lo mismo que el commit le daba. El de un dorado, plazoVolcadoDorado;
// el temporal de un fork, plazoVolcado. Una marca sin tipo (las de antes) es
// la de un dorado: esperar de más a un VMM que contesta «ocupado» no cuesta.
func TestPlazoDelCommitCortadoSegunLaMarca(t *testing.T) {
	const mib = 8192
	casos := []struct {
		nombre string
		marcar func(dir string) error
		quiere time.Duration
	}{
		{"dorado", func(dir string) error { return marcarCommitEnCurso(dir, "dorado", 4321, false) }, plazoVolcadoDorado(mib)},
		{"fork", func(dir string) error { return marcarCommitEnCurso(dir, "tmp-fork", 4321, true) }, plazoVolcado(mib)},
		{"fork sin PID", func(dir string) error { return marcarCommitEnCurso(dir, "tmp-fork", 0, true) }, plazoVolcado(mib)},
		{"marca vieja", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, marcaCommit), []byte("dorado\n4321\n"), 0o600)
		}, plazoVolcadoDorado(mib)},
	}
	for _, c := range casos {
		dir := t.TempDir()
		if err := c.marcar(dir); err != nil {
			t.Fatal(err)
		}
		if got := plazoCommitCortado(dir, mib); got != c.quiere {
			t.Errorf("%s: plazo %v, quería %v", c.nombre, got, c.quiere)
		}
		if hay, pid := marcaCommitDe(dir); !hay || (c.nombre != "fork sin PID" && pid != 4321) || (c.nombre == "fork sin PID" && pid != 0) {
			t.Errorf("%s: marcaCommitDe = %v, %d", c.nombre, hay, pid)
		}
	}
}
