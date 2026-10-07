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
	t.Setenv("KLING_SQUEEZE_ON_READY", "0")
	t.Setenv("KLING_STOPPED_RETENTION", "0")
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
		"KLING_SQUEEZE_ON_READY":  "off",
		"KLING_STOPPED_RETENTION": "off",
		"KLING_COW_STORE_GIB":     "auto",
	} {
		if a[k] != want {
			t.Errorf("%s = %q, quería %q", k, a[k], want)
		}
	}
	if n, err := strconv.Atoi(a["KLING_MAX_PARALLEL_BOOT"]); err != nil || n < 1 {
		t.Errorf("KLING_MAX_PARALLEL_BOOT = %q", a["KLING_MAX_PARALLEL_BOOT"])
	}
}

// Los ajustes de la memoria y de las paradas también se dicen: sin ellos,
// GET /info no contaba si el apretón al estar lista o el informe de páginas
// libres estaban encendidos.
func TestAjustesIncluyenMemoriaYParadas(t *testing.T) {
	t.Setenv("KLING_STOPPED_RETENTION", "2h")
	m := newTestManager(t)
	m.cow.gib = 8
	a := m.Ajustes()
	for _, k := range []string{"KLING_SQUEEZE_ON_READY", "KLING_FREE_PAGE_REPORTING"} {
		if a[k] != "on" && a[k] != "off" {
			t.Errorf("%s = %q", k, a[k])
		}
	}
	if a["KLING_STOPPED_RETENTION"] != "2h0m0s" || a["KLING_COW_STORE_GIB"] != "8" {
		t.Errorf("retención %q, almacén %q", a["KLING_STOPPED_RETENTION"], a["KLING_COW_STORE_GIB"])
	}
}
