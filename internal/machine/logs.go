package machine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// logMaxBytes es cuánto lee Logs() de firecracker.log como mucho, desde el
// final. rotarConsola (consola.go) mantiene el fichero por debajo de
// consolaMaxBytes (16 MiB), pero entre una rotación y la siguiente puede
// llegar hasta ahí: sin este segundo tope, `kling logs` de una consola ruidosa
// cargaría esos 16 MiB enteros en la memoria del daemon en cada llamada.
const logMaxBytes = 4 << 20

// logMaxLines es el tope de líneas que Logs() devuelve, con independencia de lo
// que se pida en tail: la consola es la escritura de un invitado, no del
// daemon, y M-07 es precisamente que un invitado hostil pueda hacerla crecer
// sin límite.
const logMaxLines = 10000

// Logs devuelve la consola serie de una microVM.
//
// Es la única ventana al interior mientras no haya red ni agente dentro: si una
// herramienta no arranca, la razón está aquí.
//
// tail<=0 o mayor que logMaxLines se trata como "todo lo que entra en el tope":
// nunca devuelve más de logMaxLines líneas ni lee más de logMaxBytes del
// fichero.
func (m *Manager) Logs(ref string, tail int) (string, error) {
	out, _, err := m.LogsCola(ref, tail)
	return out, err
}

// LogCursor es dónde se quedó quien lee la consola: los bytes de
// firecracker.log ya leídos y la rotación en la que se leyeron. rotarConsola
// trunca el fichero en sitio, así que un desplazamiento solo vale dentro de su
// rotación: Rotacion cambia cada vez que se rota (es la fecha de
// firecracker.log.1, que se reescribe en cada rotación).
type LogCursor struct {
	Offset   int64
	Rotacion string
}

// LogsCola es Logs y además el cursor del final de lo leído, para seguir con
// LogsDesde sin volver a leer la cola entera.
func (m *Manager) LogsCola(ref string, tail int) (string, LogCursor, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return "", LogCursor{}, marcado(ErrNoMachine, "machine %q does not exist", ref)
	}
	var b []byte
	cur, err := conRotacion(m.dir(mc.ID), func() (int64, error) {
		var fin int64
		var err error
		b, fin, err = leerConsolaPos(m.dir(mc.ID), logMaxBytes)
		return fin, err
	})
	if err != nil {
		return "", LogCursor{}, fmt.Errorf("no log for %s: %w", mc.Name, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > logMaxLines {
		lines = lines[len(lines)-logMaxLines:]
	}
	if tail > 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), cur, nil
}

// LogsDesde devuelve los bytes de la consola escritos después de desde, tal
// cual (sin partir en líneas), y el cursor nuevo. Es lo que sondea `kling logs
// -f`: sin esto releía hasta logMaxBytes cada medio segundo y adivinaba dónde
// se había quedado comparando líneas, lo que perdía las repetidas.
//
// Si la consola rotó desde entonces (otra Rotacion, o un fichero más corto que
// desde), empieza por el principio del fichero nuevo. Si hay más de
// logMaxBytes nuevos, devuelve solo los últimos logMaxBytes.
func (m *Manager) LogsDesde(ref string, desde LogCursor) ([]byte, LogCursor, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, LogCursor{}, marcado(ErrNoMachine, "machine %q does not exist", ref)
	}
	dir := m.dir(mc.ID)
	var b []byte
	cur, err := conRotacion(dir, func() (int64, error) {
		ini := desde.Offset
		if rot, err := rotacionConsola(dir); err != nil || rot != desde.Rotacion {
			ini = 0
		}
		var fin int64
		var err error
		b, fin, err = leerDesde(filepath.Join(dir, "firecracker.log"), ini, logMaxBytes)
		return fin, err
	})
	if err != nil {
		return nil, LogCursor{}, fmt.Errorf("no log for %s: %w", mc.Name, err)
	}
	return b, cur, nil
}

// conRotacion lee con leer y devuelve el cursor de lo leído. Si la consola rota
// mientras se lee, el desplazamiento no se sabría de qué fichero es: se vuelve
// a leer (rotar es cada consolaMaxBytes, dos seguidas no pasan).
func conRotacion(dir string, leer func() (int64, error)) (LogCursor, error) {
	for intento := 0; ; intento++ {
		antes, err := rotacionConsola(dir)
		if err != nil {
			return LogCursor{}, err
		}
		fin, err := leer()
		if err != nil {
			return LogCursor{}, err
		}
		despues, err := rotacionConsola(dir)
		if err != nil {
			return LogCursor{}, err
		}
		if antes == despues || intento == 2 {
			return LogCursor{Offset: fin, Rotacion: despues}, nil
		}
	}
}

// rotacionConsola identifica la rotación actual de la consola de dir: la fecha
// de firecracker.log.1, o "0" si aún no se ha rotado nunca.
func rotacionConsola(dir string) (string, error) {
	fi, err := os.Lstat(filepath.Join(dir, "firecracker.log.1"))
	if errors.Is(err, os.ErrNotExist) {
		return "0", nil
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10), nil
}

// leerDesde lee path desde el byte ini hasta el final, como mucho max bytes
// (los últimos), y devuelve dónde acabó. Un ini más allá del final (el fichero
// se truncó) empieza en 0.
func leerDesde(path string, ini, max int64) ([]byte, int64, error) {
	f, err := abrirConsolaExistente(path, os.O_RDONLY)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if ini > fi.Size() || ini < 0 {
		ini = 0
	}
	if fi.Size()-ini > max {
		ini = fi.Size() - max
	}
	if _, err := f.Seek(ini, io.SeekStart); err != nil {
		return nil, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, 0, err
	}
	return b, ini + int64(len(b)), nil
}

// leerConsolaPos lee como mucho max bytes del final de la consola de dir,
// contando lo que rotarConsola apartó en firecracker.log.1, y dice hasta qué
// byte de firecracker.log leyó (0 si no existe).
//
// Sin el .1, justo tras una rotación firecracker.log está vacío (se trunca en
// sitio) y `kling logs` no enseñaba nada: el MiB que la rotación conserva para
// diagnosticar lo que acaba de pasar no lo leía nadie. Lo de antes va delante,
// como en el fichero original.
func leerConsolaPos(dir string, max int64) ([]byte, int64, error) {
	var fin int64
	actual, err := func() ([]byte, error) {
		f, err := abrirConsolaExistente(filepath.Join(dir, "firecracker.log"), os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := leerColaDe(f, max)
		if err == nil {
			// leerColaDe acaba con un ReadAll: la posición es el final leído.
			fin, err = f.Seek(0, io.SeekCurrent)
		}
		return b, err
	}()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	sinActual := err != nil
	if resto := max - int64(len(actual)); resto > 0 {
		previo, perr := leerCola(filepath.Join(dir, "firecracker.log.1"), resto)
		if perr == nil && len(previo) > 0 {
			if len(actual) > 0 && previo[len(previo)-1] != '\n' {
				previo = append(previo, '\n')
			}
			return append(previo, actual...), fin, nil
		}
	}
	if sinActual {
		return nil, 0, err
	}
	return actual, fin, nil
}
