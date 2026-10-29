package share

// Enlaces simbólicos en una copia: las mismas reglas en el cliente (que se
// salta lo que no pasaría) y en el daemon (que rechaza la subida entera).

import (
	"fmt"
	"path"
	"strings"
)

// CheckLink exige que un enlace de una copia sea relativo, en forma canónica, y
// que, resuelto desde donde está (name, relativo a la raíz de la carpeta), no
// salga del árbol.
//
// Forma canónica: primero los "..", luego componentes que solo bajan
// ("../../a/b", nunca "a/../b"). Un ".." tras un componente que baja se rechaza
// aunque léxicamente se quede dentro, porque ese componente puede ser (o pasar
// a ser después) un enlace, y entonces lo léxico y lo real no coinciden: con
// "x -> a/up/../.." y luego "a/up -> ..", cada uno válido por separado, x sale
// dos niveles. En forma canónica los ".." solo suben por directorios reales
// (los padres del propio enlace) y lo que baja, si cruza otro enlace, cruza uno
// que también cumple esto: por inducción nunca se sale, se cree en el orden
// que se cree.
func CheckLink(name, target string) error {
	if target == "" || len(target) >= MaxPath || strings.ContainsRune(target, 0) {
		return fmt.Errorf("symlink %q has an invalid target", name)
	}
	if strings.HasPrefix(target, "/") {
		return fmt.Errorf("symlink %q points to an absolute path (%s)", name, target)
	}
	ups, down := 0, false
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if down {
				return fmt.Errorf("symlink %q goes back up after going down (%s)", name, target)
			}
			ups++
		default:
			down = true
		}
	}
	depth := 0
	if d := path.Dir(name); d != "." {
		depth = strings.Count(d, "/") + 1
	}
	if ups > depth {
		return fmt.Errorf("symlink %q points outside the shared directory (%s)", name, target)
	}
	return nil
}

// LinkTraverses dice si el destino de name atraviesa, antes de su último
// componente, algo que isLink dice que es un enlace. Si lo hace, lo léxico y lo
// real pueden no coincidir ("x -> a/up/../..", con "a/up -> ..", léxicamente es
// la raíz y de verdad su padre), y CheckLink no dice nada de adónde llega.
func LinkTraverses(name, target string, isLink func(string) bool) bool {
	var stack []string
	if d := path.Dir(name); d != "." {
		stack = strings.Split(d, "/")
	}
	parts := strings.Split(target, "/")
	for i, c := range parts {
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, c)
		if i < len(parts)-1 && isLink(strings.Join(stack, "/")) {
			return true
		}
	}
	return false
}
