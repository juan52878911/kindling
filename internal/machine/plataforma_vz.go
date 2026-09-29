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
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

const backendVMM = BackendVZ

// Las aristas de un grafo y kling db attach (una credencial Postgres con
// UpstreamMachine) llegan a la otra máquina por el broker (broker.go): el
// kling-vz del origen pide la arista, el daemon comprueba, marca al reenvío
// del destino y le entrega el socket conectado. kling-vz nunca marca él
// mismo a un reenvío (upstream.go se lo sigue prohibiendo) ni ve una
// dirección.

// direccionCopiaLocked es por dónde llega el DAEMON al puerto port de la
// máquina cp: su reenvío en el loopback, del rango reservado (reenvios.go).
// Con m.mu tomado. Solo lo usa el broker, que marca él mismo.
func direccionCopiaLocked(cp *api.Machine, port int) (string, error) {
	addr := cp.Forwards[strconv.Itoa(port)]
	if addr == "" {
		return "", fmt.Errorf("machine %s has no forward for port %d yet", cp.Name, port)
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() || !credproxy.PuertoReservado(int(ap.Port())) {
		return "", fmt.Errorf("machine %s: forward %q is not in kindling's reserved range", cp.Name, addr)
	}
	return addr, nil
}

// direccionListoLocked no da dirección en macOS: el puerto de una arista
// depends no tiene por qué tener reenvío, y esperarPuertoPlataforma pregunta
// al kling-vz de la máquina (KlingProbe) por su id, sin marcar nada.
func direccionListoLocked(*api.Machine, int) (string, error) { return "", nil }

// invalidarCopiaPlataforma corta las sesiones de credenciales que el broker
// entregó hacia id.
func invalidarCopiaPlataforma(id string) int { return invalidarCopiaBroker(id) }

// invalidarAgentePlataforma no hace nada: las sesiones que pidió una máquina
// las corta invalidarOrigen, que tiene su ID (n no lo lleva).
func invalidarAgentePlataforma(*knet.Net) int { return 0 }

// invalidarEnlacesPlataforma corta las sesiones de enlace que el broker
// entregó hacia ids.
func invalidarEnlacesPlataforma(ids ...string) int { return invalidarEnlacesBroker(ids...) }

// invalidarOrigenPlataforma corta las sesiones que pidió la máquina id.
func invalidarOrigenPlataforma(id string) int { return invalidarOrigenBroker(id) }

// esperarPuertoPlataforma espera a que algo escuche en el puerto port del
// invitado de la máquina id. El reenvío acepta siempre (lo abre kling-vz),
// así que se pregunta a kling-vz (GET /kling/probe).
func esperarPuertoPlataforma(ctx context.Context, m *Manager, id, addr string, port int) error {
	for {
		m.mu.RLock()
		sock := m.socket[id]
		m.mu.RUnlock()
		if sock != "" {
			if ok, err := fc.New(sock).KlingProbe(ctx, port); err == nil && ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("port %d of %s didn't answer: %w", port, shortID(id), ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// capacidadGrafo comprueba que el kling-vz de c sabe pedir conexiones al
// daemon (credproxy.CapGraphLink).
func capacidadGrafo(ctx context.Context, c *fc.Client) error {
	info, err := c.KlingInfo(ctx)
	if err != nil {
		return fmt.Errorf("asking kling-vz for its credential kinds: %w", err)
	}
	if !slices.Contains(info.CredentialKinds, credproxy.CapGraphLink) {
		return errors.New("this kling-vz can't reach other machines (graph edges, kling db attach): rebuild kling-vz")
	}
	return nil
}

// enviarGrafoVZ manda al kling-vz de la máquina id sus aristas salientes (si
// es el nodo de un grafo con aristas): las link y los nombres <nodo>.graph.
// Sin direcciones: cada conexión se pide al broker.
func enviarGrafoVZ(ctx context.Context, m *Manager, c *fc.Client, id string) error {
	m.mu.RLock()
	mc := m.byID[id]
	var g fc.KlingGraph
	hay := false
	if mc != nil {
		g, hay = m.aristasVZLocked(mc)
	}
	m.mu.RUnlock()
	if !hay {
		return nil
	}
	if err := capacidadGrafo(ctx, c); err != nil {
		return err
	}
	if err := c.SetKlingGraph(ctx, g); err != nil {
		return fmt.Errorf("handing the graph edges to kling-vz: %w", err)
	}
	return nil
}

// enviarGrafoPlataforma es enviarGrafoVZ con el socket de la máquina.
func enviarGrafoPlataforma(ctx context.Context, m *Manager, id string) error {
	m.mu.RLock()
	sock := m.socket[id]
	m.mu.RUnlock()
	if sock == "" {
		return fmt.Errorf("no socket for %s", shortID(id))
	}
	return enviarGrafoVZ(ctx, m, fc.New(sock), id)
}

// aristasVZLocked son las aristas salientes del nodo de mc, en la forma que
// entiende kling-vz. Con m.mu tomado.
func (m *Manager) aristasVZLocked(mc *api.Machine) (fc.KlingGraph, bool) {
	var g fc.KlingGraph
	spec, hay := m.especRedGrafoLocked(mc)
	if !hay {
		return g, false
	}
	for _, l := range spec.Links {
		g.Links = append(g.Links, fc.KlingGraphLink{Host: l.Host, Port: l.Port})
	}
	vistos := map[string]bool{}
	gr := m.grafos[mc.Labels[api.LabelGraph]]
	for _, e := range gr.Edges {
		if e.From == mc.Labels[api.LabelGraphNode] && !vistos[e.Host()] {
			vistos[e.Host()] = true
			g.Hosts = append(g.Hosts, e.Host())
		}
	}
	sort.Strings(g.Hosts)
	return g, true
}

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
	// Las aristas del nodo, si es de un grafo (un thaw): antes de que el
	// invitado corra, que despierta sabiendo ya a qué nombres conectar. Un
	// fallo no impide arrancar: las aristas fallan cerradas, y se dice.
	if err := enviarGrafoVZ(ctx, m, c, id); err != nil {
		log.Printf("%s woke up without its graph edges: %v", shortID(id), err)
	}
	// Las credenciales que la máquina ya tuviera (un thaw o un reinicio), al
	// kling-vz nuevo ANTES de que el invitado corra: despierta con el dominio
	// cacheado apuntando a la pasarela, y así la primera petición ya encuentra
	// el proxy con su clave. La reentrega de después (reentregarCredenciales)
	// repite lo mismo, y además los marcadores en MMDS.
	// Fuera de allowlist solo puede tener las de sus aristas credential, que
	// van todas a otra máquina (entregarCredencialesNodo).
	creds, err := m.cargarCredenciales(id)
	if err != nil {
		return err
	}
	if len(creds) > 0 && (red.Egress == string(knet.EgressAllowlist) || todasAMaquina(creds)) {
		if err := registrarCredenciales(ctx, c, nil, creds, m.credAuditPath(id), nil); err != nil {
			return err
		}
	}
	return nil
}

// todasAMaquina dice si todas las credenciales van a otra máquina.
func todasAMaquina(creds []credproxy.Credential) bool {
	for _, c := range creds {
		if c.UpstreamMachine == "" {
			return false
		}
	}
	return true
}

// registrarCredencialesPlataforma manda el juego completo al kling-vz de la
// máquina, que sirve el proxy en la pasarela y desvía los dominios en su DNS.
// La clave sale del daemon y se queda en la memoria de ese proceso (SECURITY.md
// §7). Con c nil (el daemon se reinició y la máquina siguió viva) no hay nada
// que hacer: el kling-vz es el mismo y conserva lo que se le dio. auditPath no
// viaja: kling-vz escribe el registro junto a su socket, que está en el mismo
// directorio de la máquina (ver vz/cmd/kling-vz).
func registrarCredencialesPlataforma(ctx context.Context, c *fc.Client, _ *knet.Net, creds []credproxy.Credential, _ string, _ credproxy.ResolveMachineFunc) error {
	if c == nil {
		return nil
	}
	// Un kling-vz anterior ignoraría el tipo (su JSON no lo conoce) y
	// trataría una credencial Postgres como HTTP: se pregunta antes qué
	// tipos entiende y, si no dice postgres, no se le da ninguna. Lo mismo
	// con Upstream: uno que no lo conozca marcaría el dominio en su lugar (y,
	// con -upstream-tls disable, exigiría TLS a un servidor que no lo tiene),
	// así que sin "postgres-upstream" no se le da ninguna que lo use.
	var pg, upstream, maquina bool
	for _, cr := range creds {
		maquina = maquina || cr.UpstreamMachine != ""
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
		// Uno que no pide conexiones al daemon trataría upstream_machine
		// como si no estuviera y marcaría el dominio: ninguna le llega.
		if maquina && !slices.Contains(info.CredentialKinds, credproxy.CapGraphLink) {
			return errors.New("this kling-vz can't reach other machines (graph credential edges, kling db attach): rebuild kling-vz")
		}
	}
	out := make([]fc.KlingCredential, 0, len(creds))
	for _, cr := range creds {
		out = append(out, fc.KlingCredential{
			Env: cr.Env, Domain: cr.Domain, Placeholder: cr.Placeholder, Secret: cr.Secret,
			Allow: append([]string(nil), cr.Allow...),
			Kind:  cr.Kind, Port: cr.Port, User: cr.User, Database: cr.Database, AnyDatabase: cr.AnyDatabase, CAPEM: cr.CAPEM,
			Upstream: cr.Upstream, UpstreamTLS: cr.UpstreamTLS, TLSServerName: cr.TLSServerName,
			UpstreamMachine: cr.UpstreamMachine, UpstreamOwner: cr.UpstreamOwner,
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
	// Fuera del rango reservado, un upstream del loopback de otra máquina
	// podría dar con este invitado (reenvios.go).
	if err := validarReenvios(fwd); err != nil {
		return err
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
//
// Y el socket del broker (broker_vz.go), por el que pide las conexiones de las
// aristas y de kling db attach; su perfil solo le deja conectar a ese.
func (m *Manager) entornoVMM() []string {
	env := []string{"KLING_VZ_CONFINE_ROOT=" + m.root}
	if m.brokerRuta != "" {
		env = append(env, "KLING_VZ_BROKER="+m.brokerRuta)
	}
	return env
}
