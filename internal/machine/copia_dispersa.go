package machine

// Copia dispersa de un disco entre dos descriptores abiertos, sin volver a
// abrir ninguna ruta (ver copiarOverlayDesde). Hace lo que `cp --sparse=always`:
// salta los huecos del origen (SEEK_DATA/SEEK_HOLE) y no escribe los bloques a
// cero, para que el destino quede tan disperso como el origen o más.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
)

// bloqueDisperso es la granularidad con la que se buscan ceros: la del bloque
// de ext4 y de la página, así que un bloque a cero no ocupa en el destino.
const bloqueDisperso = 4096

var cerosDisperso = make([]byte, bloqueDisperso)

// copiarDisperso copia in en out, que tiene que estar vacío. El tamaño se
// fija al empezar: quien copia un overlay lo hace con la máquina pausada.
func copiarDisperso(ctx context.Context, in, out *os.File) error {
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tam := fi.Size()
	// El tamaño lógico primero: escribir más allá del final hace que APFS
	// rellene el hueco intermedio en vez de dejarlo disperso.
	if err := out.Truncate(tam); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for off := int64(0); off < tam; {
		if err := ctx.Err(); err != nil {
			return err
		}
		ini, fin := siguientesDatos(in, off, tam)
		if ini >= tam {
			break
		}
		for p := ini; p < fin; {
			n := min(int64(len(buf)), fin-p)
			leidos, err := in.ReadAt(buf[:n], p)
			if leidos > 0 {
				if werr := escribirSinCeros(out, buf[:leidos], p); werr != nil {
					return werr
				}
				p += int64(leidos)
			}
			if err == io.EOF {
				fin = p // el fichero encogió: lo que falta se queda en hueco
				break
			}
			if err != nil {
				return err
			}
		}
		off = fin
	}
	// El tamaño lógico entero, aunque acabe en hueco.
	return out.Truncate(tam)
}

// siguientesDatos devuelve el tramo [ini, fin) con datos que empieza en off o
// después. Si el sistema de ficheros no sabe de huecos (EINVAL), todo lo que
// queda es datos; si no hay más datos (ENXIO), ini = tam.
func siguientesDatos(f *os.File, off, tam int64) (ini, fin int64) {
	d, err := f.Seek(off, seekData)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return tam, tam
		}
		return off, tam
	}
	if d >= tam {
		return tam, tam
	}
	// Alineado al bloque: los ceros se buscan por bloques, y una escritura que
	// no empieza en uno hace que APFS rellene lo que hay antes.
	d &^= bloqueDisperso - 1
	h, err := f.Seek(d, seekHole)
	if err != nil || h > tam || h <= d {
		h = tam
	}
	return d, h
}

// escribirSinCeros escribe b en off saltando los bloques enteros a cero.
func escribirSinCeros(out *os.File, b []byte, off int64) error {
	ini := -1 // comienzo del tramo sin ceros pendiente de escribir
	for i := 0; i < len(b); i += bloqueDisperso {
		fin := min(i+bloqueDisperso, len(b))
		cero := bytes.Equal(b[i:fin], cerosDisperso[:fin-i])
		switch {
		case !cero && ini < 0:
			ini = i
		case cero && ini >= 0:
			if _, err := out.WriteAt(b[ini:i], off+int64(ini)); err != nil {
				return err
			}
			ini = -1
		}
	}
	if ini >= 0 {
		if _, err := out.WriteAt(b[ini:], off+int64(ini)); err != nil {
			return err
		}
	}
	return nil
}
