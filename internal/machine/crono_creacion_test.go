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
	g := strings.Index(s, "m.enterRestore(ctx)")
	v := strings.Index(s, "fases.marca(fcVMM)")
	if p < 0 || g < 0 || v < 0 || !(p < g && g < v) {
		t.Fatalf("orden roto: persistirYa=%d enterRestore=%d vmm=%d", p, g, v)
	}
}
