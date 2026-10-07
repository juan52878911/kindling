package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
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
	// Memoria y procesos, si se puede: sin ellos queda el techo de CPU, que
	// es lo que no se puede perder. Quién los tiene lo dice después
	// controladoresDelegados, leyendo lo que de verdad quedó.
	for _, c := range []string{"memory", "pids"} {
		if !controladorPresente(readFile(filepath.Join(root, "cgroup.subtree_control")), c) {
			_ = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+"+c), 0o644)
		}
		_ = os.WriteFile(filepath.Join(cgroupBase, "cgroup.subtree_control"), []byte("+"+c), 0o644)
	}
	return cgroupBase, nil
}

// controladoresDelegados dice si los cgroups de las microVMs, hijos de root,
// tendrán los controladores de memoria y de procesos.
func controladoresDelegados(root string) (memoria, procesos bool) {
	lista := readFile(filepath.Join(root, "cgroup.subtree_control"))
	return controladorPresente(lista, "memory"), controladorPresente(lista, "pids")
}

// margenMemoriaVMM es lo que el cgroup de un VMM admite por encima de la RAM
// del invitado: el propio Firecracker (su montón, las colas virtio, los
// búferes de red y disco), lo que el kernel le cobra por KVM (tablas de
// páginas del invitado, estructuras de cada vCPU) y su caché de página. La
// caché limpia se reclama sola al tocar el techo; lo demás no. Medido en el
// laboratorio (SECURITY.md, apartado 4): con un invitado de 512 MiB que llena
// su RAM, el cgroup se queda en ~500 MiB; recién arrancado, la RAM que tocó
// más ~15 MiB.
func margenMemoriaVMM(memMiB int) int {
	return 64 + memMiB/16
}

// memoriaCgroup es la RAM máxima del invitado de mc, la que acota su cgroup:
// el techo si arrancó con globo para crecer (MemMaxMiB), su memoria si no.
func memoriaCgroup(mc *api.Machine) int {
	return max(mc.MemMiB, mc.MemMaxMiB)
}

// pidsMaxVMM es el techo de procesos e hilos del cgroup de un VMM: un hilo
// por vCPU (32 como mucho en Firecracker), el de la API, el principal y los
// de E/S. Firecracker no crea procesos (su seccomp no le deja), así que esto
// solo frena a uno comprometido que lo intentara.
const pidsMaxVMM = 128

// escribirLimitesMemoria fija memory.max y pids.max en el cgroup dir, según
// estén delegados. memMiB es la RAM máxima del invitado (MemMaxMiB si tiene
// techo, MemMiB si no); 0 no pone límite de memoria.
func (m *Manager) escribirLimitesMemoria(dir string, memMiB int) error {
	if m.cgroupMemoria && memMiB > 0 {
		lim := int64(memMiB+margenMemoriaVMM(memMiB)) << 20
		if err := os.WriteFile(filepath.Join(dir, "memory.max"),
			[]byte(strconv.FormatInt(lim, 10)), 0o644); err != nil {
			return fmt.Errorf("could not set memory.max: %w", err)
		}
	}
	if m.cgroupProcesos {
		if err := os.WriteFile(filepath.Join(dir, "pids.max"),
			[]byte(strconv.Itoa(pidsMaxVMM)), 0o644); err != nil {
			return fmt.Errorf("could not set pids.max: %w", err)
		}
	}
	return nil
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// limitCPU mete el proceso de una microVM en su propio cgroup con techo de CPU
// y, si están delegados, de memoria y de procesos (memMiB es la RAM máxima
// del invitado).
//
// Firecracker acota la RAM del invitado y el caudal de E/S, pero nada impide que
// consuma su vCPU al 100% indefinidamente. Sin esto, una herramienta con un bucle
// infinito degrada a todas las vecinas del host. Y un VMM comprometido no
// tiene, sin el cgroup, nada que le impida reservar memoria del host fuera de
// la del invitado: memory.max es lo que hace cierta la "RAM fija".
func (m *Manager) limitCPU(id string, pid int, quotaPct, memMiB int) string {
	if m.cgroupRoot == "" {
		return "" // ya se avisó al arrancar; no repetirlo en cada máquina
	}
	dir, warn := m.crearCgroup(id, quotaPct, memMiB)
	if warn != "" {
		return warn
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"),
		[]byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Sprintf("could not move process to cgroup: %v", err)
	}
	return ""
}

// crearCgroup crea el cgroup de una microVM con sus techos (CPU, y memoria y
// procesos si están delegados), sin meter ningún proceso. Devuelve su
// directorio, o el aviso si no se pudo.
func (m *Manager) crearCgroup(id string, quotaPct, memMiB int) (string, string) {
	dir := m.dirCgroup(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Sprintf("could not create cgroup: %v", err)
	}
	if err := m.escribirCPUMax(dir, quotaPct); err != nil {
		return "", fmt.Sprintf("could not set cpu.max: %v", err)
	}
	if err := m.escribirLimitesMemoria(dir, memMiB); err != nil {
		return "", err.Error()
	}
	return dir, ""
}

// cgroupParaLanzar prepara el cgroup de una microVM ANTES de lanzar su VMM y
// devuelve su directorio abierto, para que spawn lo cree ya dentro
// (CLONE_INTO_CGROUP, ver enCgroup). nil si no hay cgroups o no se pudo: quien
// llama cae entonces en limitCPU tras el arranque, como siempre.
//
// Por qué: mover un proceso ya vivo escribiendo su PID en cgroup.procs toma
// el candado de escritura de los grupos de hilos del kernel, que espera un
// periodo de gracia de RCU: 4-13 ms medidos por thaw en un i7-8700T, y hasta
// 30 ms con el host ocupado. Nacer dentro solo toma el de lectura.
func (m *Manager) cgroupParaLanzar(id string, quotaPct, memMiB int) *os.File {
	if m.cgroupRoot == "" || !cloneEnCgroup {
		return nil
	}
	dir, warn := m.crearCgroup(id, quotaPct, memMiB)
	if warn != "" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	return f
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
