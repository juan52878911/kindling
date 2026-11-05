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
	versionFijada = 6                  // la 4 o la 5 con la sección de virtio-fs detrás
	v5Fijada      = 5                  // la 4 con los vCPU de más de 8 detrás
	v4Fijada      = 4
	hdrFijada     = 11776 // la cabecera de la versión 4 (el globo con la cola del informe de libres)
	hdrV3Fijada   = 11744 // la versión 3: el globo con la cola de estadísticas y sin la del informe
	hdrV2Fijada   = 11696 // la versión 2: el globo sin esa cola
	hdrV1Fijada   = 11384 // offsetof(struct snap_hdr, hay_vsock): la versión 1
	pieFijado     = 32
	cpuFijada     = 1296 // sizeof(struct cpu_state): lo que crece la cabecera de la 5 por vCPU de más
	cpusV4Fijado  = 8    // los vCPU que caben en la cabecera de la 4
	vfsCabFijada  = 16   // offsetof(struct snap_vfs, vfs): la cabecera de la sección de virtio-fs de la 6
	vfsFijada     = 184  // sizeof(struct vvfs_state): lo que crece esa sección por dispositivo
)

// hdrV5 es la cabecera de la versión 5 con n vCPU (n > cpusV4Fijado).
func hdrV5(n uint64) uint64 { return hdrFijada + (n-cpusV4Fijado)*cpuFijada }

// hdrV6 es la de la versión 6 con n vCPU y k dispositivos virtio-fs: la de la
// 4 o la 5 según los vCPU, y la sección de virtio-fs.
func hdrV6(n, k uint64) uint64 {
	base := uint64(hdrFijada)
	if n > cpusV4Fijado {
		base = hdrV5(n)
	}
	return base + vfsCabFijada + k*vfsFijada
}

