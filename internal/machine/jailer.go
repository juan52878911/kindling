package machine

// Aislamiento con jailer para TODOS los caminos que arrancan un firecracker
// (Run en frío, runFrom al restaurar, Thaw al descongelar): chroot,
// pivot_root y namespaces de PID y de montaje propios, de modo que un
// invitado que escapara de Firecracker no vería el disco del host, solo un
// directorio con los ficheros que su microVM necesita —y nada más—.
// Complementa a setpriv/netns (ver privdrop.go y red.go), que ya bajan
// privilegios y aíslan la red pero comparten el filesystem del anfitrión.
//
// POR DEFECTO en Linux (P7 del plan de remediación: "hostil por defecto"). Si
// el binario jailer y el usuario sin privilegios (con el grupo kvm) existen,
// se usa sin configurar nada. Si falta alguno de los dos, el daemon se NIEGA
// a arrancar máquinas NUEVAS (Run, runFrom, el Thaw de una congelada) con un
// error que explica qué falta y cómo arreglarlo; las máquinas ya vivas y los
// comandos de solo lectura siguen funcionando igual. Antes jailer era
// opcional por defecto, y eso dejaba la barrera más fuerte apagada justo en
// las instalaciones donde nadie se había leído SECURITY.md.
//
// KLING_JAILER=0 apaga la barrera a propósito, para el host que todavía no
// puede instalar jailer o el usuario de servicio: el daemon arranca máquinas
// igual que si jailer no existiera, pero avisa una vez y bien alto en el log
// al arrancar (ver JailerWarning), porque apagar la barrera más fuerte a
// propósito no debería pasar desapercibida. KLING_JAILER=1 la fuerza aunque
// falte el usuario —jailer exige uid/gid: sin usuario de servicio se le da
// root explícito, igual que antes, y se avisa al arrancar—, pero si falta el
// propio binario sigue sin haber nada que ejecutar.
//
// El chroot exige que TODO lo que Firecracker abre esté dentro: kernel,
// snapshot, overlay y volúmenes. Se enlazan con HARDLINK, no copia: el mem.file
// se mapea (backend File), y un hardlink es el mismo inodo, así que la caché de
// páginas se comparte igual que sin jail —la densidad de 12× sobrevive—. Copiar
// un mem.file de 256 MiB por restauración destruiría justo eso.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	knet "github.com/juan52878911/kindling/internal/net"
)

// jailerBinName es el binario de jailer a buscar: KLING_JAILER_BIN si está, o
// "jailer" a secas —junto a firecracker en las instalaciones que lo traen—.
func jailerBinName() string {
	if p := os.Getenv("KLING_JAILER_BIN"); p != "" {
		return p
	}
	return "jailer"
}

// jailerLookPath resuelve jailerBinName() en PATH. Variable y no una llamada
// directa a exec.LookPath para que los tests simulen "no está instalado" sin
// tocar el PATH real del proceso.
var jailerLookPath = exec.LookPath

// jailerBinPresent dice si el binario de jailer se puede ejecutar.
func jailerBinPresent() bool {
	_, err := jailerLookPath(jailerBinName())
	return err == nil
}

