//go:build darwin

package machine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// fNoCache es F_NOCACHE: las lecturas y escrituras del descriptor no se
// quedan en la caché unificada.
const fNoCache = 48

// fPunchHole es F_PUNCHHOLE, con su struct fpunchhole.
const fPunchHole = 99

type fpunchhole struct {
	flags, reserved uint32
	offset, length  int64
}

// residentes cuenta, con mincore, cuántos bytes de [off, off+n) del fichero
// están en la caché.
func residentes(t *testing.T, path string, off, n int64) int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(m)
	pag := int64(os.Getpagesize())
	vec := make([]byte, n/pag)
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m[off])),
		uintptr(n), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		t.Fatal(e)
	}
	var r int64
	for _, v := range vec {
		if v&1 != 0 {
			r += pag
		}
	}
	return r
}

// En macOS precargar tenía cuerpo vacío: un mem.file fuera de la caché (tras
// presión de memoria o un reinicio) se leía del SSD a golpe de fallo de página
// cuando el invitado despertaba, y el resync del thaw pasaba de 3 a ~80 ms.
// Ahora lo lee entero, y solo los datos: los huecos del perforado (aquí
// SEEK_HOLE y SEEK_DATA van al revés que en Linux) no se tocan.
func TestPrecargarDarwinLeeLosDatosYSaltaLosHuecos(t *testing.T) {
	const mib = 1 << 20
	path := filepath.Join(t.TempDir(), "mem.file")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fNoCache, 1); e != 0 {
		t.Fatal(e)
	}
	// Búfer alineado a página, como pide F_NOCACHE para no pasar por la caché.
	buf, err := syscall.Mmap(-1, 0, mib, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(buf)
	for i := range buf {
		buf[i] = byte(i) | 1
	}
	// 8 MiB escritos y luego perforados como hace el volcado: datos en [0, 1)
	// y [6, 7) MiB, el resto huecos.
	for off := int64(0); off < 8*mib; off += mib {
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range [][2]int64{{mib, 5 * mib}, {7 * mib, mib}} {
		ph := fpunchhole{offset: h[0], length: h[1]}
		if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fPunchHole, uintptr(unsafe.Pointer(&ph))); e != 0 {
			t.Skipf("F_PUNCHHOLE: %v (not APFS?)", e)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if r := residentes(t, path, 0, 8*mib); r != 0 {
		t.Skipf("the file is already cached (%d bytes) after F_NOCACHE writes: can't tell", r)
	}

	if d, err := os.Open(path); err == nil {
		h, _ := d.Seek(0, seekHole)
		d.Close()
		if h != mib {
			t.Skipf("first hole at %d, want %d: the filesystem doesn't report holes", h, mib)
		}
	}
	precargar(path)

	for _, off := range []int64{0, 6 * mib} {
		if r := residentes(t, path, off, mib); r != mib {
			t.Errorf("data at %d MiB: %d of %d bytes cached after precargar", off/mib, r, mib)
		}
	}
	if r := residentes(t, path, mib, 5*mib) + residentes(t, path, 7*mib, mib); r != 0 {
		t.Errorf("holes: %d bytes cached after precargar, want 0 (it read the holes)", r)
	}
}
