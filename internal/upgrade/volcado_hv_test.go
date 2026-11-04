package upgrade

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memHVCrudo arma un mem.file de kling-hv con la disposición de vm.c: RAM, la
// cabecera {magia, versión, …} en 4096 y el pie {magia, ram, hdrOff, hdrLen}
// en los últimos 32 bytes. cabMagia es la de la cabecera (normalmente la
// misma que la del pie).
func memHVCrudo(pieMagia, cabMagia uint64, version uint32, hdrOff, hdrLen uint64) []byte {
	const ram = 4096
	b := make([]byte, ram+64+32)
	binary.LittleEndian.PutUint64(b[ram:], cabMagia)
	binary.LittleEndian.PutUint32(b[ram+8:], version)
	pie := b[len(b)-32:]
	binary.LittleEndian.PutUint64(pie[0:], pieMagia)
	binary.LittleEndian.PutUint64(pie[8:], ram)
	binary.LittleEndian.PutUint64(pie[16:], hdrOff)
	binary.LittleEndian.PutUint64(pie[24:], hdrLen)
	return b
}

// Los casos de la cabecera KLNGHV que TestVersionVolcadoHV no cubre, vistos
// desde kling upgrade (CongeladaLegible al recongelar en el Mac): la cabecera
// v1, truncada, con la magia equivocada y de otra generación.
func TestVersionVolcadoHVCasos(t *testing.T) {
	const otraGeneracion = magiaHV&^0xffff | 0x3033 // "KLNGHV03"
	casos := []struct {
		nombre  string
		datos   []byte
		version int
		ok      bool
		err     string // "" si no hay error
	}{
		{"v2", memHVCrudo(magiaHV, magiaHV, 2, 4096, 64), 2, true, ""},
		{"v1 (cabecera más corta)", memHVCrudo(magiaHV, magiaHV, 1, 4096, 12), 1, true, ""},
		{"versión más nueva", memHVCrudo(magiaHV, magiaHV, 3, 4096, 64), 3, true, ""},
		{"vacío", nil, 0, false, ""},
		{"más corto que el pie", []byte("KLNGHV02"), 0, false, ""},
		// Solo queda el pie: la cabecera a la que apunta se cortó.
		{"truncado", memHVCrudo(magiaHV, magiaHV, 2, 4096, 64)[4096+64:], 0, true, "out of bounds"},
		{"hdr_len corto", memHVCrudo(magiaHV, magiaHV, 2, 4096, 8), 0, true, "out of bounds"},
		{"cabecera con otra magia", memHVCrudo(magiaHV, 0x1122334455667788, 2, 4096, 64), 0, true, "header is not KLNGHV"},
		// Antes pasaba por uno de kling-vz (ok false) y la recongelación lo
		// daba por legible para el kling-hv viejo.
		{"otra generación", memHVCrudo(otraGeneracion, otraGeneracion, 1, 4096, 64), 0, true, `"KLNGHV03"`},
	}
	for _, c := range casos {
		p := filepath.Join(t.TempDir(), "mem.file")
		if err := os.WriteFile(p, c.datos, 0o600); err != nil {
			t.Fatal(err)
		}
		v, ok, err := VersionVolcadoHV(p)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: error %v, quería %q", c.nombre, err, c.err)
			continue
		}
		if v != c.version || ok != c.ok {
			t.Errorf("%s: versión %d ok %v, quería %d %v", c.nombre, v, ok, c.version, c.ok)
		}
	}
}
