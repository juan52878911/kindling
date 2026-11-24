//go:build linux

package machine

import (
	"fmt"
	"os"
	"syscall"
)

// anclajeLinux es un mapeo compartido de solo lectura del mem.file con
// mlock sobre sus tramos con datos. MAP_SHARED de un fichero abierto solo
// para leer: el daemon no puede escribir en él, y las páginas que ancla son
// las de la caché de página, las mismas que mapean (MAP_PRIVATE, sin
// escribir) las réplicas restauradas de ese dorado.
type anclajeLinux struct {
	data     []byte
	anclados int64
}

func (a *anclajeLinux) bytes() int64 { return a.anclados }

func (a *anclajeLinux) soltar() {
	// munmap suelta también el mlock.
	_ = syscall.Munmap(a.data)
}

// anclarFicheroPlataforma es el anclado de Linux.
var anclarFicheroPlataforma = anclarFicheroLinux

// anclarFicheroLinux ancla los tramos con datos de path. Los huecos del
// perforado no: anclarlos llenaría la caché de páginas a cero, MemMiB enteros
// en vez de lo asignado. mlock lee del disco lo que falte, así que esto
// también precalienta.
func anclarFicheroLinux(path string) (anclaje, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	tam := fi.Size()
	if tam == 0 || int64(int(tam)) != tam {
		return nil, fmt.Errorf("can't map a file of %d bytes", tam)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(tam), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	a := &anclajeLinux{data: data}
	pag := int64(os.Getpagesize())
	for off := int64(0); off < tam; {
		ini, fin := siguientesDatos(f, off, tam)
		if ini >= tam {
			break
		}
		ini &^= pag - 1
		if err := syscall.Mlock(data[ini:fin]); err != nil {
			a.soltar()
			return nil, fmt.Errorf("mlock: %w", err)
		}
		a.anclados += fin - ini
		off = fin
	}
	return a, nil
}
