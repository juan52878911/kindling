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

// TestEL2Soportado solo deja constancia: en un M3 o posterior con macOS 15
// debería dar true (medido en el M4 con macOS 26.5).
func TestEL2Soportado(t *testing.T) {
	t.Logf("EL2 supported: %v", EL2Soportado())
}
