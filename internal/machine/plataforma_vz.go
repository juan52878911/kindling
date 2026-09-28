//go:build darwin

package machine

// macOS: el VMM es kling-vz (docs/backend-vz.md). Lo que en Linux hace el host
// alrededor de Firecracker —namespace, iptables, cgroups, jailer, /proc— aquí
// o no existe o se le pide al propio ayudante por su API.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/credproxy"
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

// delegacionCgroups: en macOS no hay cgroups; el techo de CPU lo aplica
// kling-vz (ver abrirReenvios). Sin aviso: el límite sí se aplica.
func delegacionCgroups() (string, error) { return "", nil }

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
	// Las credenciales que la máquina ya tuviera (un thaw o un reinicio), al
	// kling-vz nuevo ANTES de que el invitado corra: despierta con el dominio
	// cacheado apuntando a la pasarela, y así la primera petición ya encuentra
	// el proxy con su clave. La reentrega de después (reentregarCredenciales)
	// repite lo mismo, y además los marcadores en MMDS.
	if red.Egress == string(knet.EgressAllowlist) {
		creds, err := m.cargarCredenciales(id)
		if err != nil {
			return err
		}
		if len(creds) > 0 {
			if err := registrarCredenciales(ctx, c, nil, creds, m.credAuditPath(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

// registrarCredencialesPlataforma manda el juego completo al kling-vz de la
// máquina, que sirve el proxy en la pasarela y desvía los dominios en su DNS.
// La clave sale del daemon y se queda en la memoria de ese proceso (SECURITY.md
// §7). Con c nil (el daemon se reinició y la máquina siguió viva) no hay nada
// que hacer: el kling-vz es el mismo y conserva lo que se le dio. auditPath no
// viaja: kling-vz escribe el registro junto a su socket, que está en el mismo
// directorio de la máquina (ver vz/cmd/kling-vz).
func registrarCredencialesPlataforma(ctx context.Context, c *fc.Client, _ *knet.Net, creds []credproxy.Credential, _ string) error {
	if c == nil {
		return nil
	}
	// Un kling-vz anterior ignoraría el tipo (su JSON no lo conoce) y
	// trataría una credencial Postgres como HTTP: se pregunta antes qué
	// tipos entiende y, si no dice postgres, no se le da ninguna. Lo mismo
	// con Upstream: uno que no lo conozca marcaría el dominio en su lugar (y,
	// con -upstream-tls disable, exigiría TLS a un servidor que no lo tiene),
	// así que sin "postgres-upstream" no se le da ninguna que lo use.
	var pg, upstream bool
	for _, cr := range creds {
		if cr.Kind == credproxy.KindPostgres {
			pg = true
			upstream = upstream || cr.Upstream != "" || cr.UpstreamTLS != "" || cr.TLSServerName != ""
			// kling-vz corre confinado (vz/cmd/kling-vz/kling-vz.sb) y desde
			// ahí no llega al resolver del Mac: un upstream con nombre fallaría
			// en cada conexión. Mejor decirlo ahora.
			if credproxy.UpstreamNecesitaDNS(cr.Upstream) {
				return fmt.Errorf("credential for %s: on macOS -upstream must be an IP address or localhost (kling-vz is sandboxed and cannot use the Mac's resolver); got %s",
					cr.Domain, cr.Upstream)
			}
		}
	}
	if pg {
		info, err := c.KlingInfo(ctx)
		if err != nil {
			return fmt.Errorf("asking kling-vz for its credential kinds: %w", err)
		}
		if !slices.Contains(info.CredentialKinds, credproxy.KindPostgres) {
			return errors.New("this kling-vz does not support postgres credentials: rebuild kling-vz")
		}
		if upstream && !slices.Contains(info.CredentialKinds, credproxy.CapPostgresUpstream) {
			return errors.New("this kling-vz does not support -upstream, -upstream-tls or -tls-server-name on postgres credentials: rebuild kling-vz")
		}
	}
	out := make([]fc.KlingCredential, 0, len(creds))
	for _, cr := range creds {
		out = append(out, fc.KlingCredential{
			Env: cr.Env, Domain: cr.Domain, Placeholder: cr.Placeholder, Secret: cr.Secret,
			Allow: append([]string(nil), cr.Allow...),
			Kind:  cr.Kind, Port: cr.Port, User: cr.User, Database: cr.Database, CAPEM: cr.CAPEM,
			Upstream: cr.Upstream, UpstreamTLS: cr.UpstreamTLS, TLSServerName: cr.TLSServerName,
		})
	}
	if err := c.SetKlingCredentials(ctx, out); err != nil {
		// Un kling-vz anterior no conoce la ruta: que el error diga qué hacer.
		if strings.Contains(err.Error(), "does not implement") {
			return fmt.Errorf("%w: rebuild kling-vz to use credentials on macOS", err)
		}
		return fmt.Errorf("handing the credentials to kling-vz: %w", err)
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
	pct := defaultCPUPct
	if cur := m.byID[id]; cur != nil {
		cur.Forwards = fwd
		if cur.CPUPct > 0 {
			pct = cur.CPUPct
		}
	}
	m.mu.Unlock()
	// El techo de CPU, que en Linux pone el cgroup. kling-vz no lo aplica
	// hasta que el agente del invitado escucha, igual que el impulso de
	// arranque de Linux. Un kling-vz anterior no conoce la ruta: se sigue sin
	// techo y se avisa, como antes.
	if err := c.KlingCPU(ctx, pct); err != nil {
		log.Printf("warning: %s: kling-vz did not take the CPU limit (%v): rebuild kling-vz to apply cpu_pct on macOS", shortID(id), err)
	}
	return nil
}

// copiarDisco clona con clonefile de APFS (`cp -c`): la copia es instantánea
// y no ocupa nada hasta que alguna de las dos se escribe, que es lo que en
// Linux da --reflink o --sparse. Si el volumen no es APFS, cp cae solo a una
// copia normal.
func copiarDisco(ctx context.Context, src, dst string) ([]byte, error) {
	return exec.CommandContext(ctx, "cp", "-c", src, dst).CombinedOutput()
}

// clonarDisco copia un volumen para un snapshot o un restore y dice cómo. En
// APFS, `cp -c` clona con clonefile: instantáneo y sin ocupar nada hasta que
// las copias divergen ("clone"). Fuera de APFS cp caería sin avisar a una copia
// completa, así que se mira antes el sistema de ficheros y, si no es APFS, se
// pasa por antesDeCopiar como cualquier copia completa ("copy").
func clonarDisco(ctx context.Context, src, dst string, antesDeCopiar func() error) (string, error) {
	modo, args := "clone", []string{"-c", src, dst}
	if !esAPFS(filepath.Dir(dst)) {
		if err := antesDeCopiar(); err != nil {
			return "", err
		}
		modo, args = "copy", []string{src, dst}
	}
	if out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput(); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("copying %s: %v: %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	return modo, nil
}

// esAPFS dice si dir está en un volumen APFS, el único donde clonefile clona.
func esAPFS(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false
	}
	var b []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b) == "apfs"
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

// entornoVMM es lo que kling-vz recibe además del entorno del daemon: la raíz
// de datos, con la que se encierra en su perfil de sandbox al crear la VM
// (vz/cmd/kling-vz/kling-vz.sb): lee bajo la raíz y escribe solo en su
// directorio, snapshots/ y volumes/.
func (m *Manager) entornoVMM() []string {
	return []string{"KLING_VZ_CONFINE_ROOT=" + m.root}
}
