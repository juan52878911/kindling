//go:build darwin && arm64

package hvvm

import (
	"testing"
)

// TestSysregISS: los ISS medidos en el M4 al arrancar con EL2 se traducen a
// la codificación de hv_sys_reg_t.
func TestSysregISS(t *testing.T) {
	for _, c := range []struct {
		iss  uint64
		want uint16
	}{
		{0x313802, 0xe708}, // MSR CNTHCTL_EL2, x0 (init_el2)
		{0x313803, 0xe708}, // lo mismo como lectura
		{0x362be8, 0xc523}, // MSR LORC_EL1, xzr
	} {
		if got := sysregISS(c.iss); got != c.want {
			t.Errorf("iss %#x: %#x, want %#x", c.iss, got, c.want)
		}
	}
}

// TestSEADestino: un data abort sin ISV (un MMIO que no se puede emular)
// terminaba el VMM; ahora el invitado recibe un aborto externo síncrono en el
// vector que le toca, como en una máquina física.
func TestSEADestino(t *testing.T) {
	const tge = 1 << 27
	// Sin ISV, un STP (WnR) de 32 bits (IL): EC 0x24, IL, WnR.
	const syn = 0x24<<26 | 1<<25 | 1<<6
	for _, c := range []struct {
		nombre     string
		cpsr, hcr  uint64
		el2        bool
		el         int
		off, nuevo uint64
		esr        uint64
	}{
		{"EL1h kernel", 0x3c5, 0, false, 1, 0x200, 0x3c5, 0x25<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL1t", 0x3c4, 0, false, 1, 0x000, 0x3c5, 0x25<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL0 user", 0x0, 0, false, 1, 0x400, 0x3c5, 0x24<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL0 AArch32", 0x10, 0, false, 1, 0x600, 0x3c5, 0x24<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL2h with EL2", 0x3c9, 0, true, 2, 0x200, 0x3c9, 0x25<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL0 under VHE (TGE)", 0x0, tge, true, 2, 0x400, 0x3c9, 0x24<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL0 without TGE", 0x0, 0, true, 1, 0x400, 0x3c5, 0x24<<26 | 1<<25 | 1<<6 | 0x10},
		{"EL1 nested guest", 0x3c5, 0, true, 1, 0x200, 0x3c5, 0x25<<26 | 1<<25 | 1<<6 | 0x10},
	} {
		el, off, cpsr, esr := seaDestino(c.cpsr, c.hcr, c.el2, syn)
		if el != c.el || off != c.off || cpsr != c.nuevo || esr != c.esr {
			t.Errorf("%s: EL%d off %#x cpsr %#x esr %#x; want EL%d off %#x cpsr %#x esr %#x",
				c.nombre, el, off, cpsr, esr, c.el, c.off, c.nuevo, c.esr)
		}
	}
	// Una lectura (sin WnR) no lo pone en el ESR.
	if _, _, _, esr := seaDestino(0x3c5, 0, false, 0x24<<26|1<<25); esr&(1<<6) != 0 {
		t.Errorf("read: esr %#x has WnR", esr)
	}
}

// TestEL2Soportado solo deja constancia: en un M3 o posterior con macOS 15
// debería dar true (medido en el M4 con macOS 26.5).
func TestEL2Soportado(t *testing.T) {
	t.Logf("EL2 supported: %v", EL2Soportado())
}