// decidirJailer es la selección de jailer en sí, aislada de leer el entorno y
// el sistema (variable de entorno, PATH, usuario del sistema) para poder
// testear las combinaciones sin binarios, usuarios ni permisos reales.
//
// posible es jailerPosible: falso en macOS/vz, donde jailer no existe y nada
// de esto aplica —ni siquiera el aviso de KLING_JAILER=0—. forced es
// KLING_JAILER tal cual: "1", "0" o "" (automático). binPresent y userReady
// son si el binario está en PATH y si el usuario sin privilegios (con el
// grupo kvm) quedó listo (ver Privileges.Enabled); userReason es por qué no
// lo está (Privileges.Motivo), para que el bloqueo diga la causa real. Vacío,
// se usa un texto genérico. root es si el daemon corre como root: antes
// bastaba, con el binario, para jailear como root aunque faltara el usuario;
// desde A3 solo cambia el mensaje del bloqueo (ver abajo).
//
// jailed dice si hay que arrancar dentro del jail. blocked, si no está vacío,
// es el motivo por el que Run, runFrom y Thaw deben NEGARSE a arrancar
// máquinas nuevas: solo en automático, y solo si falta el binario o el
// usuario —forzarlo con KLING_JAILER=1 es una decisión explícita y se
// respeta aunque falte el usuario (ver jailerArgv), aunque no si falta el
// propio binario, que no hay cómo ejecutar—. startupWarn, si no está vacío,
// es el aviso de SEGURIDAD que hay que imprimir UNA VEZ al arrancar el daemon:
// con el opt-out explícito, o con KLING_JAILER=1 sin usuario (jailer como
// root), porque apagar la barrera más fuerte a
// propósito merece ruido, no una nota discreta en un log que nadie relee.
func decidirJailer(posible bool, forced string, binPresent, userReady bool, userReason string, root bool) (jailed bool, blocked, startupWarn string) {
	if !posible {
		return false, "", ""
	}
	switch forced {
	case "1":
		if !binPresent {
			return false, "jailer forced with KLING_JAILER=1 but its binary isn't on " +
				"PATH: install it (sudo ./scripts/20-install-firecracker.sh) or point " +
				"KLING_JAILER_BIN at it", ""
		}
		if !userReady {
			// Forzado sin usuario: se respeta (es la salida explícita del
			// bloqueo de arriba), pero merece el mismo ruido que KLING_JAILER=0.
			return true, "", "jailer forced with KLING_JAILER=1 runs Firecracker as ROOT, with " +
				"no privilege drop inside the jail: a VMM that escaped its guest could write " +
				"through the jail's hardlinks to the kernel, base images and golden snapshots " +
				"shared by every microVM (SECURITY.md §11). Create the unprivileged service " +
				"user (kindling, in the kvm group) to drop root."
		}
		return true, "", ""
	case "0":
		return false, "", "jailer disabled by KLING_JAILER=0: microVMs run WITHOUT the " +
			"jailer chroot/pivot_root isolation (SECURITY.md §11). A guest that escaped " +
			"Firecracker would reach the host filesystem, not just a jail. Meant for " +
			"hosts that can't install jailer or the unprivileged user yet."
	}
	if binPresent && userReady {
		return true, "", ""
	}
	// Binario y root, pero sin usuario de servicio: el daemon de antes jaileaba
	// igual, como root (ver jailerArgv), con un aviso. Ya no (A3): con --uid 0
	// el VMM conserva CAP_DAC_OVERRIDE dentro del chroot, y los ficheros del
	// jail son HARDLINKS, el mismo inodo que el kernel, las bases y los
	// dorados de TODAS las microVMs. Un Firecracker comprometido podría
	// reescribirlos para las siguientes. El chroot no protege de eso; bajar de
	// root sí. Se bloquea como cuando falta el usuario, y el mensaje dice cómo
	// crearlo y cómo forzar el modo viejo (KLING_JAILER=1) si se acepta.
	if binPresent && root {
		motivo := userReason
		if motivo == "" {
			motivo = "the unprivileged service user doesn't exist or lacks the kvm group"
		}
		return false, "refusing to start: jailer would run Firecracker as ROOT because " + motivo +
			"; as root the VMM can write through the jail's hardlinks to the kernel, base " +
			"images and golden snapshots shared by every microVM. Create the user (sudo " +
			"useradd --system --no-create-home --shell /usr/sbin/nologin kindling && sudo " +
			"usermod -aG kvm kindling, or pass -run-as/KLING_RUN_AS) and restart the daemon; " +
			"KLING_JAILER=1 forces jailing as root anyway (SECURITY.md §11)", ""
	}
	var missing []string
	if !binPresent {
		missing = append(missing, "the jailer binary isn't on PATH (install it with "+
			"sudo ./scripts/20-install-firecracker.sh, or point KLING_JAILER_BIN at it)")
	}
	if !userReady && userReason != "" {
		missing = append(missing, userReason)
	} else if !userReady {
		missing = append(missing, "the unprivileged user Firecracker runs as doesn't "+
			"exist or lacks the kvm group (sudo useradd --system --no-create-home "+
			"--shell /usr/sbin/nologin kindling && sudo usermod -aG kvm kindling, or "+
			"pass -run-as/KLING_RUN_AS if it's named differently)")
	}
	return false, fmt.Sprintf("refusing to start: jailer is required by default on "+
		"Linux (SECURITY.md §11) and %s; opt out explicitly with KLING_JAILER=0 if "+
		"you accept running without it for now", strings.Join(missing, ", and ")), ""
}

