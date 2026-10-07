package machine

import (
	"strconv"
	"testing"
)

// Ajustes dice lo que se APLICA: el valor del entorno si es válido, el
// defecto si no.
func TestAjustesEfectivos(t *testing.T) {
	t.Setenv("KLING_MAX_MACHINES", "512")
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "basura")
	t.Setenv("KLING_GC_DISK_HIGH", "80")
	t.Setenv("KLING_GC_DISK_TARGET", "95") // por encima de la marca alta: se acota
	t.Setenv("KLING_READY_BOOST", "0")
	m := newTestManager(t)
	a := m.Ajustes()
	for k, want := range map[string]string{
		"KLING_MAX_MACHINES":      "512",
		"KLING_MIN_FREE_DISK_MIB": strconv.Itoa(minDiscoLibrePlataforma),
		"KLING_GC_DISK_HIGH":      "80",
		"KLING_GC_DISK_TARGET":    "79",
		"KLING_READY_BOOST":       "off",
		"KLING_JAILER":            "off",
		"KLING_FAILED_RETENTION":  "1h0m0s",
	} {
		if a[k] != want {
			t.Errorf("%s = %q, quería %q", k, a[k], want)
		}
	}
	if n, err := strconv.Atoi(a["KLING_MAX_PARALLEL_BOOT"]); err != nil || n < 1 {
		t.Errorf("KLING_MAX_PARALLEL_BOOT = %q", a["KLING_MAX_PARALLEL_BOOT"])
	}
}
