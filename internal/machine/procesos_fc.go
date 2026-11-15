//go:build !darwin

package machine

// Cómo se reconoce, en Linux, qué VMM vivo es de qué máquina: leyendo /proc.
// La versión de macOS, sin /proc, está en procesos_vz.go.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/juan52878911/kindling/pkg/api"
)

// vmmVivoDe es el VMM vivo de id (0 si no hay). Antes escaneaba /proc entero
// en cada thaw: un ReadDir y un cmdline por proceso del host, con el cerrojo
// de la máquina tomado, y en una ráfaga de thaws eso por cada uno. Ahora,
// como en macOS (procesos_vz.go): primero los PID que ya se conocen (pistas:
// el apuntado en el estado, el que dejó vivo su freeze); si ninguno es su VMM
// y en sus sockets (el de su directorio y el del chroot del jailer) no
// escucha nadie, no hay VMM suyo vivo: todo VMM nuestro escucha en el suyo
// desde que arranca hasta que sale. Si algo contesta, o no se sabe, se
// escanea /proc como antes.
func (m *Manager) vmmVivoDe(id string, pistas ...int) int {
	prefix := filepath.Join(m.root, "machines") + "/"
	for _, pid := range pistas {
		if pid <= 0 {
			continue
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err == nil && m.vmmDeArgs(strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), prefix) == id {
			return pid
		}
	}
	if nadieEscuchaEn(m.dir(id)+"/fc.sock") && nadieEscuchaEn(m.jailSock(id)) {
		return 0
	}
	return m.liveVMs()[id]
}

// escaneosProc cuenta los escaneos de /proc de liveVMs (para las pruebas).
var escaneosProc atomic.Int64

// liveVMs escanea /proc y devuelve qué microVM posee cada firecracker vivo.
//
// Es adopt() del revés: en vez de "¿sigue vivo el PID que apunté?", pregunta
// "¿qué está corriendo ahí fuera?". Esa vuelta es lo que permite reconciliar sin
// creerse el fichero de estado.
func (m *Manager) liveVMs() map[string]int {
	escaneosProc.Add(1)
	out := map[string]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out // no es Linux, o /proc no está montado
	}
	prefix := filepath.Join(m.root, "machines") + "/"

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // no es un proceso
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue // murió mientras mirábamos, o no es nuestro
		}
		if id := m.vmmDeArgs(strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), prefix); id != "" {
			out[id] = pid
		}
	}
	return out
}

// vmmDeArgs es la máquina cuyo VMM es el proceso de línea de órdenes args, o
// "" si no es un VMM nuestro. prefix es machines/ bajo la raíz.
func (m *Manager) vmmDeArgs(args []string, prefix string) string {
	for i, arg := range args {
		// Camino normal: el socket lleva el prefijo de machines/, y es el
		// argumento de --api-sock (ver vmmDeLinea: un curl --unix-socket
		// al mismo socket no es un VMM).
		if rest, ok := strings.CutPrefix(arg, prefix); ok && i > 0 && args[i-1] == argSocketVMM {
			if id, ok := strings.CutSuffix(rest, "/fc.sock"); ok && id != "" && !strings.Contains(id, "/") {
				return id
			}
			continue
		}
		// Camino con jail: jailer lleva "--id <id>" y el socket es relativo
		// al chroot, sin el prefijo. Se reconoce por el argumento --id.
		if arg == "--id" && i+1 < len(args) {
			if id := args[i+1]; id != "" && !strings.Contains(id, "/") {
				if _, err := os.Stat(m.jailSock(id)); err == nil {
					return id
				}
			}
		}
	}
	return ""
}

// adopt comprueba si el proceso de una microVM sigue vivo y es realmente suyo.
//
// No basta con mirar si el PID existe: los PID se reciclan. Se verifica que la
// línea de comandos del proceso menciona el socket de ESTA máquina.
func (m *Manager) adopt(mc *api.Machine) (string, bool) {
	if mc.PID <= 0 {
		return "", false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(mc.PID) + "/cmdline")
	if err != nil {
		return "", false
	}
	// Una instancia jailed: tras el exec de jailer, el proceso ES firecracker con
	// "--id <id>" en su línea —la palabra "jailer" desaparece con el exec, así
	// que buscarla marcaría la máquina como muerta y el sweep la mataría en
	// bucle—. Su socket vive dentro del chroot.
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	for i, a := range args {
		if a == "--id" && i+1 < len(args) && args[i+1] == mc.ID {
			sock := m.jailSock(mc.ID)
			if _, err := os.Stat(sock); err != nil {
				return "", false
			}
			return sock, true
		}
	}
	sock := m.dir(mc.ID) + "/fc.sock"
	suyo := false
	for i := 1; i < len(args); i++ {
		if args[i] == sock && args[i-1] == argSocketVMM {
			suyo = true
			break
		}
	}
	if !suyo {
		return "", false
	}
	if _, err := os.Stat(sock); err != nil {
		return "", false
	}
	return sock, true
}

// socketDe es el socket de la API de un VMM que sigue vivo tras reiniciar el
// daemon: el del chroot si corre dentro de jailer, el de su directorio si no.
//
// Readoptar SIEMPRE con el del directorio dejaba a las máquinas jailed con un
// socket que no existe: seguían corriendo, pero congelarlas o pararlas fallaba
// con "dial unix .../fc.sock: no such file or directory" hasta destruirlas.
// Solo lee /proc: vale bajo m.mu.
func (m *Manager) socketDe(id string, pid int) string {
	if sock, ok := m.adopt(&api.Machine{ID: id, PID: pid}); ok {
		return sock
	}
	return m.dir(id) + "/fc.sock"
}

// VMMsVivosDe solo tiene sentido en macOS, donde hay dos VMM entre los que elegir.
func VMMsVivosDe(root, nombre string) int { return 0 }