// jailerBin es el binario de jailer. Junto a firecracker en las instalaciones
// que lo traen.
func (m *Manager) jailerBin() string { return jailerBinName() }

// jailRoot es el chroot de una microVM: <root>/jails/firecracker/<id>/root.
//
// Esa forma no la elegimos: la fija jailer, que crea <chroot-base>/firecracker/
// <id>/root y hace chroot ahí. El daemon la calcula igual para alcanzar el
// socket y para enlazar los ficheros dentro.
func (m *Manager) jailBase() string { return filepath.Join(m.root, "jails") }

func (m *Manager) jailRoot(id string) string {
	return filepath.Join(m.jailBase(), "firecracker", id, "root")
}

// jailPath es dónde cae, dentro del jail, un fichero cuyo path absoluto en el
// host es hostPath. La convención de todo el jail: los paths que se pasan a la
// API son los absolutos del host, y resuelven dentro del chroot a esta ruta.
func (m *Manager) jailPath(id, hostPath string) string {
	return filepath.Join(m.jailRoot(id), hostPath)
}

// jailSock es el socket de la API dentro del jail, visto desde el anfitrión.
// jailer lo crea en <root>/run/firecracker.socket.
func (m *Manager) jailSock(id string) string {
	return filepath.Join(m.jailRoot(id), "run", "firecracker.socket")
}

// linkAbs replica un fichero del host DENTRO del jail en su MISMA ruta absoluta.
//
// Es lo que exige LoadSnapshot: firecracker abre cada drive con el path que
// quedó GRABADO en el snapshot —comprobado en el laboratorio, el load falla con
// "No such file or directory /var/lib/kindling/images/X.ext4" si no está—. Así
// que el path que se le pasa NO cambia; lo que cambia es que ese path resuelva,
// dentro del chroot, al hardlink que aquí se crea.
//
// Hardlink y no copia: para el mem.file es la diferencia entre compartir la
// caché de páginas —la densidad de 12×— y duplicar 256 MiB por restauración. Un
// hardlink es el mismo inodo, así que se comparte igual que sin jail.
func (m *Manager) linkAbs(id, hostPath string) error {
	return m.linkComo(id, hostPath, hostPath)
}

// linkComo enlaza src dentro del jail bajo la ruta que tiene hostPath en el
// host. Con src == hostPath es linkAbs. Sirve para que Firecracker, al abrir
// una ruta grabada en un snapshot, reciba otro fichero: la copia propia de la
// instancia en vez del overlay del dorado (ver runFrom).
func (m *Manager) linkComo(id, hostPath, src string) error {
	dst := filepath.Join(m.jailRoot(id), hostPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst) // una restauración anterior sin limpiar apuntaría a un inodo viejo
	if err := os.Link(src, dst); err != nil {
		return fmt.Errorf("linking %s inside the jail: %w", src, err)
	}
	hostPath = src
	// firecracker corre como el usuario del jail: tiene que poder abrirlo. El
	// hardlink es el MISMO inodo que el original, así que tocarlo es tocar el
	// original. Antes se le hacía chown al usuario del VMM, y así el kernel, la
	// base y los dorados acababan siendo SUYOS, escribibles por un Firecracker
	// comprometido para todas las microVMs futuras. Ahora: lo que ya es del VMM
	// (su overlay, sus volúmenes, su volcado) se deja; lo demás, que es de solo
	// lectura, se le da por grupo con dueño root (ver EnsureReadable).
	if m.priv.Enabled {
		darLecturaVMM(hostPath, m.priv)
	}
	return nil
}

