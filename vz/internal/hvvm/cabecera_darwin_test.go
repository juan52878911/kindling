//go:build darwin && arm64

package hvvm

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lo que hay en disco: un mem.file de kling-hv lleva al final el pie y, en
// hdr_off, la struct snap_hdr cruda de vm.c. Su disposición es un formato: si
// cambia el tamaño de la cabecera sin subir SNAP_VERSION (y la magia, si deja
// de poder leerse la anterior), los mem.file que ya hay se leerían mal o se
// rechazarían con un error que no dice por qué. El daemon (internal/machine,
// causaFormatoVolcado) lee la magia y la versión con las mismas constantes.
const (
	magiaFijada   = 0x4b4c4e4748563032 // "KLNGHV02"
	versionFijada = 2
	hdrFijada     = 11696 // sizeof(struct snap_hdr) en la versión 2
	hdrV1Fijada   = 11384 // offsetof(struct snap_hdr, hay_vsock): la versión 1
	pieFijado     = 32
)

func TestCabeceraKLNGHV(t *testing.T) {
	magia, version, hdr, hdrV1, pie := formatoSnap()
	if magia != magiaFijada || version != versionFijada || hdr != hdrFijada || hdrV1 != hdrV1Fijada || pie != pieFijado {
		t.Fatalf("the kling-hv memory file layout changed: magic %#x version %d header %d (v1 %d) footer %d; "+
			"fixed: %#x %d %d (%d) %d. If struct snap_hdr changed, bump SNAP_VERSION (and read the old one), "+
			"update internal/machine (magiaHV, versionHVMax) and only then these constants",
			magia, version, hdr, hdrV1, pie, magiaFijada, versionFijada, hdrFijada, hdrV1Fijada, pieFijado)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], magia)
	if string(b[:]) != "KLNGHV02" {
		t.Fatalf("magic reads %q", b)
	}
}

// memFile escribe un mem.file con la cabecera dada (solo magia y versión; el
// resto a cero) y el pie con hdrLen.
func memFile(t *testing.T, magia uint64, version uint32, hdrLen uint64) string {
	t.Helper()
	const ram = 4096
	b := make([]byte, ram+int(hdrLen)+pieFijado)
	binary.LittleEndian.PutUint64(b[ram:], magia)
	binary.LittleEndian.PutUint32(b[ram+8:], version)
	p := b[ram+int(hdrLen):]
	binary.LittleEndian.PutUint64(p[0:], magia)
	binary.LittleEndian.PutUint64(p[8:], ram)
	binary.LittleEndian.PutUint64(p[16:], ram)
	binary.LittleEndian.PutUint64(p[24:], hdrLen)
	ruta := filepath.Join(t.TempDir(), "mem.file")
	if err := os.WriteFile(ruta, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return ruta
}

// khv_restore rechaza otra magia, un hdr_len que no es de ninguna versión y
// una versión que no lee, con los mensajes que el daemon traduce
// (explicarRestauracion en internal/machine/meta.go). Una cabecera buena pasa
// de ahí y falla al compararla con la máquina (aquí no hay ninguna).
func TestRestaurarCabeceraKLNGHV(t *testing.T) {
	casos := []struct {
		nombre  string
		magia   uint64
		version uint32
		hdrLen  uint64
		quiero  string
	}{
		{"otra magia", 0x4b4c4e4748563031, versionFijada, hdrFijada, "is not a kling-hv memory file"},
		{"hdr_len de nadie", magiaFijada, versionFijada, hdrFijada - 8, "is not a kling-hv memory file"},
		{"versión del futuro", magiaFijada, versionFijada + 1, hdrFijada, "this kling-hv reads 2"},
		{"v1 con la cabecera de la v2", magiaFijada, 1, hdrFijada, "this kling-hv reads 2"},
		{"v2", magiaFijada, versionFijada, hdrFijada, "does not match the configured machine"},
		{"v1", magiaFijada, 1, hdrV1Fijada, "does not match the configured machine"},
	}
	for _, c := range casos {
		err := restaurarSinVM(memFile(t, c.magia, c.version, c.hdrLen))
		if err == nil || !strings.Contains(err.Error(), c.quiero) {
			t.Errorf("%s: %v; quería %q", c.nombre, err, c.quiero)
		}
	}
}
