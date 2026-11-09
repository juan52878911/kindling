package fc

// A quién se conecta el daemon (M3).
//
// El cliente vuelve a resolver la ruta del socket en cada petición (sin
// keep-alive, ver New), y con jailer esa ruta, <jaula>/run/firecracker.socket,
// cuelga de run/, que es del VMM. Un Firecracker comprometido podía cambiar su
// socket, o su run/, por un enlace simbólico al de OTRA jaula: el daemon, que
// es root y sigue enlaces, mandaba el siguiente Pause o CreateSnapshot de la
// máquina A al VMM de la B.
//
// Dos barreras, en Linux (dial_linux.go):
//
//   - El último componente se abre sin seguir enlaces (O_PATH|O_NOFOLLOW) y
//     tiene que ser un socket; se conecta a ESE inodo por /proc/self/fd/N.
//   - Tras conectar, SO_PEERCRED da el pid de quien atiende, y su raíz
//     (/proc/<pid>/root) tiene que ser la jaula de la ruta: un enlace en un
//     directorio intermedio lleva a otro VMM, cuya raíz es otra.
//
// La ruta dice si es de una jaula: jailer fija run/firecracker.socket (ver
// jailSock en internal/machine), y un VMM sin jailer escucha en fc.sock. Se
// comparan inodos y no rutas: el VMM vive en su propio espacio de montaje tras
// pivot_root, y desde fuera readlink de su /proc/<pid>/root no dice la ruta
// del anfitrión.

import (
	"fmt"
	"path/filepath"
)

// ComprobarJaula activa las dos barreras de arriba. Solo la apagan los tests
// de otros paquetes que simulan un VMM enjaulado con un servidor sin jaula,
// enlazado en la ruta del socket (ver internal/machine/enjaulado_test.go); en
// el daemon siempre está a true.
var ComprobarJaula = true

// idFichero identifica un fichero por dispositivo e inodo.
type idFichero struct{ dev, ino uint64 }

// esSocketDeJaula dice si path es el socket de un VMM enjaulado:
// <jaula>/run/firecracker.socket.
func esSocketDeJaula(path string) bool {
	return filepath.Base(path) == "firecracker.socket" && filepath.Base(filepath.Dir(path)) == "run"
}

// comprobarPar decide si el proceso que atiende path es el que debe.
//
// raizPar es la raíz del proceso del otro lado; raizHost, la de este; jaula,
// la del directorio <jaula> de la ruta (errJaula si no se pudo mirar). Un
// socket que no es de jaula no se comprueba: sin jailer el VMM ve el disco del
// anfitrión entero y no hay a quién suplantar que no pudiera alcanzar ya.
func comprobarPar(path string, raizPar, raizHost, jaula idFichero, errJaula error) error {
	if !esSocketDeJaula(path) {
		return nil
	}
	if raizPar == raizHost {
		return fmt.Errorf("firecracker socket %s is served by a process that is not jailed: refusing it", path)
	}
	if errJaula != nil {
		return fmt.Errorf("firecracker socket %s: checking its jail: %w", path, errJaula)
	}
	if raizPar != jaula {
		return fmt.Errorf("firecracker socket %s is served by a process jailed somewhere else: refusing it", path)
	}
	return nil
}
