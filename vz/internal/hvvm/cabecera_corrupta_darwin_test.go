//go:build darwin && arm64

package hvvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Los errores de khv_restore sobre la cabecera y la disposición (vm.c). Con
// una cabecera que pasa todas las comprobaciones, el siguiente es el de la
// máquina configurada, que aquí no hay (restaurarSinVM).
const (
	errAjeno     = "is not a kling-hv memory file"
	errVersion   = "this kling-hv reads"
	errRecortado = "truncated or inconsistent kling-hv memory file"
	errMaquina   = "does not match the configured machine"
)

// volcadoBueno es un mem.file (de la versión dada) que pasa la validación: 4 KiB de RAM, la
// cabecera, 64 bytes de GIC y el pie. Devuelve sus bytes y dónde empieza
// cada sección.
func volcadoBueno(t testing.TB, version, ncpus uint32, hdrLen uint64) (b []byte, hdr, gic, pie int) {
	t.Helper()
	ruta := escribirMem(t, memSpec{magia: magiaFijada, version: version, ncpus: ncpus, hdrLen: hdrLen, gicHdr: 64, gic: 64})
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	hdr = 4096
	gic = hdr + int(hdrLen)
	pie = gic + 64
	if pie+pieFijado != len(b) {
		t.Fatalf("mem.file of %d bytes, expected %d", len(b), pie+pieFijado)
	}
	if err := restaurarSinVM(ruta); err == nil || !strings.Contains(err.Error(), errMaquina) {
		t.Fatalf("the good file does not pass validation: %v", err)
	}
	return b, hdr, gic, pie
}

// restaurarBytes escribe b en ruta y llama a khv_restore. Si el proceso
// muriera (SIGBUS al mapear lo que no hay, un malloc absurdo), el test entero
// cae: lo que se prueba es que eso no pase nunca.
func restaurarBytes(t testing.TB, ruta string, b []byte) error {
	t.Helper()
	if err := os.WriteFile(ruta, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return restaurarSinVM(ruta)
}

// volcadosBuenos: los de cada versión que se prueban rotos.
var volcadosBuenos = []struct {
	nombre         string
	version, ncpus uint32
	hdrLen         uint64
}{
	{"v5", versionFijada, 10, hdrV5(10)},
	{"v4", v4Fijada, 2, hdrFijada},
	{"v3", 3, 1, hdrV3Fijada},
	{"v1", 1, 1, hdrV1Fijada},
}

func errConocido(err error) bool {
	if err == nil {
		return false
	}
	for _, m := range []string{errAjeno, errVersion, errRecortado, errMaquina} {
		if strings.Contains(err.Error(), m) {
			return true
		}
	}
	return false
}

// Un bit cambiado en cualquier byte de la cabecera, del GIC o del pie: nunca
// rompe el proceso ni pasa sin error. En los campos que khv_restore comprueba
// (la magia, la versión y ram_size de la cabecera, y todo el pie) cualquier
// bit se rechaza antes de mirar la máquina; en el resto (registros, estado de
// los dispositivos) la validación no tiene cómo saberlo, y el error es el de
// la máquina que no casa: no hay suma del estado (docs/backend-hv.md,
// «Estabilidad de kling-hv»).
func TestRestaurarCabeceraBitsCambiados(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "mem.file")
	for _, v := range volcadosBuenos {
		bueno, hdr, gic, pie := volcadoBueno(t, v.version, v.ncpus, v.hdrLen)
		// Campos cuya corrupción se tiene que rechazar en la validación:
		// [desde, hasta) dentro del fichero.
		vigilados := [][2]int{
			{hdr, hdr + 8},                         // magic
			{hdr + 8, hdr + 12},                    // version
			{hdr + offRAMHdr, hdr + offRAMHdr + 8}, // ram_size
			{pie, pie + pieFijado},                 // el pie entero
		}
		vigilado := func(off int) bool {
			for _, r := range vigilados {
				if off >= r[0] && off < r[1] {
					return true
				}
			}
			return false
		}
		b := make([]byte, len(bueno))
		probados := 0
		for off := hdr; off < len(bueno); off++ {
			bits := []uint{uint(off % 8)}
			if vigilado(off) {
				bits = []uint{0, 1, 2, 3, 4, 5, 6, 7}
			}
			for _, bit := range bits {
				copy(b, bueno)
				b[off] ^= 1 << bit
				err := restaurarBytes(t, ruta, b)
				probados++
				if !errConocido(err) {
					t.Fatalf("%s: bit %d of byte %d (header +%d): %v", v.nombre, bit, off, off-hdr, err)
				}
				// La versión 3 con la cabecera de otra versión: hueco
				// conocido, lo vigila TestRestaurarVersionConOtraCabecera.
				if off == hdr+8 && uint32(b[off]) == v4Fijada && v.version != v4Fijada {
					continue
				}
				if vigilado(off) && strings.Contains(err.Error(), errMaquina) {
					t.Errorf("%s: bit %d of byte %d (header +%d) passed validation: %v", v.nombre, bit, off, off-hdr, err)
				}
			}
		}
		// gic_size: un bit que lo haga crecer pide más GIC del que hay.
		for bit := uint(7); bit < 64; bit++ {
			copy(b, bueno)
			o := hdr + offGICHdr + int(bit/8)
			b[o] ^= 1 << (bit % 8)
			if err := restaurarBytes(t, ruta, b); err == nil || !strings.Contains(err.Error(), errRecortado) {
				t.Errorf("%s: gic_size with bit %d set: %v; want %q", v.nombre, bit, err, errRecortado)
			}
		}
		t.Logf("%s: %d corrupted files rejected (header at %d, GIC at %d, footer at %d)", v.nombre, probados, hdr, gic, pie)
	}
}

