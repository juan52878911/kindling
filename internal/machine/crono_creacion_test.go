package machine

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Cada creación anota todas las fases y el total; la suma de fases es el
// total (±5 %): una fase mal colocada daría números engañosos.
func TestCronoCreacionSumaYCuentas(t *testing.T) {
	const n = 5
	var m Manager
	antes := m.Telemetria()
	for i := 0; i < n; i++ {
		c := nuevoCronoCreacion()
		for f := 0; f < numFasesCreacion-1; f++ {
			time.Sleep(2 * time.Millisecond)
			c.marca(f)
		}
		p := c.cerrar()
		var suma float64
		for _, ms := range msFasesCreacion(p) {
			suma += ms
		}
		if math.Abs(suma-p.TotalMS) > 0.05*p.TotalMS {
			t.Fatalf("suma de fases %.2f ms != total %.2f ms", suma, p.TotalMS)
		}
		tel.creacion(p)
	}
	des := m.Telemetria()
	if got := des.DuracionCreacion.Cuenta - antes.DuracionCreacion.Cuenta; got != n {
		t.Fatalf("total: count +%d, quería %d", got, n)
	}
	for _, f := range FasesCreacion {
		if got := des.FasesCreacionH[f].Cuenta - antes.FasesCreacionH[f].Cuenta; got != n {
			t.Errorf("fase %s: count +%d, quería %d", f, got, n)
		}
	}
}

// En runFromCon la máquina se escribe en disco antes de la puerta y antes de
// que exista el VMM (invariante de persistirYa). Se vigila sobre el fuente:
// lanzar un VMM necesita Firecracker y root.
func TestRunFromConPersisteAntesDelVMM(t *testing.T) {
	b, err := os.ReadFile("snapshot.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	ini := strings.Index(s, "func (m *Manager) runFromCon(")
	if ini < 0 {
		t.Fatal("no encuentro runFromCon")
	}
	s = s[ini:]
	p := strings.Index(s, "m.persistirYa()")
	sp := strings.Index(s, "m.spawnJailed(")
	v := strings.Index(s, "fases.marca(fcVMM)")
	if p < 0 || sp < 0 || v < 0 || !(p < sp && sp < v) {
		t.Fatalf("orden roto: persistirYa=%d spawnJailed=%d vmm=%d", p, sp, v)
	}
}

// G-02: la puerta de restauraciones cubre la carga en el VMM y no el
// lanzamiento: se entra con el VMM ya lanzado (con o sin jailer), su socket
// esperado y el jail poblado, y antes de LoadSnapshotRepoint. Sobre el fuente,
// como el de arriba: lanzar un VMM necesita Firecracker y root.
func TestRunFromConPuertaSoloEnLaCarga(t *testing.T) {
	b, err := os.ReadFile("snapshot.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	ini := strings.Index(s, "func (m *Manager) runFromUna(")
	if ini < 0 {
		t.Fatal("no encuentro runFromUna")
	}
	s = s[ini:]
	fin := strings.Index(s, "\nfunc ")
	if fin > 0 {
		s = s[:fin]
	}
	g := strings.Index(s, "m.enterRestore(ctx)")
	if g < 0 || strings.Count(s, "m.enterRestore(ctx)") != 1 {
		t.Fatal("runFromUna debe entrar en la puerta una sola vez")
	}
	for _, antes := range []string{"m.spawnJailed(", "m.prepareJail(", "m.spawn(id", "m.waitSocket(", "fases.marca(fcVMM)"} {
		if i := strings.LastIndex(s, antes); i < 0 || i > g {
			t.Errorf("%s va dentro de la puerta (o no está): %d frente a enterRestore=%d", antes, i, g)
		}
	}
	for _, despues := range []string{"LoadSnapshotRepoint(", "fases.marca(fcGateWait)", "c.Resume(ctx)"} {
		if i := strings.Index(s, despues); i < 0 || i < g {
			t.Errorf("%s va antes de la puerta (o no está): %d frente a enterRestore=%d", despues, i, g)
		}
	}
	// Un fallo en la cola tiene que matar el VMM ya lanzado: por abortar.
	if !strings.Contains(s[g:], "return abortar(glErr)") {
		t.Error("un error al entrar en la puerta no pasa por abortar")
	}
}
