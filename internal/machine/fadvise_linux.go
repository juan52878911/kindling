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
