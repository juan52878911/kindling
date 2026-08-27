package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cgroupBase es un árbol propio, FUERA del cgroup del servicio.
//
// La tentación es colgar las microVMs del cgroup de kling.service, pero cgroup v2
// prohíbe que un cgroup tenga procesos propios y controladores delegados a la vez.
// Al habilitarlos ahí, systemd deja de poder colocar su proceso principal y el
// servicio falla con 219/CGROUP en el siguiente arranque.
//
// Con un árbol aparte no hay conflicto: systemd gestiona el suyo y nosotros el
// nuestro.
const cgroupBase = "/sys/fs/cgroup/kindling"

// ensureDelegation prepara el árbol de cgroups para las microVMs.
//
// Devuelve la ruta raíz, o un error explicando por qué no habrá límite de CPU.
// No tener límite es peor que tenerlo, pero no lo bastante como para no arrancar.
func ensureDelegation() (string, error) {
	root := "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return "", fmt.Errorf("no cgroup v2 mounted")
	}
	// El controlador cpu tiene que estar disponible para los hijos de la raíz.
	//
	// Se compara PALABRA a palabra y no con Contains: la lista de controladores
	// incluye `cpuset`, que contiene "cpu" como subcadena. En un anfitrion donde
	// cpuset este delegado y cpu no, Contains daba un falso positivo, se saltaba
	// el `+cpu`, y a partir de ahi ninguna microVM tenia techo de CPU.
	if !controladorPresente(readFile(filepath.Join(root, "cgroup.subtree_control")), "cpu") {
		if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"),
			[]byte("+cpu"), 0o644); err != nil {
			return "", fmt.Errorf("cpu controller is not available at the root: %w", err)
		}
	}
	if err := os.MkdirAll(cgroupBase, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", cgroupBase, err)
	}
	// Nuestro árbol nunca tiene procesos propios, solo hijos: puede delegar.
	if err := os.WriteFile(filepath.Join(cgroupBase, "cgroup.subtree_control"),
		[]byte("+cpu"), 0o644); err != nil {
		return "", fmt.Errorf("enabling cpu on %s: %w", cgroupBase, err)
	}
	return cgroupBase, nil
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// limitCPU mete el proceso de una microVM en su propio cgroup con techo de CPU.
//
// Firecracker acota la RAM del invitado y el caudal de E/S, pero nada impide que
// consuma su vCPU al 100% indefinidamente. Sin esto, una herramienta con un bucle
// infinito degrada a todas las vecinas del host.
func (m *Manager) limitCPU(id string, pid int, quotaPct int) string {
	if m.cgroupRoot == "" {
		return "" // ya se avisó al arrancar; no repetirlo en cada máquina
	}
	dir := filepath.Join(m.cgroupRoot, "kl-"+id[:8])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("could not create cgroup: %v", err)
	}
	// cpu.max = "<cuota> <periodo>" en microsegundos; 100000 = un core completo.
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"),
		[]byte(fmt.Sprintf("%d 100000", quotaPct*1000)), 0o644); err != nil {
		return fmt.Sprintf("could not set cpu.max: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"),
		[]byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Sprintf("could not move process to cgroup: %v", err)
	}
	return ""
}

// releaseCPU borra el cgroup de una microVM que ya no corre.
//
// Un cgroup no se puede borrar mientras conserve procesos, y entre el SIGKILL y
// la salida real del proceso pasa un instante. Se reintenta brevemente en vez de
// asumir que el kernel va a nuestro ritmo; lo que sobreviva a esto lo recoge el
// barrido periódico.
func (m *Manager) releaseCPU(id string) {
	if m.cgroupRoot == "" {
		return
	}
	dir := filepath.Join(m.cgroupRoot, "kl-"+id[:8])
	for i := 0; i < 20; i++ {
		if err := os.Remove(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// sweepCgroups borra los cgroups que no corresponden a ninguna máquina viva.
func (m *Manager) sweepCgroups(live map[string]bool) {
	if m.cgroupRoot == "" {
		return
	}
	entries, err := os.ReadDir(m.cgroupRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "kl-") && !live[e.Name()] {
			_ = os.Remove(filepath.Join(m.cgroupRoot, e.Name()))
		}
	}
}

// controladorPresente busca un controlador EXACTO en la lista de cgroup v2, que
// viene separada por espacios ("cpuset cpu io memory pids").
func controladorPresente(lista, quiero string) bool {
	for _, c := range strings.Fields(lista) {
		if c == quiero {
			return true
		}
	}
	return false
}
