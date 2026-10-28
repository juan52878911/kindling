//go:build darwin

package machine

// En macOS el daemon habla con kling-vz (Virtualization.framework) o con
// kling-hv (Hypervisor.framework, docs/backend-hv.md). Los dos hablan el mismo
// API y lo que el núcleo hace alrededor (red, reenvíos, sandbox, /proc que no
// existe) es lo mismo: lo que cambia es cuánto cuesta restaurar.
//
// kling-hv mapea el fichero de memoria del snapshot (MAP_PRIVATE) en vez de
// copiarlo: una restauración ocupa lo que el invitado escribe (~4–20 MiB
// medidos), no su RAM entera, y las copias de un dorado comparten las páginas
// limpias. Por eso, con hv:
//
//   - restaurarComparteMemoria: las copias de un dorado son MemShared, y la
//     reserva de memoria de la segunda en adelante es la fracción divergente
//     (reserveMemory, shareReserveDiv), como en Firecracker;
//   - la compuerta de restauraciones deja pasar más a la vez
//     (restoresSimultaneosHV): una restauración ya no carga cientos de MiB de
//     golpe, que era lo que llevaba a jetsam con vz.
//
// El congelado diferencial (diff_volcado.go) sigue siendo solo de Firecracker:
// el fichero de memoria de kling-hv ya es un clon APFS del de su base más las
// páginas sucias, así que no ocupa disco de más.

import "fmt"

// restoresSimultaneosHV es cuántas restauraciones cargan a la vez con
// kling-hv. Cada una cuesta unos ms y pocos MiB; el límite es la CPU del
// arranque de cada proceso (el servidor, la red de gVisor).
const restoresSimultaneosHV = 8

// BackendSoportado dice si este binario de kling sabe hablar con ese backend.
func BackendSoportado(b string) bool { return b == BackendVZ || b == BackendHV }

// ElegirBackend fija el backend del daemon antes de crear el Manager.
func ElegirBackend(b string) error {
	switch b {
	case BackendVZ:
		backendVMM, restaurarComparteMemoria = BackendVZ, false
	case BackendHV:
		backendVMM, restaurarComparteMemoria = BackendHV, true
	default:
		return fmt.Errorf("this kling binary was built for macOS (vz or hv), not %q", b)
	}
	return nil
}
