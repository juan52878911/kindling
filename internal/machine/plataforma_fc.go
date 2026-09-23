//go:build !darwin

package machine

// Lo que hace el HOST alrededor de Firecracker en Linux. Cada función tiene su
// gemela en plataforma_vz.go (macOS, kling-vz); el resto del paquete llama a
// estas y no pregunta en qué sistema está. Aquí todo es lo de siempre: las
// funciones solo envuelven lo que antes estaba escrito en línea.

import (
	"context"
	"os/exec"

	"github.com/juan52878911/kindling/internal/fc"
)

// backendVMM es el VMM con el que arranca este binario.
const backendVMM = BackendFirecracker

// jailerPosible dice si esta plataforma tiene jailer. En Linux lo decide
// jailerEnabled según haya binario y root.
const jailerPosible = true

// globoSinEstadisticas: Firecracker sí da las estadísticas del invitado.
const globoSinEstadisticas = false

// dirsE2fsExtra son directorios donde buscar e2fsprogs además del PATH, /sbin
// y /usr/sbin. En Linux no hace falta ninguno.
var dirsE2fsExtra []string

// privilegiosPlataforma resuelve el usuario sin privilegios del VMM.
func privilegiosPlataforma(runAs string) (*Privileges, string) { return resolvePrivileges(runAs) }

// delegacionCgroups prepara el cgroup donde se limita la CPU de cada VMM.
func delegacionCgroups() (string, error) { return ensureDelegation() }

// redAntesDeArrancar no hace nada en Linux: la red ya la montó el namespace
// antes de lanzar el VMM.
func (m *Manager) redAntesDeArrancar(ctx context.Context, c *fc.Client, id string) error { return nil }

// abrirReenvios no hace nada en Linux: el host alcanza al invitado por la IP
// del veth, sin reenvíos.
func (m *Manager) abrirReenvios(ctx context.Context, c *fc.Client, id string) error { return nil }

// copiarDisco copia un disco conservando su dispersión. --sparse=always: el
// overlay es disperso y copiarlo denso destruiría lo que hace que una máquina
// cueste ~8 MB en vez de 512.
func copiarDisco(ctx context.Context, src, dst string) ([]byte, error) {
	return exec.CommandContext(ctx, "cp", "--sparse=always", src, dst).CombinedOutput()
}

// perforarHuecos devuelve al disco las páginas a cero de un volcado de
// memoria (ver Freeze).
func perforarHuecos(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, "fallocate", "--dig-holes", path).CombinedOutput()
}

// e2fsCmd prepara una orden de e2fsprogs. En Linux van por el PATH, como
// siempre.
func e2fsCmd(ctx context.Context, nombre string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, nombre, args...)
}

// checkPresionPlataforma no añade nada en Linux: allí la presión la mide PSI
// en checkPressure.
func checkPresionPlataforma() error { return nil }

// lanzamientoPlataforma es el tope de encendidos simultáneos propio de la
// plataforma; 0 = el cálculo general de maxParallelLaunch.
func lanzamientoPlataforma() int { return 0 }

// memoriaVMM es la memoria de host que ocupa el VMM de una máquina (PSS).
func memoriaVMM(pid int, sock string) int64 { return pssMiB(pid) }

// rssVMM es el RSS del VMM, la cifra que cae cuando el globo devuelve páginas.
func rssVMM(pid int, sock string) int { return procRSSMiB(pid) }

// memoriaHost es la memoria disponible y libre del anfitrión, en MiB.
func memoriaHost() (available, free int64) { return hostMemMiB() }
