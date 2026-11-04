package machine

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// vz/ es otro módulo y su internal/ no se puede importar desde aquí: los
// formatos que lee el VMM se copian a mano en meta.go (formatoVZMax, magiaHV,
// versionHVMax). Este test lee las constantes de las fuentes de vz/ para que
// subir una sin la otra lo rompa: con formatoVZMax por detrás, el daemon
// daría por obsoletos todos los snapshots nuevos (los dorados incluidos).
func TestFormatosCasanConVZ(t *testing.T) {
	raiz := filepath.Join("..", "..", "vz", "internal")
	casos := []struct {
		fichero, patron string
		base            int
		quiero          uint64
		nombre          string
	}{
		{"spec/spec.go", `(?m)^const FormatVersion = (\d+)$`, 10, formatoVZMax, "formatoVZMax"},
		{"hvvm/khv.h", `(?m)^#define KHV_SNAP_VERSION (\d+)\b`, 10, versionHVMax, "versionHVMax"},
		{"hvvm/vm.c", `(?m)^#define SNAP_MAGIC 0x([0-9a-fA-F]+)ULL\b`, 16, magiaHV, "magiaHV"},
	}
	for _, c := range casos {
		ruta := filepath.Join(raiz, filepath.FromSlash(c.fichero))
		b, err := os.ReadFile(ruta)
		if err != nil {
			t.Fatalf("%s: %v", ruta, err)
		}
		m := regexp.MustCompile(c.patron).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no encuentro %s (¿cambió cómo se declara?)", ruta, c.patron)
		}
		v, err := strconv.ParseUint(string(m[1]), c.base, 64)
		if err != nil {
			t.Fatalf("%s: %v", ruta, err)
		}
		if v != c.quiero {
			f := "%s dice %d y meta.go %s = %d: súbelos juntos"
			if c.base == 16 {
				f = "%s dice %#x y meta.go %s = %#x: súbelos juntos"
			}
			t.Errorf(f, ruta, v, c.nombre, c.quiero)
		}
	}
	if magiaHV>>16 != magiaHVPrefix {
		t.Errorf("magiaHVPrefix %#x no es el prefijo de magiaHV %#x", magiaHVPrefix, magiaHV)
	}
}
