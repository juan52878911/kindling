//go:build darwin

package machine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// fLog2PhysExt es F_LOG2PHYS_EXT de <sys/fcntl.h>: dónde está en el
// dispositivo un desplazamiento del fichero.
const fLog2PhysExt = 65

// posicionFisica devuelve el desplazamiento en el dispositivo del byte off de
// f. Dos ficheros que comparten bloques dan el mismo. struct log2phys va con
// #pragma pack(4): 4 + 8 + 8 bytes.
func posicionFisica(t *testing.T, f *os.File, off int64) int64 {
	t.Helper()
	var b [20]byte
	binary.LittleEndian.PutUint64(b[4:], 4096) // l2p_contigbytes: lo que se consulta
	binary.LittleEndian.PutUint64(b[12:], uint64(off))
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fLog2PhysExt, uintptr(unsafe.Pointer(&b[0])))
	if e != 0 {
		t.Skipf("F_LOG2PHYS_EXT: %v", e)
	}
	return int64(binary.LittleEndian.Uint64(b[12:]))
}

func escribirAleatorio(t *testing.T, ruta string, n int) []byte {
	t.Helper()
	datos := make([]byte, n)
	if _, err := rand.Read(datos); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(ruta, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(datos); err != nil {
		t.Fatal(err)
	}
	// Sin sync los bloques aún no tienen sitio en el disco (asignación
	// diferida) y F_LOG2PHYS no dice nada útil.
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return datos
}

// En APFS (el disco de este Mac) el commit clona el overlay desde el
// descriptor comprobado: el dorado comparte los bloques del overlay.
func TestCopiarOverlayDesdeClonaEnAPFS(t *testing.T) {
	d := t.TempDir()
	if !esAPFS(d) {
		t.Skip("el directorio temporal no está en APFS")
	}
	src := filepath.Join(d, "overlay.ext4")
	datos := escribirAleatorio(t, src, 4<<20)
	in, tras, err := fijarOverlayParaLeer(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	dst := filepath.Join(d, "dorado.ext4")
	out, clonado, err := crearDestinoOverlay(in, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	if !clonado {
		out.Close()
		t.Fatal("no clonó en APFS")
	}
	if posicionFisica(t, in, 0) != posicionFisica(t, out, 0) || posicionFisica(t, in, 3<<20) != posicionFisica(t, out, 3<<20) {
		t.Error("el clon no comparte los bloques del overlay")
	}
	fi, _ := out.Stat()
	out.Close()
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("el clon tiene modo %v, no 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, datos) {
		t.Error("el clon no tiene el contenido del overlay")
	}
	if err := tras(); err != nil {
		t.Fatal(err)
	}

	// Y por copiarOverlayDesde entero, con su cesión por descriptor.
	dst2 := filepath.Join(d, "dorado2.ext4")
	cedido := false
	m := &Manager{}
	fi2, err := m.copiarOverlayDesde(context.Background(), in, dst2, func(*os.File) error { cedido = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ahora, _ := os.Lstat(dst2); !os.SameFile(fi2, ahora) {
		t.Error("la identidad devuelta no es la del dorado")
	}
	if !cedido {
		t.Error("no cedió el clon")
	}
	if b, _ := os.ReadFile(dst2); !bytes.Equal(b, datos) {
		t.Error("el dorado no tiene el contenido del overlay")
	}
}

// Si fclonefileat no puede (ENOTSUP fuera de APFS, EXDEV entre volúmenes),
// copia dispersa, sin dejar nada a medias.
func TestCopiarOverlayDesdeSinClonCopia(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.EXDEV} {
		t.Run(errno.Error(), func(t *testing.T) {
			orig := fclonefileat
			fclonefileat = func(int, int, string, int) error { return errno }
			defer func() { fclonefileat = orig }()
			d := t.TempDir()
			src := filepath.Join(d, "overlay.ext4")
			datos := escribirAleatorio(t, src, 64<<10)
			in, _, err := fijarOverlayParaLeer(src)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			dst := filepath.Join(d, "dorado.ext4")
			out, clonado, err := crearDestinoOverlay(in, dst, false)
			if err != nil {
				t.Fatal(err)
			}
			out.Close()
			if clonado {
				t.Fatal("dice que clonó")
			}
			_ = os.Remove(dst)
			m := &Manager{}
			if _, err := m.copiarOverlayDesde(context.Background(), in, dst, nil); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(dst); !bytes.Equal(b, datos) {
				t.Error("la copia no tiene el contenido del overlay")
			}
			// Una copia no comparte bloques: lo que hace que la comprobación
			// del test de APFS signifique algo.
			if esAPFS(d) {
				f, err := os.OpenFile(dst, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				_ = f.Sync()
				if posicionFisica(t, in, 0) == posicionFisica(t, f, 0) {
					t.Error("una copia da la misma posición física que el original")
				}
			}
		})
	}
}

// Si el nombre deja de ser el clon entre fclonefileat y la apertura (un
// hardlink a otro fichero puesto en su lugar), se rechaza y no se copia.
func TestClonarADestinoRechazaOtroFichero(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "overlay.ext4")
	escribirAleatorio(t, src, 64<<10)
	victima := filepath.Join(d, "victima")
	escribirAleatorio(t, victima, 64<<10)
	in, _, err := fijarOverlayParaLeer(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dst := filepath.Join(d, "dorado.ext4")
	orig := fclonefileat
	fclonefileat = func(_, _ int, _ string, _ int) error {
		// En lugar del clon, un hardlink a la víctima.
		return os.Link(victima, dst)
	}
	defer func() { fclonefileat = orig }()
	m := &Manager{}
	if _, err := m.copiarOverlayDesde(context.Background(), in, dst, nil); err == nil {
		t.Fatal("aceptó un hardlink a otro fichero como clon")
	}
	if _, err := os.Stat(victima); err != nil {
		t.Errorf("la víctima ya no está: %v", err)
	}
}
