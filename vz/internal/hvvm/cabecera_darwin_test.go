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
	versionFijada = 4
	hdrFijada     = 11776 // sizeof(struct snap_hdr) en la versión 4 (el globo con la cola del informe de libres)
	hdrV3Fijada   = 11744 // la versión 3: el globo con la cola de estadísticas y sin la del informe
	hdrV2Fijada   = 11696 // la versión 2: el globo sin esa cola
	hdrV1Fijada   = 11384 // offsetof(struct snap_hdr, hay_vsock): la versión 1
	pieFijado     = 32
)

func TestCabeceraKLNGHV(t *testing.T) {
	magia, version, hdr, hdrV1, hdrV2, hdrV3, pie := formatoSnap()
	if magia != magiaFijada || version != versionFijada || hdr != hdrFijada || hdrV1 != hdrV1Fijada || hdrV2 != hdrV2Fijada ||
		hdrV3 != hdrV3Fijada || pie != pieFijado {
		t.Fatalf("the kling-hv memory file layout changed: magic %#x version %d header %d (v1 %d, v2 %d, v3 %d) footer %d; "+
			"fixed: %#x %d %d (%d, %d, %d) %d. If struct snap_hdr changed, bump SNAP_VERSION (and read the old one), "+
			"update internal/machine (magiaHV, versionHVMax) and only then these constants",
			magia, version, hdr, hdrV1, hdrV2, hdrV3, pie, magiaFijada, versionFijada, hdrFijada, hdrV1Fijada, hdrV2Fijada,
			hdrV3Fijada, pieFijado)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], magia)
	if string(b[:]) != "KLNGHV02" {
		t.Fatalf("magic reads %q", b)
	}
}

// memFile escribe un mem.file con la cabecera dada (solo magia, versión y
// ram_size; el resto a cero) y el pie con hdrLen.
func memFile(t *testing.T, magia uint64, version uint32, hdrLen uint64) string {
	t.Helper()
	return escribirMem(t, memSpec{magia: magia, version: version, hdrLen: hdrLen})
}

// memSpec describe un mem.file de prueba. ramFichero es la RAM que de verdad
// lleva el fichero (4096 si es 0); ramPie, hdrOff y ramHdr, lo que dicen el
// pie y la cabecera (ramFichero si son 0); gicHdr, el gic_size de la
// cabecera, y gic, los bytes del GIC que van detrás de ella.
type memSpec struct {
	magia                              uint64
	version                            uint32
	hdrLen                             uint64
	ramFichero, ramPie, hdrOff, ramHdr uint64
	gicHdr, gic                        uint64
}

const (
	offRAMHdr = 24              // offsetof(struct snap_hdr, ram_size)
	offGICHdr = hdrV1Fijada - 8 // offsetof(struct snap_hdr, gic_size): justo antes de hay_vsock
)

func escribirMem(t *testing.T, m memSpec) string {
	t.Helper()
	ram := m.ramFichero
	if ram == 0 {
		ram = 4096
	}
	pon := func(v *uint64) {
		if *v == 0 {
			*v = ram
		}
	}
	pon(&m.ramPie)
	pon(&m.hdrOff)
	pon(&m.ramHdr)
	b := make([]byte, ram+m.hdrLen+m.gic+pieFijado)
	h := b[ram:]
	binary.LittleEndian.PutUint64(h[0:], m.magia)
	binary.LittleEndian.PutUint32(h[8:], m.version)
	binary.LittleEndian.PutUint64(h[offRAMHdr:], m.ramHdr)
	if m.hdrLen > offGICHdr {
		binary.LittleEndian.PutUint64(h[offGICHdr:], m.gicHdr)
	}
	p := b[ram+m.hdrLen+m.gic:]
	binary.LittleEndian.PutUint64(p[0:], m.magia)
	binary.LittleEndian.PutUint64(p[8:], m.ramPie)
	binary.LittleEndian.PutUint64(p[16:], m.hdrOff)
	binary.LittleEndian.PutUint64(p[24:], m.hdrLen)
	ruta := filepath.Join(t.TempDir(), "mem.file")
	if err := os.WriteFile(ruta, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return ruta
}

// Un mem.file recortado o que no cuadra se rechaza con un error antes de
// mapear la RAM: con hdr_off por delante de ram_size, mapear_ram mapearía
// páginas tras el final del fichero y el VMM moriría de SIGBUS al tocarlas.
// Tampoco se reserva un gic_size absurdo.
func TestRestaurarMemRecortado(t *testing.T) {
	const roto = "truncated or inconsistent kling-hv memory file"
	v3 := func(m memSpec) memSpec {
		m.magia, m.version, m.hdrLen = magiaFijada, versionFijada, hdrFijada
		return m
	}
	casos := []struct {
		nombre string
		m      memSpec
		quiero string
	}{
		// La RAM que dicen pie y cabecera (1 GiB) no está en el fichero: la
		// cabecera va a los 4 KiB.
		{"RAM tras el final", v3(memSpec{ramPie: 1 << 30, hdrOff: 4096, ramHdr: 1 << 30}), roto},
		{"hdr_off fuera de la RAM", v3(memSpec{ramFichero: 8192, ramPie: 4096, ramHdr: 4096}), roto},
		{"ram_size de la cabecera distinta", v3(memSpec{ramHdr: 1 << 30}), roto},
		{"GIC enorme", v3(memSpec{gicHdr: 1 << 40}), roto},
		{"GIC más largo que el fichero", v3(memSpec{gicHdr: 64, gic: 32}), roto},
		{"GIC de la v1 recortado", memSpec{magia: magiaFijada, version: 1, hdrLen: hdrV1Fijada, gicHdr: 64}, roto},
		{"GIC que cabe", v3(memSpec{gicHdr: 64, gic: 64}), "does not match the configured machine"},
	}
	for _, c := range casos {
		err := restaurarSinVM(escribirMem(t, c.m))
		if err == nil || !strings.Contains(err.Error(), c.quiero) {
			t.Errorf("%s: %v; quería %q", c.nombre, err, c.quiero)
		}
	}
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
		{"versión del futuro", magiaFijada, versionFijada + 1, hdrFijada, "this kling-hv reads 4"},
		{"v1 con la cabecera de la v4", magiaFijada, 1, hdrFijada, "this kling-hv reads 4"},
		{"v2 con la cabecera de la v4", magiaFijada, 2, hdrFijada, "this kling-hv reads 4"},
		{"v3 con la cabecera de la v4", magiaFijada, 3, hdrFijada, "this kling-hv reads 4"},
		{"v4", magiaFijada, versionFijada, hdrFijada, "does not match the configured machine"},
		{"v3", magiaFijada, 3, hdrV3Fijada, "does not match the configured machine"},
		{"v2", magiaFijada, 2, hdrV2Fijada, "does not match the configured machine"},
		{"v1", magiaFijada, 1, hdrV1Fijada, "does not match the configured machine"},
	}
	for _, c := range casos {
		err := restaurarSinVM(memFile(t, c.magia, c.version, c.hdrLen))
		if err == nil || !strings.Contains(err.Error(), c.quiero) {
			t.Errorf("%s: %v; quería %q", c.nombre, err, c.quiero)
		}
	}
}
