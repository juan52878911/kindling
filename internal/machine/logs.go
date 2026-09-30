package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	mc, ok := m.Get(ref)
	if !ok {
		return "", fmt.Errorf("machine %q does not exist", ref)
	}
	b, err := leerConsola(m.dir(mc.ID), logMaxBytes)
	if err != nil {
		return "", fmt.Errorf("no log for %s: %w", mc.Name, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > logMaxLines {
		lines = lines[len(lines)-logMaxLines:]
	}
	if tail > 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// leerConsola lee como mucho max bytes del final de la consola de dir,
// contando lo que rotarConsola apartó en firecracker.log.1.
//
// Sin el .1, justo tras una rotación firecracker.log está vacío (se trunca en
// sitio) y `kling logs` no enseñaba nada: el MiB que la rotación conserva para
// diagnosticar lo que acaba de pasar no lo leía nadie. Lo de antes va delante,
// como en el fichero original.
func leerConsola(dir string, max int64) ([]byte, error) {
	actual, err := leerCola(filepath.Join(dir, "firecracker.log"), max)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sinActual := err != nil
	if resto := max - int64(len(actual)); resto > 0 {
		previo, perr := leerCola(filepath.Join(dir, "firecracker.log.1"), resto)
		if perr == nil && len(previo) > 0 {
			if len(actual) > 0 && previo[len(previo)-1] != '\n' {
				previo = append(previo, '\n')
			}
			return append(previo, actual...), nil
		}
	}
	if sinActual {
		return nil, err
	}
	return actual, nil
}
