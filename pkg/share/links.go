package share

// Enlaces simbólicos en una copia: las mismas reglas en el cliente (que se
// salta lo que no pasaría) y en el daemon (que rechaza la subida entera).

import (
	"fmt"
	"path"
	"strings"
)

// CheckLink exige que un enlace de una copia sea relativo y que, resuelto desde
// donde está (name, relativo a la raíz de la carpeta), no salga del árbol. Es una
// comprobación léxica: vale si ningún componente intermedio del destino es a su
// vez un enlace (ver LinkTraverses).
func CheckLink(name, target string) error {
	if target == "" || len(target) >= MaxPath || strings.ContainsRune(target, 0) {
		return fmt.Errorf("symlink %q has an invalid target", name)
	}
	if strings.HasPrefix(target, "/") {
		return fmt.Errorf("symlink %q points to an absolute path (%s)", name, target)
	}
	if r := path.Clean(path.Join(path.Dir(name), target)); r == ".." || strings.HasPrefix(r, "../") {
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
