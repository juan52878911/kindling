package daemon

import (
	"strings"
	"testing"
)

// Al arrancar, el daemon avisa si el disco está por debajo del mínimo de
// admisión o cerca de él, y si no se pudo medir; con disco de sobra o la
// comprobación apagada, nada.
func TestAvisoDiscoAlArrancar(t *testing.T) {
	for _, c := range []struct {
		libre, minimo int64
		quiero        string
	}{
		{100, 2048, "below the 2048 MiB minimum"},
		{3000, 2048, "close to the 2048 MiB minimum"},
		{-1, 2048, "couldn't measure"},
		{5000, 2048, ""},
		{100, 0, ""},
	} {
		got := avisoDisco(c.libre, c.minimo, "/var/lib/kling")
		if c.quiero == "" {
			if got != "" {
				t.Errorf("%d/%d: aviso de más: %q", c.libre, c.minimo, got)
			}
			continue
		}
		if !strings.Contains(got, c.quiero) || !strings.Contains(got, "/var/lib/kling") {
			t.Errorf("%d/%d: %q, quería %q", c.libre, c.minimo, got, c.quiero)
		}
	}
}