func TestCabeceraKLNGHV(t *testing.T) {
	magia, version, hdr, hdrV1, hdrV2, hdrV3, pie, cpu, cpusV4, vfsCab, vfs := formatoSnap()
	if magia != magiaFijada || version != versionFijada || hdr != hdrFijada || hdrV1 != hdrV1Fijada || hdrV2 != hdrV2Fijada ||
		hdrV3 != hdrV3Fijada || pie != pieFijado || cpu != cpuFijada || cpusV4 != cpusV4Fijado || vfsCab != vfsCabFijada ||
		vfs != vfsFijada {
		t.Fatalf("the kling-hv memory file layout changed: magic %#x version %d header %d (v1 %d, v2 %d, v3 %d) footer %d "+
			"cpu %d (%d in v4) virtio-fs %d+%d; fixed: %#x %d %d (%d, %d, %d) %d %d (%d) %d+%d. If struct snap_hdr changed, "+
			"bump SNAP_VERSION (and read the old one), update internal/machine (magiaHV, versionHVMax) and only then these "+
			"constants",
			magia, version, hdr, hdrV1, hdrV2, hdrV3, pie, cpu, cpusV4, vfsCab, vfs, magiaFijada, versionFijada, hdrFijada,
			hdrV1Fijada, hdrV2Fijada, hdrV3Fijada, pieFijado, cpuFijada, cpusV4Fijado, vfsCabFijada, vfsFijada)
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
	magia          uint64
	version, ncpus uint32
	hdrLen         uint64
	// En la 6, el nvfs de la sección de virtio-fs: el que cuadra con hdrLen
	// (se calcula) salvo que nvfs lo fije (> 0).
	nvfs                               uint32
	ramFichero, ramPie, hdrOff, ramHdr uint64
	gicHdr, gic                        uint64
}

const (
	offCPUsHdr = 12              // offsetof(struct snap_hdr, ncpus)
	offRAMHdr  = 24              // offsetof(struct snap_hdr, ram_size)
	offGICHdr  = hdrV1Fijada - 8 // offsetof(struct snap_hdr, gic_size): justo antes de hay_vsock
)

func escribirMem(t testing.TB, m memSpec) string {
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
	binary.LittleEndian.PutUint32(h[offCPUsHdr:], m.ncpus)
	binary.LittleEndian.PutUint64(h[offRAMHdr:], m.ramHdr)
	if m.hdrLen > offGICHdr {
		binary.LittleEndian.PutUint64(h[offGICHdr:], m.gicHdr)
	}
	if m.version == 6 {
		base := uint64(hdrFijada)
		if m.ncpus > cpusV4Fijado {
			base = hdrV5(uint64(m.ncpus))
		}
		if m.hdrLen >= base+4 {
			n := m.nvfs
			if n == 0 && m.hdrLen >= base+vfsCabFijada {
				n = uint32((m.hdrLen - base - vfsCabFijada) / vfsFijada)
			}
			binary.LittleEndian.PutUint32(h[base:], n)
		}
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
		m.magia, m.version, m.hdrLen = magiaFijada, v4Fijada, hdrFijada
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
	const lee = "this kling-hv reads up to 6"
	casos := []struct {
		nombre  string
		magia   uint64
		version uint32
		ncpus   uint32
		hdrLen  uint64
		quiero  string
	}{
		{"otra magia", 0x4b4c4e4748563031, v4Fijada, 1, hdrFijada, "is not a kling-hv memory file"},
		{"hdr_len de nadie", magiaFijada, v4Fijada, 1, hdrFijada - 8, "is not a kling-hv memory file"},
		{"hdr_len de la v5 sin un vCPU entero", magiaFijada, v5Fijada, 10, hdrV5(10) - 16, "is not a kling-hv memory file"},
		{"hdr_len mayor que el de 64 vCPU", magiaFijada, v5Fijada, 65, hdrV5(65), "is not a kling-hv memory file"},
		{"versión del futuro", magiaFijada, v5Fijada + 1, 1, hdrFijada, lee},
		{"v1 con la cabecera de la v4", magiaFijada, 1, 1, hdrFijada, lee},
		{"v2 con la cabecera de la v4", magiaFijada, 2, 1, hdrFijada, lee},
		{"v3 con la cabecera de la v4", magiaFijada, 3, 1, hdrFijada, lee},
		// La 5 es solo para más de 8 vCPU, y con la cabecera de sus vCPU.
		{"v5 con 8 vCPU", magiaFijada, v5Fijada, 8, hdrFijada, lee},
		{"v5 con la cabecera de otros vCPU", magiaFijada, v5Fijada, 10, hdrV5(11), lee},
		{"v5 con más vCPU de los que caben", magiaFijada, v5Fijada, 65, hdrV5(64), lee},
		{"v4 con 10 vCPU", magiaFijada, v4Fijada, 10, hdrFijada, lee},
		{"v4 con 10 vCPU y la cabecera de la v5", magiaFijada, v4Fijada, 10, hdrV5(10), lee},
		{"v5", magiaFijada, v5Fijada, 10, hdrV5(10), "does not match the configured machine"},
		{"v5 con 64 vCPU", magiaFijada, v5Fijada, 64, hdrV5(64), "does not match the configured machine"},
		{"v4", magiaFijada, v4Fijada, 8, hdrFijada, "does not match the configured machine"},
		{"v3", magiaFijada, 3, 2, hdrV3Fijada, "does not match the configured machine"},
		{"v2", magiaFijada, 2, 1, hdrV2Fijada, "does not match the configured machine"},
		{"v1", magiaFijada, 1, 0, hdrV1Fijada, "does not match the configured machine"},
		// La 6: la cabecera de la 4 (hasta 8 vCPU) o de la 5 (más), y detrás la
		// sección de virtio-fs con sus dispositivos; sola o con los vCPU de más.
		{"v6 con 1 virtio-fs", magiaFijada, versionFijada, 2, hdrV6(2, 1), "does not match the configured machine"},
		{"v6 con 8 virtio-fs", magiaFijada, versionFijada, 2, hdrV6(2, 8), "does not match the configured machine"},
		{"v6 con 10 vCPU y 2 virtio-fs", magiaFijada, versionFijada, 10, hdrV6(10, 2), "does not match the configured machine"},
		{"v6 sin virtio-fs", magiaFijada, versionFijada, 2, hdrFijada + vfsCabFijada, "is not a kling-hv memory file"},
		{"v6 con 9 virtio-fs", magiaFijada, versionFijada, 2, hdrV6(2, 9), "is not a kling-hv memory file"},
		{"v6 sin un dispositivo entero", magiaFijada, versionFijada, 2, hdrV6(2, 2) - 8, "is not a kling-hv memory file"},
		{"v6 con la cabecera de otros vCPU", magiaFijada, versionFijada, 10, hdrV6(9, 2), lee},
		{"v4 con la sección de virtio-fs", magiaFijada, v4Fijada, 2, hdrV6(2, 1), lee},
		{"v5 con la sección de virtio-fs", magiaFijada, v5Fijada, 10, hdrV6(10, 1), lee},
	}
	for _, c := range casos {
		err := restaurarSinVM(escribirMem(t, memSpec{magia: c.magia, version: c.version, ncpus: c.ncpus, hdrLen: c.hdrLen}))
		if err == nil || !strings.Contains(err.Error(), c.quiero) {
			t.Errorf("%s: %v; quería %q", c.nombre, err, c.quiero)
		}
	}
}
