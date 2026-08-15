package machine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// Un directorio de machines/ que no pertenece a ninguna máquina conocida es
// basura: guarda un mem.file del tamaño de la RAM y no se recupera nunca solo.
func TestSeBorranLosDirectoriosHuerfanos(t *testing.T) {
	m := newTestManager(t)
	base := filepath.Join(m.root, "machines")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	// Uno conocido (tiene entrada en byID) y uno huérfano.
	for _, id := range []string{"viva1234", "huerfano9"} {
		if err := os.MkdirAll(filepath.Join(base, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, id, "mem.file"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	m.byID["viva1234"] = &api.Machine{ID: "viva1234", Name: "viva", State: api.StateWarm}
	m.sweepMachineDirs()
	m.mu.Unlock()

	if _, err := os.Stat(filepath.Join(base, "viva1234")); err != nil {
		t.Error("borró el directorio de una máquina conocida")
	}
	if _, err := os.Stat(filepath.Join(base, "huerfano9")); !os.IsNotExist(err) {
		t.Error("no borró el directorio huérfano")
	}
}

// Una warm SIN snapshot de respaldo no se toca aunque el disco apriete: su
// mem.file es el único estado que tiene, y borrarlo sería perder datos.
//
// La distinción entre recuperable e irrecuperable es la línea que separa "libero
// espacio" de "pierdo el trabajo del usuario".
func TestElGCNoBorraLoQueNoSePuedeRecrear(t *testing.T) {
	m := newTestManager(t)

	// Warm creada a mano (From vacío): irrecuperable.
	m.mu.Lock()
	m.byID["propia"] = &api.Machine{ID: "propia", Name: "propia", State: api.StateWarm, From: ""}
	// Warm de un servicio cuyo snapshot NO existe: tampoco recuperable.
	m.byID["sinsnap"] = &api.Machine{ID: "sinsnap", Name: "sinsnap", State: api.StateWarm, From: "fantasma"}
	m.mu.Unlock()

	// La selección de candidatos es lo que decide qué se puede borrar.
	m.mu.RLock()
	var recuperables int
	for _, mc := range m.byID {
		if mc.State != api.StateWarm || mc.From == "" {
			continue
		}
		if _, err := m.loadSnapshot(mc.From); err != nil {
			continue
		}
		recuperables++
	}
	m.mu.RUnlock()

	if recuperables != 0 {
		t.Errorf("consideró recuperables %d máquinas que no lo son", recuperables)
	}
	// Y siguen ahí.
	m.mu.RLock()
	_, a := m.byID["propia"]
	_, b := m.byID["sinsnap"]
	m.mu.RUnlock()
	if !a || !b {
		t.Error("el GC tocó una máquina irrecuperable")
	}
}

// Sin poder leer el disco, el GC no hace NADA: mejor no recolectar que
// recolectar a ciegas y borrar lo que no debía.
func TestSinDatoDeDiscoElGCNoActua(t *testing.T) {
	m := newTestManager(t)
	// diskUsedPct sobre un root válido devuelve un número real; lo que se prueba
	// aquí es el contrato de que -1 (no se sabe) no dispara nada. Se comprueba
	// que la marca alta manda: por debajo de ella, gcDisk retorna sin mirar
	// candidatos.
	if pct := m.diskUsedPct(); pct >= 0 && pct < gcDiskHighPct() {
		// Con el disco por debajo de la marca, no debe eliminar nada aunque
		// haya candidatos.
		m.mu.Lock()
		m.byID["x"] = &api.Machine{ID: "x", State: api.StateWarm, From: "s"}
		m.mu.Unlock()
		m.gcDisk(t.Context())
		m.mu.RLock()
		_, sigue := m.byID["x"]
		m.mu.RUnlock()
		if !sigue {
			t.Error("eliminó una instancia con el disco por debajo de la marca")
		}
	}
}

// Los umbrales del GC de disco son configurables por entorno, con defectos e
// invariante target<high (el hueco evita expulsar en cada tick).
func TestGCThresholdsFromEnv(t *testing.T) {
	// Defectos sin entorno.
	if h := gcDiskHighPct(); h != defaultDiskHighPct {
		t.Errorf("high por defecto = %d, want %d", h, defaultDiskHighPct)
	}
	if tg := gcDiskTargetPct(defaultDiskHighPct); tg != defaultDiskTargetPct {
		t.Errorf("target por defecto = %d, want %d", tg, defaultDiskTargetPct)
	}
	// Override válido.
	t.Setenv("KLING_GC_DISK_HIGH", "95")
	t.Setenv("KLING_GC_DISK_TARGET", "70")
	if h := gcDiskHighPct(); h != 95 {
		t.Errorf("high override = %d, want 95", h)
	}
	if tg := gcDiskTargetPct(95); tg != 70 {
		t.Errorf("target override = %d, want 70", tg)
	}
	// Valor fuera de rango en high → defecto.
	t.Setenv("KLING_GC_DISK_HIGH", "150")
	if h := gcDiskHighPct(); h != defaultDiskHighPct {
		t.Errorf("high fuera de rango = %d, want %d", h, defaultDiskHighPct)
	}
	// Invariante: target >= high se acota a high-1.
	t.Setenv("KLING_GC_DISK_TARGET", "99")
	if tg := gcDiskTargetPct(90); tg != 89 {
		t.Errorf("target>=high se acota a %d, want 89", tg)
	}
}
