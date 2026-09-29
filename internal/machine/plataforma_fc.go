//go:build !darwin

package machine

// Lo que hace el HOST alrededor de Firecracker en Linux. Cada función tiene su
// gemela en plataforma_vz.go (macOS, kling-vz); el resto del paquete llama a
// estas y no pregunta en qué sistema está. Aquí todo es lo de siempre: las
// funciones solo envuelven lo que antes estaba escrito en línea.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// putSinMontar: en Linux, intentarPut monta la imagen por loop.
const putSinMontar = false

// backendVMM es el VMM con el que arranca este binario.
const backendVMM = BackendFirecracker

// jailerPosible dice si esta plataforma tiene jailer. En Linux lo decide
// decidirJailer según haya binario y usuario sin privilegios (ver jailer.go).
const jailerPosible = true

// restaurarComparteMemoria: Firecracker restaura mapeando el mem.file del
// dorado MAP_PRIVATE, así que las copias comparten sus páginas limpias.
const restaurarComparteMemoria = true

// globoSinEstadisticas: Firecracker sí da las estadísticas del invitado.
const globoSinEstadisticas = false

// auditoriaEnElDaemon: en Linux el registro de auditoría del proxy lo escribe
// el daemon (root) y vive fuera del directorio de la máquina, que es del
// usuario sin privilegios del VMM (ver credaudit.go). Variable y no constante
// solo para que los tests de macOS puedan probar también este camino.
var auditoriaEnElDaemon = true

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

// registrarCredencialesPlataforma: en Linux el proxy y el resolver son del
// daemon (internal/net); el VMM no interviene, así que c no se usa. El proxy
// escribe su registro de auditoría en auditPath.
func registrarCredencialesPlataforma(_ context.Context, _ *fc.Client, n *knet.Net, creds []credproxy.Credential, auditPath string, resolve credproxy.ResolveMachineFunc) error {
	return knet.SetCredentials(n, creds, auditPath, resolve)
}

// direccionCopiaLocked es por dónde llega el proxy del daemon al puerto port
// de la copia cp: la IP de su netns, cuyo DNAT lleva todos los puertos al
// invitado. Con m.mu tomado (cp es la entrada viva).
func direccionCopiaLocked(cp *api.Machine, port int) (string, error) {
	if cp.NetIndex <= 0 {
		return "", fmt.Errorf("machine %s has no network yet", cp.Name)
	}
	return net.JoinHostPort(knet.Plan(cp.NetIndex, cp.ID).NSIP, strconv.Itoa(port)), nil
}

// direccionListoLocked es a qué marca esperarPuertoPlataforma para saber si
// el puerto port de cp contesta (una arista depends con puerto): la misma
// dirección que usa el proxy. Con m.mu tomado.
func direccionListoLocked(cp *api.Machine, port int) (string, error) {
	return direccionCopiaLocked(cp, port)
}

// invalidarCopiaPlataforma corta las sesiones de todos los proxies hacia id.
func invalidarCopiaPlataforma(id string) int { return knet.InvalidarMaquina(id) }

// invalidarAgentePlataforma corta las sesiones hacia otras máquinas del
// proxy de la máquina de n.
func invalidarAgentePlataforma(n *knet.Net) int { return knet.InvalidarAgente(n) }

// invalidarEnlacesPlataforma corta las sesiones de enlace hacia ids en los
// proxies de enlace de todas las máquinas (internal/net).
func invalidarEnlacesPlataforma(ids ...string) int { return knet.InvalidarEnlaces(ids...) }

// invalidarOrigenPlataforma no hace nada en Linux: las sesiones que pidió una
// máquina las corta invalidarAgente (su proxy es del daemon).
func invalidarOrigenPlataforma(string) int { return 0 }

// iniciarBroker no hace nada en Linux: las aristas las sirve el daemon en el
// netns de cada nodo (internal/net), sin broker (broker.go).
func (m *Manager) iniciarBroker() {}

// cerrarBroker: ver iniciarBroker.
func (m *Manager) cerrarBroker() {}

// enviarGrafoPlataforma no hace nada en Linux: SetGraph ya lo montó todo en
// el netns del nodo.
func enviarGrafoPlataforma(context.Context, *Manager, string) error { return nil }

// esperarPuertoPlataforma espera a que addr (la IP del netns del destino)
// acepte conexiones: un nodo recién despertado tarda un poco en volver a
// escuchar.
func esperarPuertoPlataforma(ctx context.Context, _ *Manager, _, addr string, _ int) error {
	var d net.Dialer
	for {
		intento, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		c, err := d.DialContext(intento, "tcp", addr)
		cancel()
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("port %s didn't answer: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// abrirReenvios no hace nada en Linux: el host alcanza al invitado por la IP
// del veth, sin reenvíos.
func (m *Manager) abrirReenvios(ctx context.Context, c *fc.Client, id string) error { return nil }

// copiarDisco copia un disco conservando su dispersión. --sparse=always: el
// overlay es disperso y copiarlo denso destruiría lo que hace que una máquina
// cueste ~8 MB en vez de 512.
func copiarDisco(ctx context.Context, src, dst string) ([]byte, error) {
	return exec.CommandContext(ctx, "cp", "--sparse=always", src, dst).CombinedOutput()
}

// clonarDisco copia un volumen para un snapshot o un restore y dice cómo:
// "reflink" si el sistema de ficheros comparte bloques (XFS con reflink, Btrfs),
// que es instantáneo y no ocupa nada hasta que las copias divergen; "copy" si
// hubo que copiar entero, disperso.
//
// --reflink=always SIN --sparse: GNU cp rechaza --reflink con cualquier
// --sparse que no sea el de por defecto, y con --sparse=always el primer
// intento fallaría siempre y todo acabaría en copia completa sin decirlo. Un
// reflink conserva los huecos igualmente: comparte las extensiones, no las
// rellena. antesDeCopiar decide si una copia completa cabe; su error es el que
// se devuelve.
func clonarDisco(ctx context.Context, src, dst string, antesDeCopiar func() error) (string, error) {
	if _, err := exec.CommandContext(ctx, "cp", "--reflink=always", src, dst).CombinedOutput(); err == nil {
		return "reflink", nil
	}
	_ = os.Remove(dst)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := antesDeCopiar(); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return "", fmt.Errorf("copying %s: %v: %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	return "copy", nil
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
func (m *Manager) checkPresionPlataforma() error { return nil }

// minDiscoLibrePlataforma: 2 GiB en Linux, donde el swap (si lo hay) no
// crece a costa del disco de datos.
const minDiscoLibrePlataforma = 2048

// lanzamientoPlataforma es el tope de encendidos simultáneos propio de la
// plataforma; 0 = el cálculo general de maxParallelLaunch.
func lanzamientoPlataforma() int { return 0 }

// memoriaVMM es la memoria de host que ocupa el VMM de una máquina (PSS).
func memoriaVMM(pid int, sock string) int64 { return pssMiB(pid) }

// rssVMM es el RSS del VMM, la cifra que cae cuando el globo devuelve páginas.
func rssVMM(pid int, sock string) int { return procRSSMiB(pid) }

// memoriaHost es la memoria disponible y libre del anfitrión, en MiB.
func memoriaHost() (available, free int64) { return hostMemMiB() }

// entornoVMM: Firecracker no necesita nada más que el entorno del daemon (su
// confinamiento es el jailer).
func (m *Manager) entornoVMM() []string { return nil }
