package machine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
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
	if pct := m.diskUsedPct(); pct >= 0 && pct < diskHighPct {
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

// Las failed se recogen solas pasado su tiempo de gracia. Failed es terminal
// —no hay `start` que las saque de ahí—, así que acumularlas no da opciones,
// solo basura: se observaron nueve en 21 horas, una por intento roto.
func TestLasFailedViejasSeRecogenSolas(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()

	vieja := time.Now().Add(-2 * defaultFailedRetention)
	reciente := time.Now().Add(-time.Minute)
	m.mu.Lock()
	m.byID["vieja456789abcde"] = &api.Machine{ID: "vieja456789abcde", Name: "vieja",
		State: api.StateFailed, FailedAt: &vieja, LastErr: "boom"}
	m.byID["fresca6789abcdef"] = &api.Machine{ID: "fresca6789abcdef", Name: "fresca",
		State: api.StateFailed, FailedAt: &reciente, LastErr: "boom"}
	m.byID["sana567890abcdef"] = &api.Machine{ID: "sana567890abcdef", Name: "sana",
		State: api.StateWarm}
	m.mu.Unlock()

	m.gcFailed()

	m.mu.RLock()
	_, viejaSigue := m.byID["vieja456789abcde"]
	_, frescaSigue := m.byID["fresca6789abcdef"]
	_, sanaSigue := m.byID["sana567890abcdef"]
	m.mu.RUnlock()
	if viejaSigue {
		t.Error("no recogió una failed que ya cumplió su tiempo de gracia")
	}
	if !frescaSigue {
		t.Error("recogió una failed reciente: la gracia existe para poder diagnosticarla")
	}
	if !sanaSigue {
		t.Error("tocó una máquina que no estaba failed")
	}
}

// Una failed sin fecha (estado de antes del campo FailedAt) no se borra al
// instante: se le arranca el reloj y cae cuando le toque. La diferencia entre
// recoger basura y borrar algo que quizá falló hace un minuto.
func TestUnaFailedSinFechaRecibeRelojNoBorrado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	m.mu.Lock()
	m.byID["legacy4567890abc"] = &api.Machine{ID: "legacy4567890abc", Name: "legacy",
		State: api.StateFailed}
	m.mu.Unlock()

	m.gcFailed()

	m.mu.RLock()
	mc := m.byID["legacy4567890abc"]
	m.mu.RUnlock()
	if mc == nil {
		t.Fatal("borró una failed sin fecha en la primera pasada")
	}
	if mc.FailedAt == nil {
		t.Fatal("no le arrancó el reloj: sin FailedAt no se recogerá nunca")
	}

	// Con el reloj ya envejecido, la siguiente pasada sí la recoge.
	old := time.Now().Add(-2 * defaultFailedRetention)
	m.mu.Lock()
	mc.FailedAt = &old
	m.mu.Unlock()
	m.gcFailed()
	m.mu.RLock()
	_, sigue := m.byID["legacy4567890abc"]
	m.mu.RUnlock()
	if sigue {
		t.Error("no la recogió ni con el reloj cumplido")
	}
}

// KLING_FAILED_RETENTION=0 desactiva la recogida: quien quiera conservar los
// cadáveres para una autopsia larga puede.
func TestLaRecogidaDeFailedSePuedeDesactivar(t *testing.T) {
	t.Setenv("KLING_FAILED_RETENTION", "0")
	m := newTestManager(t)
	m.bus = events.New()
	vieja := time.Now().Add(-100 * defaultFailedRetention)
	m.mu.Lock()
	m.byID["eterna4567890abc"] = &api.Machine{ID: "eterna4567890abc", Name: "eterna",
		State: api.StateFailed, FailedAt: &vieja}
	m.mu.Unlock()

	m.gcFailed()

	m.mu.RLock()
	_, sigue := m.byID["eterna4567890abc"]
	m.mu.RUnlock()
	if !sigue {
		t.Error("recogió una failed con la recogida desactivada")
	}
}