// Recortado en cada frontera de sección (y un byte antes y después): sin el
// final, o sin un trozo de en medio con el pie intacto, que es lo que deja
// una copia a medias. Nunca rompe el proceso, y se rechaza en la validación,
// antes de mapear la RAM, salvo un trozo quitado de dentro de la cabecera
// (abajo).
func TestRestaurarRecortadoEnCadaFrontera(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "mem.file")
	for _, v := range volcadosBuenos {
		bueno, hdr, gic, pie := volcadoBueno(t, v.version, v.ncpus, v.hdrLen)
		fronteras := []int{0, 1, hdr, gic, pie, len(bueno)}
		// Los campos de la cabecera que se validan también son fronteras.
		fronteras = append(fronteras, hdr+8, hdr+12, hdr+offRAMHdr, hdr+offRAMHdr+8, hdr+offGICHdr, hdr+offGICHdr+8)
		var cortes []int
		for _, f := range fronteras {
			for _, c := range []int{f - 1, f, f + 1} {
				if c >= 0 && c < len(bueno) {
					cortes = append(cortes, c)
				}
			}
		}
		for _, c := range cortes {
			// Sin la cola: el fichero acaba en c.
			err := restaurarBytes(t, ruta, bueno[:c])
			if !errConocido(err) || strings.Contains(err.Error(), errMaquina) {
				t.Errorf("%s: cut at %d of %d: %v", v.nombre, c, len(bueno), err)
			}
			// Sin lo que va de c a la siguiente frontera, con el pie intacto.
			if c >= pie {
				continue
			}
			sig := len(bueno) - pieFijado
			for _, f := range fronteras {
				if f > c && f < sig {
					sig = f
				}
			}
			hueco := append(append([]byte{}, bueno[:c]...), bueno[sig:]...)
			err = restaurarBytes(t, ruta, hueco)
			if !errConocido(err) {
				t.Fatalf("%s: bytes [%d,%d) removed, footer kept: %v", v.nombre, c, sig, err)
			}
			// Quitar bytes de dentro de la cabecera la desplaza sin cambiar
			// los campos que se validan si lo quitado eran ceros (gic_size
			// se lee entonces de los bytes de detrás): sin una suma del
			// estado eso no se puede ver. Lo demás se rechaza.
			if c > hdr && sig <= gic {
				continue
			}
			if strings.Contains(err.Error(), errMaquina) {
				t.Errorf("%s: bytes [%d,%d) removed, footer kept, passed validation: %v", v.nombre, c, sig, err)
			}
		}
	}
}

// Una cabecera con la versión 4 se tiene que leer con el tamaño de la 4: con
// el hdr_len de la 1, la 2 o la 3, khv_restore la trataría como v4 sobre
// bytes que no lo son (con la 2 o la 3, su globo leído con la forma de la v4).
// khv_restore la rechaza por la versión, antes de mirar la máquina.
func TestRestaurarVersionConOtraCabecera(t *testing.T) {
	for _, c := range []struct {
		nombre string
		hdrLen uint64
	}{
		{"v4 con la cabecera de la v1", hdrV1Fijada},
		{"v4 con la cabecera de la v2", hdrV2Fijada},
		{"v4 con la cabecera de la v3", hdrV3Fijada},
	} {
		err := restaurarSinVM(memFile(t, magiaFijada, v4Fijada, c.hdrLen))
		if err == nil || !strings.Contains(err.Error(), errVersion) {
			t.Errorf("%s: want %q, got %v", c.nombre, errVersion, err)
		}
	}
}

// FuzzCabecera: lo que va detrás de la RAM (cabecera, GIC y pie) con bytes
// cualesquiera, a partir de volcados buenos de cada versión (la 5 con 10
// vCPU). khv_restore nunca rompe el proceso ni deja pasar uno que no
// cuadre: siempre uno de sus errores (aquí, sin VM, como mucho el de la
// máquina que no casa).
func FuzzCabecera(f *testing.F) {
	for _, v := range volcadosBuenos {
		b, hdr, _, _ := volcadoBueno(f, v.version, v.ncpus, v.hdrLen)
		f.Add(b[hdr:])
	}
	ram := make([]byte, 4096)
	// Un fichero por proceso: cada trabajador del fuzz es un proceso y dentro
	// de él las llamadas van una tras otra.
	ruta := filepath.Join(f.TempDir(), "mem.file")
	f.Fuzz(func(t *testing.T, cola []byte) {
		if err := restaurarBytes(t, ruta, append(append([]byte{}, ram...), cola...)); !errConocido(err) {
			t.Fatalf("%d bytes after the RAM: %v", len(cola), err)
		}
	})
}
