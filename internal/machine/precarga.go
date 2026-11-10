package machine

import "os"

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
//
// En macOS igual: kling-hv mapea el mem.file con MAP_PRIVATE y la RAM del
// invitado entra al primer toque, desde la caché unificada o, si el fichero
// salió de ella (presión de memoria, un reinicio), desde el SSD. Medido en un
// M4 con alpine de 256 MiB (52 MiB asignados) y el fichero fuera de la caché:
// resync de 80 ms sin precarga frente a 2,3 ms con ella (medianas de 7).
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