// darLecturaVMM hace legible por grupo para el VMM un fichero que no es suyo,
// sin cederle la propiedad. Sin seguir enlaces simbólicos.
func darLecturaVMM(ruta string, p *Privileges) {
	fi, err := os.Lstat(ruta)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == p.UID {
		return
	}
	if int(st.Gid) != p.GID {
		_ = os.Lchown(ruta, -1, p.GID)
	}
	if fi.Mode().Perm()&0o040 == 0 {
		_ = os.Chmod(ruta, fi.Mode().Perm()|0o040)
	}
}

// prepareJail replica dentro del jail todo lo que la restauración va a abrir:
// el snapshot, el rootfs, el overlay y los volúmenes. Se llama tras arrancar
// jailer (que crea el chroot) y antes de la primera llamada a la API.
func (m *Manager) prepareJail(id string, paths ...string) error {
	for _, hp := range paths {
		if hp == "" {
			continue
		}
		if err := m.linkAbs(id, hp); err != nil {
			return err
		}
	}
	return nil
}

// jailerArgv construye la invocación de jailer.
//
// jailer hace por su cuenta lo que hoy hacen setpriv y `ip netns exec`: baja
// privilegios (--uid/--gid) y entra en el netns (--netns). Por eso el comando NO
// se envuelve con priv.Wrap ni net.Wrap: sería hacerlo dos veces, y la segunda
// fallaría.
func (m *Manager) jailerArgv(id string, netnsPath string) ([]string, error) {
	// jailer NO resuelve el PATH: exige una ruta absoluta al binario para
	// canonicalizarla. m.fcBin suele ser "firecracker" a secas, así que se
	// resuelve aquí. El fallo, si no, es "Failed to canonicalize path
	// firecracker" — un mensaje de jailer que no menciona la causa real.
	fc := m.fcBin
	if !filepath.IsAbs(fc) {
		resolved, err := exec.LookPath(fc)
		if err != nil {
			return nil, fmt.Errorf("can't find firecracker binary %q for jailer: %w", fc, err)
		}
		fc = resolved
	}
	argv := []string{
		m.jailerBin(),
		"--id", id,
		"--exec-file", fc,
		"--chroot-base-dir", m.jailBase(),
		"--cgroup-version", "2",
		// Espacio de PID propio: Firecracker es su PID 1 y no ve (ni puede
		// señalar, ni leer en /proc) ningún otro proceso del host. jailer
		// hace fork, apunta el PID del hijo en <root>/<exec>.pid y sale: ver
		// pidDelHijo.
		"--new-pid-ns",
	}
	if m.priv.Enabled {
		argv = append(argv,
			"--uid", strconv.Itoa(m.priv.UID),
			"--gid", strconv.Itoa(m.priv.GID),
		)
	} else {
		// jailer EXIGE uid/gid. Sin usuario de servicio, se le da root explícito:
		// el aislamiento por chroot sigue valiendo aunque no baje privilegios.
		argv = append(argv, "--uid", "0", "--gid", "0")
	}
	if netnsPath != "" {
		argv = append(argv, "--netns", netnsPath)
	}
	// Todo lo que va tras `--` son los argumentos de Firecracker. El socket es
	// relativo al chroot: jailer lo crea en /run dentro del jail.
	argv = append(argv, "--", "--api-sock", "/run/firecracker.socket")
	argv = append(argv, argsVMM()...)
	return argv, nil
}

