//go:build darwin && arm64

package hvvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La RAM de una congelación es un clon APFS del fichero que la respalda (el
// dorado o la congelación anterior), y un clon hereda su modo: de un dorado
// 0644 salía una congelación 0644 con toda la RAM del invitado. La rama de
// copia ya la creaba 0600.
func TestClonRAMEs0600(t *testing.T) {
	dir := t.TempDir()
	base, out := filepath.Join(dir, "mem.base"), filepath.Join(dir, "mem.file")
	if err := os.WriteFile(base, make([]byte, 1<<16), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o644); err != nil { // sin la umask
		t.Fatal(err)
	}
	switch m := clonRAM(base, out); {
	case m < 0:
		t.Fatalf("cloning the RAM file: errno %d", -m)
	case m != 0o600:
		t.Fatalf("the frozen RAM file is %#o, want 0600", m)
	}
}

// Issue #183: en el M4 HVF anuncia sve2/svei8mm/svebf16 en ID_AA64ZFR0_EL1 sin
// SVE, y OpenSSL moría con SIGILL. Al invitado no le llega nada de SVE ni de
// SME; el resto de los campos se queda como venía.
func TestFiltrarIDQuitaSVEySME(t *testing.T) {
	const (
		sve = 0xf << 32
		sme = 0xf << 24
	)
	pfr0 := uint64(0x1101_0000_1111_2222) | 1<<32 // EL0-EL3 y SVE=1
	pfr1 := uint64(0x0000_0000_0200_0121)         // SME=2, BT, SSBS, MTE
	p0, p1, z, s := filtrarID(pfr0, pfr1, 0x0110_0110_0001_0001, 0x8000_0000_0000_0000)
	if p0&sve != 0 || p0 != pfr0&^sve {
		t.Fatalf("PFR0 %#x: SVE must be cleared and nothing else (want %#x)", p0, uint64(pfr0&^sve))
	}
	if p1&sme != 0 || p1 != pfr1&^sme {
		t.Fatalf("PFR1 %#x: SME must be cleared and nothing else (want %#x)", p1, uint64(pfr1&^sme))
	}
	if z != 0 || s != 0 {
		t.Fatalf("ZFR0 %#x SMFR0 %#x: both must be zero", z, s)
	}
}

// Un vCPU con un CPU_ON pendiente al congelar se guarda como si ya hubiera
// entrado: registros de sistema de reinicio, PC en la entrada, X0 con el
// contexto y encendido; lo demás (GIC, timer) como estaba.
func TestCPUOnPendienteEnElSnapshot(t *testing.T) {
	if r := pruebaCPUOn(); r != 0 {
		t.Fatalf("khv_prueba_cpu_on failed check %d", r)
	}
}

// El resumen de KLING_HV_TRACE: las fases de antes de soltar la VM por
// orden, la espera, los hitos desde que se soltó y el total hasta el agente.
func TestTrazaResumen(t *testing.T) {
	r := trazaPrueba()
	for _, s := range []string{"trace boot ms:", " ram ", " kernel+fdt ", " vcpus ", " wait ", "->primer-run ", "->agente ", "| total "} {
		if !strings.Contains(r, s) {
			t.Errorf("summary %q lacks %q", r, s)
		}
	}
	if i, j := strings.Index(r, " ram "), strings.Index(r, " kernel+fdt "); i < 0 || j < i {
		t.Errorf("summary %q: phases out of order", r)
	}
	for _, s := range []string{"pausa", "snap-", "estado-cpu", "->primer-uart"} {
		if strings.Contains(r, s) {
			t.Errorf("summary %q has %q, which did not happen before the agent", r, s)
		}
	}
}

func TestFinMotivoSinVM(t *testing.T) {
	if m := finMotivo(); m != 0 {
		t.Fatalf("khv_fin_motivo without a VM = %d, want 0 (KHV_FIN_NADA)", m)
	}
}
