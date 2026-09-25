//go:build linux

package machine

import (
	"os"
	"syscall"
)

// posixFadvDontNeed le dice al kernel que ya no hace falta un fichero en caché.
const posixFadvDontNeed = 4

// dropCache saca un fichero de la caché de página.
//
// POR QUÉ. Congelar una microVM escribe un fichero de memoria del tamaño de su
// RAM, y el perforado de ceros lo vuelve a leer entero. Al terminar, esos ~150 MB
// se quedan en caché aunque nadie los vaya a tocar hasta que alguien descongele
// esa máquina concreta —que puede no pasar nunca—.
//
// Tras varias decenas de ciclos de congelar y descongelar, el invitado acumula
// gigabytes de caché de snapshots muertos. Es memoria reclamable y el kernel la
// suelta bajo presión, así que no rompe nada; pero desde fuera el hipervisor ve
// una VM "llena" y no puede aprovechar esa RAM para otra cosa.
//
// NO se aplica a los snapshots dorados: ahí la caché es justo lo que hace que N
// instancias compartan páginas, y tirarla saldría carísimo.
//
// Es una sugerencia al kernel, no una orden: si falla, no pasa nada.
func dropCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// fadvise64(fd, offset=0, len=0 -> hasta el final, DONTNEED)
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64,
		f.Fd(), 0, 0, posixFadvDontNeed, 0, 0)
}

// precargar trae un fichero a la caché de página leyéndolo entero, en
// segundo plano. Es la otra mitad de dropCache: Freeze saca de la caché el
// mem.file de una máquina congelada, y Thaw lo vuelve a pedir en cuanto sabe
// que lo va a cargar, antes de montar la red y lanzar el VMM. Así la lectura
// del disco corre a la vez que eso, y no a golpe de fallo de página cuando el
// invitado despierta (medido: 55 ms de resync y 12 ms de primera petición que
// se iban en eso; ver docs/despertar.md).
//
// Se lee de verdad y no con POSIX_FADV_WILLNEED: el kernel acota ese
// readahead a su ventana (unos cientos de KiB), y medido no cambió nada.
// SEEK_DATA/SEEK_HOLE saltan los huecos del perforado, que no cuestan E/S.
// Si algo falla, no pasa nada: el invitado leerá lo que le falte al tocarlo.
func precargar(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	size := fi.Size()
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; {
		data, err := f.Seek(off, seekData)
		if err != nil {
			return // ENXIO: no quedan datos
		}
		hole, err := f.Seek(data, seekHole)
		if err != nil {
			hole = size
		}
		for p := data; p < hole; {
			n := int64(len(buf))
			if hole-p < n {
				n = hole - p
			}
			k, err := f.ReadAt(buf[:n], p)
			if k <= 0 || (err != nil && k < int(n)) {
				return
			}
			p += int64(k)
		}
		off = hole
	}
}

// Los whence de lseek para ficheros dispersos (no están en syscall).
const (
	seekData = 3
	seekHole = 4
)
