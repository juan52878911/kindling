package machine

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// Un state.json de un kling más nuevo no se lee, no se aparta y no se pisa:
// NewManager se niega a arrancar y, si aun así se llega a load, el daemon queda
// en modo protegido sin escribir el estado.
func TestEstadoMasNuevoSeRechazaSinPisarlo(t *testing.T) {
	m := newTestManager(t)
	futuro := []byte(`{"schema": 99, "machines": [{"id":"aa11bb22cc33dd44"}], "algo_nuevo": true}`)
	if err := os.WriteFile(m.statePath(), futuro, 0o644); err != nil {
		t.Fatal(err)
	}

	err := comprobarVersionEstado(m.root)
	if !esquema.EsMasNuevo(err) {
		t.Fatalf("NewManager arrancaría con un estado del futuro: %v", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("el error no dice qué versión encontró: %v", err)
	}

	m.load()
	if !m.barridoBloqueado() {
		t.Error("con un estado del futuro el barrido sigue activo")
	}
	if len(m.byID) != 0 {
		t.Error("cargó máquinas de un formato que no entiende")
	}
	m.addForTest("bb22cc33dd44ee55") // persist con otra máquina en memoria
	m.writePending()
	if b, _ := os.ReadFile(m.statePath()); !bytes.Equal(b, futuro) {
		t.Fatalf("pisó el state.json del futuro:\n%s", b)
	}
	entradas, _ := os.ReadDir(m.root)
	for _, e := range entradas {
		if strings.Contains(e.Name(), sufijoCuarentena) {
			t.Errorf("apartó como corrupto un fichero que no lo está: %s", e.Name())
		}
	}
}

// El state.json de antes (un array, versión 0) se lee, se respalda antes de
// migrarlo y la escritura siguiente ya lleva el campo.
func TestEstadoVersion0SeMigraConCopia(t *testing.T) {
	m := newTestManager(t)
	viejo := []byte(`[{"id":"aa11bb22cc33dd44","name":"vieja","state":"stopped"}]`)
	if err := os.WriteFile(m.statePath(), viejo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := comprobarVersionEstado(m.root); err != nil {
		t.Fatalf("rechazó el formato anterior: %v", err)
	}
	m.load()
	if m.barridoBloqueado() {
		t.Fatalf("el formato anterior dejó el daemon en modo protegido: %s", m.estadoIlegible)
	}
	if mc := m.byID["aa11bb22cc33dd44"]; mc == nil || mc.Name != "vieja" {
		t.Fatalf("no cargó la máquina del formato anterior: %+v", m.byID)
	}
	b, err := os.ReadFile(esquema.RutaRespaldo(m.statePath(), 0))
	if err != nil || !bytes.Equal(b, viejo) {
		t.Fatalf("no dejó la copia del original: %q, %v", b, err)
	}

	m.mu.Lock()
	m.persist()
	m.mu.Unlock()
	m.writePending()
	st := readState(t, m) // falla si no lleva schema = versionEstado
	if len(st) != 1 || st[0].ID != "aa11bb22cc33dd44" {
		t.Fatalf("la migración perdió máquinas: %+v", st)
	}

	// Y lo que escribe se vuelve a leer.
	m2 := newTestManager(t)
	m2.root = m.root
	m2.load()
	if m2.barridoBloqueado() || m2.byID["aa11bb22cc33dd44"] == nil {
		t.Fatalf("no relee lo que escribe: %s %+v", m2.estadoIlegible, m2.byID)
	}
}

// Con una copia congelada en diferencial el estado se escribe con el esquema
// 2: un kling anterior se niega a arrancar en vez de cargar su mem.file
// disperso como si fuera la RAM entera. Sin ninguna, sigue siendo el 1.
func TestEstadoEsquemaConDiff(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["aa11bb22cc33dd44"] = &api.Machine{ID: "aa11bb22cc33dd44", Name: "copia", State: api.StateWarm}
	m.persist()
	m.mu.Unlock()
	m.writePending()
	b, _ := os.ReadFile(m.statePath())
	if v, _ := esquema.Version(b); v != versionEstado {
		t.Fatalf("sin diferenciales, schema %d; quería %d", v, versionEstado)
	}

	m.mu.Lock()
	m.byID["aa11bb22cc33dd44"].DiffBase = "/x/snapshots/dorado/mem.file"
	m.persist()
	m.mu.Unlock()
	m.writePending()
	b, _ = os.ReadFile(m.statePath())
	if v, _ := esquema.Version(b); v != versionEstadoDiff {
		t.Fatalf("con un diferencial, schema %d; quería %d", v, versionEstadoDiff)
	}
	if _, err := esquema.Comprobar(m.statePath(), b, versionEstado); !esquema.EsMasNuevo(err) {
		t.Fatalf("un binario que solo conoce el esquema %d lo aceptaría: %v", versionEstado, err)
	}
	m2 := newTestManager(t)
	m2.root = m.root
	m2.load()
	if m2.barridoBloqueado() || m2.byID["aa11bb22cc33dd44"] == nil || m2.byID["aa11bb22cc33dd44"].DiffBase == "" {
		t.Fatalf("no relee el esquema 2: %s %+v", m2.estadoIlegible, m2.byID)
	}
}