// spawnJailed lanza Firecracker dentro de un jail y devuelve su PID y el socket
// del anfitrión por el que se le habla.
//
// El socket lo crea jailer, así que hay que esperar a que aparezca antes de
// devolverlo: a diferencia del camino normal, aquí el fichero no existe hasta
// que jailer ha hecho su preparación.
//
// cg es como en spawn: el cgroup en el que nace el proceso, si se puede.
func (m *Manager) spawnJailed(id string, n *knet.Net, cg *os.File) (int, string, bool, error) {
	// jailer canonicaliza --chroot-base-dir antes de nada, así que tiene que
	// existir. Si no se crea aquí, el primero en crearlo era el propio jailer
	// (al hacer firecracker/<id>), y el segundo de dos arranques a la vez en
	// una raíz nueva fallaba con "Failed to canonicalize path .../jails".
	// 0700, como lo deja restringirRaiz.
	if err := os.MkdirAll(m.jailBase(), 0o700); err != nil {
		return 0, "", false, fmt.Errorf("creating the jail directory: %w", err)
	}
	// jailer se queja si su directorio ya existe de una ejecución anterior que
	// no se limpió. Se borra: el estado que importa (snapshot, overlay) vive en
	// machines/, no aquí.
	if err := m.borrarJail(id); err != nil {
		return 0, "", false, err
	}
	// El overlay en el almacén de discos entra al jail por bind, y tiene que
	// estar montado antes de que jailer haga su chroot (ver prepararBindsJail).
	if err := m.prepararBindsJail(id); err != nil {
		return 0, "", false, err
	}

	logf, err := abrirConsola(m.dir(id))
	if err != nil {
		return 0, "", false, err
	}

	var netnsPath string
	if n != nil {
		netnsPath = filepath.Join("/var/run/netns", n.NS)
	}
	argv, err := m.jailerArgv(id, netnsPath)
	if err != nil {
		logf.Close()
		return 0, "", false, err
	}
	cmd, enCg, err := arrancarEnCgroup(argv, logf, cg)
	if err != nil {
		logf.Close()
		return 0, "", false, fmt.Errorf("launching jailer: %w", err)
	}
	// Con --new-pid-ns el jailer que lanzamos no es el VMM: hace fork y sale.
	// Firecracker queda huérfano (lo recoge init) y su PID, el del host, es
	// el que deja apuntado jailer. Se vigila por pidfd como uno readoptado.
	err = cmd.Wait()
	logf.Close()
	if err != nil {
		return 0, "", false, fmt.Errorf("jailer: %w", err)
	}
	pid, err := m.pidDelHijo(id, argv)
	if err != nil {
		return 0, "", false, err
	}
	if !vigilarSalida(pid, func() { m.vmmTermino(id, detalleEnjaulado) }) {
		log.Printf("jailer: can't watch the VMM of %s (pid %d) for its exit; the watcher will notice it",
			shortID(id), pid)
	}
	return pid, m.jailSock(id), enCg, nil
}

// detalleEnjaulado es el detalle de la salida de un VMM enjaulado: no es hijo
// del daemon (lo es del jailer, que ya salió), así que no hay código de salida.
const detalleEnjaulado = "jailed in its own PID namespace, no exit status"

// pidDelHijo lee el PID de Firecracker que jailer --new-pid-ns apunta en
// <root>/<nombre del ejecutable>.pid. Lo escribe el jailer (root) antes de
// salir, y Firecracker aún no ha arrancado ningún invitado: nada que lo haya
// podido reescribir. Aun así se abre sin seguir enlaces.
func (m *Manager) pidDelHijo(id string, argv []string) (int, error) {
	i := slices.Index(argv, "--exec-file")
	if i < 0 || i+1 >= len(argv) {
		return 0, fmt.Errorf("jailer argv without --exec-file")
	}
	ruta := filepath.Join(m.jailRoot(id), filepath.Base(argv[i+1])+".pid")
	fd, err := syscall.Open(ruta, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("reading the VMM pid from jailer: %s: %w", ruta, err)
	}
	b := make([]byte, 32)
	n, _ := syscall.Read(fd, b)
	_ = syscall.Close(fd)
	if n < 0 {
		n = 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("jailer left a bad pid file %s: %q", ruta, b[:n])
	}
	return pid, nil
}
