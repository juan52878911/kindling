//go:build darwin

package machine

// macOS: el VMM es kling-vz (docs/backend-vz.md). Lo que en Linux hace el host
// alrededor de Firecracker —namespace, iptables, cgroups, jailer, /proc— aquí
// o no existe o se le pide al propio ayudante por su API.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
)

const backendVMM = BackendVZ

// Sin jailer en macOS: el aislamiento es el proceso auxiliar de Apple que
// aloja cada VM, y el daemon corre sin root.
const jailerPosible = false

// globoSinEstadisticas: Virtualization.framework no da las estadísticas de
// memoria del invitado; squeeze aprieta a ciegas (ver objetivoSinEstadisticas).
const globoSinEstadisticas = true

// e2fsprogs no viene con macOS; Homebrew lo instala "keg-only", fuera del
// PATH, en uno de estos dos prefijos (Apple Silicon e Intel).
var dirsE2fsExtra = []string{
	"/opt/homebrew/opt/e2fsprogs/sbin", "/opt/homebrew/opt/e2fsprogs/bin",
	"/usr/local/opt/e2fsprogs/sbin", "/usr/local/opt/e2fsprogs/bin",
}

// privilegiosPlataforma: el daemon ya corre como el usuario y no hay a quién
// bajar. Ni siquiera con sudo: setpriv y el grupo kvm no existen aquí.
func privilegiosPlataforma(runAs string) (*Privileges, string) { return &Privileges{}, "" }

func delegacionCgroups() (string, error) {
	return "", errors.New("macOS has no cgroups: the per-microVM CPU limit (cpu_pct) is not applied")
}

// redAntesDeArrancar manda la política de salida de la máquina al ayudante.
// Tiene que llegar antes de InstanceStart o de snapshot/load: sin ella, el
// ayudante aplica "none" y una máquina con internet arrancaría aislada.
func (m *Manager) redAntesDeArrancar(ctx context.Context, c *fc.Client, id string) error {
	m.mu.RLock()
	mc := m.byID[id]
	var red fc.KlingNetwork
	if mc != nil {
		red.Egress = mc.Egress
		red.AllowDomains = append([]string(nil), mc.AllowDomains...)
	}
	m.mu.RUnlock()
	if mc == nil {
		return fmt.Errorf("machine %s no longer exists", shortID(id))
	}
	if red.Egress == "" {
		red.Egress = string(knet.EgressNone)
	}
	if err := c.SetKlingNetwork(ctx, red); err != nil {
		return fmt.Errorf("setting the network policy: %w", err)
	}
	return nil
}

// abrirReenvios pide al ayudante un puerto de loopback por cada puerto
// expuesto del invitado y lo guarda en la máquina. Se llama tras cada
// arranque o descongelación: el proceso es nuevo y sus puertos también.
//
// El mapa se SUSTITUYE, nunca se modifica en su sitio: persist() copia las
// máquinas por valor y serializa fuera del candado, y un mapa compartido que
// cambiara por debajo sería una carrera.
func (m *Manager) abrirReenvios(ctx context.Context, c *fc.Client, id string) error {
	m.mu.RLock()
	mc := m.byID[id]
	var puertos []int
	if mc != nil {
		puertos = mc.ExposedPorts()
	}
	m.mu.RUnlock()
	if mc == nil {
		return fmt.Errorf("machine %s no longer exists", shortID(id))
	}
	fwd, err := c.KlingForwards(ctx, puertos)
	if err != nil {
		return fmt.Errorf("opening port forwards: %w", err)
	}
	m.mu.Lock()
	if cur := m.byID[id]; cur != nil {
		cur.Forwards = fwd
	}
	m.mu.Unlock()
	return nil
}

// copiarDisco clona con clonefile de APFS (`cp -c`): la copia es instantánea
// y no ocupa nada hasta que alguna de las dos se escribe, que es lo que en
// Linux da --reflink o --sparse. Si el volumen no es APFS, cp cae solo a una
// copia normal.
func copiarDisco(ctx context.Context, src, dst string) ([]byte, error) {
	return exec.CommandContext(ctx, "cp", "-c", src, dst).CombinedOutput()
}

// perforarHuecos no hace nada: el mem.file de kling-vz es el estado que
// guarda Virtualization.framework, no un volcado crudo de la RAM con páginas
// a cero que perforar.
func perforarHuecos(ctx context.Context, path string) ([]byte, error) { return nil, nil }

// e2fsCmd localiza la herramienta de e2fsprogs fuera del PATH si hace falta.
// Sin ella, la orden falla al arrancar con un error que dice cómo instalarla,
// en vez del "executable file not found" que no dice nada.
func e2fsCmd(ctx context.Context, nombre string, args ...string) *exec.Cmd {
	if bin := buscarE2fs(nombre); bin != "" {
		return exec.CommandContext(ctx, bin, args...)
	}
	// mkfs.ext4 es un alias de mke2fs; hay instalaciones que solo traen este.
	if nombre == "mkfs.ext4" {
		if bin := buscarE2fs("mke2fs"); bin != "" {
			return exec.CommandContext(ctx, bin, append([]string{"-t", "ext4"}, args...)...)
		}
	}
	cmd := exec.CommandContext(ctx, nombre, args...)
	cmd.Err = errFaltaE2fs(nombre)
	return cmd
}

// checkPresionPlataforma rechaza con 507 cuando macOS dice que le queda poca
// memoria. PSI no existe aquí; kern.memorystatus_level es lo que usa el
// propio sistema para decidir cuándo avisar y cuándo matar procesos.
func checkPresionPlataforma() error {
	nivel, err := syscall.SysctlUint32("kern.memorystatus_level")
	if err != nil {
		return nil // sin poder medirlo no se bloquea nada
	}
	return evaluarNivelMemoria(int(nivel), minMemLevel())
}

// lanzamientoPlataforma: 4 encendidos a la vez. El prototipo vio fallos con
// ~20 restauraciones simultáneas (docs/vz-mac-prototipo.md): cada una copia el
// estado a memoria y el sistema empieza a matar procesos auxiliares.
func lanzamientoPlataforma() int { return 4 }

// memoriaVMM pregunta al ayudante: suma su huella y la del proceso auxiliar
// de Apple que aloja la VM, que es donde vive de verdad la memoria.
func memoriaVMM(pid int, sock string) int64 {
	if sock == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := fc.New(sock).KlingFootprintMiB(ctx)
	if err != nil {
		return 0
	}
	return n
}

func rssVMM(pid int, sock string) int { return int(memoriaVMM(pid, sock)) }

// memoriaHost estima la memoria disponible con el nivel del sistema sobre la
// RAM física. macOS no separa "libre" de "disponible" de una forma que sirva
// aquí, así que las dos cifras son la misma.
func memoriaHost() (available, free int64) {
	total := memoriaFisicaMiB()
	nivel, err := syscall.SysctlUint32("kern.memorystatus_level")
	if total <= 0 || err != nil {
		return 0, 0
	}
	a := total * int64(nivel) / 100
	return a, a
}

// memoriaFisicaMiB lee hw.memsize. syscall.Sysctl devuelve el entero crudo
// como cadena y le quita el último byte si es cero, así que se rellena hasta
// los ocho antes de leerlo (little-endian).
func memoriaFisicaMiB() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(leerUint64LE(s) >> 20)
}
