package machine

import (
	"context"
	"os"
	"sync/atomic"
)

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
//
// Lee de trozo en trozo (precargaTrozo) y mira ctx entre uno y otro: un thaw
// que aborta deja de leer en el acto, en vez de seguir trayendo hasta
// precargaMaxBytes de un volcado que ya nadie va a cargar. Devuelve cuántos
// bytes leyó.
func precargar(ctx context.Context, path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	size := fi.Size()
	buf := make([]byte, precargaTrozo)
	var leidos int64
	for off := int64(0); off < size; {
		data, err := f.Seek(off, seekData)
		if err != nil {
			return leidos // ENXIO: no quedan datos
		}
		hole, err := f.Seek(data, seekHole)
		if err != nil {
			hole = size
		}
		for p := data; p < hole; {
			if ctx.Err() != nil {
				return leidos
			}
			n := int64(len(buf))
			if hole-p < n {
				n = hole - p
			}
			k, err := f.ReadAt(buf[:n], p)
			if k <= 0 || (err != nil && k < int(n)) {
				return leidos + int64(max(k, 0))
			}
			p += int64(k)
			leidos += int64(k)
		}
		off = hole
	}
	return leidos
}

// precargaTrozo es lo que precargar lee de una vez.
const precargaTrozo = 1 << 20

// precargasMax es cuántas precargas de thaw corren a la vez en el daemon.
// Antes cada thaw lanzaba la suya sin tope: una tormenta de thaws (el gateway
// despertando a la vez lo que congeló) eran decenas de lecturas secuenciales
// de hasta precargaMaxBytes cada una peleándose por el mismo disco, y ninguna
// terminaba antes de que su invitado despertara, que es para lo que sirven.
// Con el tope, las que no caben no se lanzan: el invitado de esas lee lo que
// toque al tocarlo, como sin precarga, y las que corren acaban a tiempo.
const precargasMax = 4

// precargasEnCurso cuenta las precargas de thaw que corren (precargarEnFondo).
var precargasEnCurso atomic.Int32

// precargarEnFondo lanza precargar de path en segundo plano si hay turno
// (precargasMax), y dice si lo lanzó. La precarga para cuando ctx se cancela:
// quien la lanza lo cancela si el thaw aborta.
func precargarEnFondo(ctx context.Context, path string) bool {
	if precargasEnCurso.Add(1) > precargasMax {
		precargasEnCurso.Add(-1)
		return false
	}
	go func() {
		defer precargasEnCurso.Add(-1)
		precargar(ctx, path)
	}()
	return true
}
