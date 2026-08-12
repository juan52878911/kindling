package machine

// Aislamiento con jailer para el camino de restauración (runFrom).
//
// Hoy Firecracker corre con setpriv dentro del netns de su microVM: sin
// privilegios y en su red propia, pero compartiendo el sistema de ficheros del
// anfitrión. jailer añade la capa que falta: chroot, pivot_root y un namespace
// de PID y de montaje propios, de modo que un invitado que escapara de
// Firecracker no vería el disco del host, solo un directorio con los ficheros
// que su microVM necesita —y nada más—.
//
// Va OPT-IN y solo en runFrom, a propósito. runFrom es el 95% de las máquinas en
// producción (el gateway despierta servicios restaurando), así que es donde el
// aislamiento rinde; y arrancar en frío (Run) es un camino distinto que no se
// toca hasta que este demuestre estar sólido. Se enciende con KLING_JAILER=1.
//
// El chroot exige que TODO lo que Firecracker abre esté dentro: kernel,
// snapshot, overlay y volúmenes. Se enlazan con HARDLINK, no copia: el mem.file
// se mapea (backend File), y un hardlink es el mismo inodo, así que la caché de
// páginas se comparte igual que sin jail —la densidad de 12× sobrevive—. Copiar
// un mem.file de 256 MiB por restauración destruiría justo eso.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	knet "github.com/juan52878911/kindling/internal/net"
)

// jailerEnabled indica si se restaura dentro de un jail. Se lee una vez.
func jailerEnabled() bool { return os.Getenv("KLING_JAILER") == "1" }

// jailerBin es el binario de jailer. Junto a firecracker en las instalaciones
// que lo traen.
func (m *Manager) jailerBin() string {
	if p := os.Getenv("KLING_JAILER_BIN"); p != "" {
		return p
	}
	return "jailer"
}

// jailRoot es el chroot de una microVM: <root>/jails/firecracker/<id>/root.
//
// Esa forma no la elegimos: la fija jailer, que crea <chroot-base>/firecracker/
// <id>/root y hace chroot ahí. El daemon la calcula igual para alcanzar el
// socket y para enlazar los ficheros dentro.
func (m *Manager) jailBase() string { return filepath.Join(m.root, "jails") }

func (m *Manager) jailRoot(id string) string {
	return filepath.Join(m.jailBase(), "firecracker", id, "root")
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
	dst := filepath.Join(m.jailRoot(id), hostPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst) // una restauración anterior sin limpiar apuntaría a un inodo viejo
	if err := os.Link(hostPath, dst); err != nil {
		return fmt.Errorf("enlazando %s dentro del jail: %w", hostPath, err)
	}
	// firecracker corre como el usuario del jail: tiene que poder abrirlo. Chown
	// del hardlink toca el inodo compartido con el original; para una imagen o
	// un volumen de solo lectura eso es justo lo que hace falta.
	if m.priv.Enabled {
		_ = os.Chown(dst, m.priv.UID, m.priv.GID)
	}
	return nil
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
			return nil, fmt.Errorf("no encuentro el binario de firecracker %q para jailer: %w", fc, err)
		}
		fc = resolved
	}
	argv := []string{
		m.jailerBin(),
		"--id", id,
		"--exec-file", fc,
		"--chroot-base-dir", m.jailBase(),
		"--cgroup-version", "2",
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
	return argv, nil
}

// spawnJailed lanza Firecracker dentro de un jail y devuelve su PID y el socket
// del anfitrión por el que se le habla.
//
// El socket lo crea jailer, así que hay que esperar a que aparezca antes de
// devolverlo: a diferencia del camino normal, aquí el fichero no existe hasta
// que jailer ha hecho su preparación.
func (m *Manager) spawnJailed(id string, n *knet.Net) (int, string, error) {
	// jailer se queja si su directorio ya existe de una ejecución anterior que
	// no se limpió. Se borra: el estado que importa (snapshot, overlay) vive en
	// machines/, no aquí.
	_ = os.RemoveAll(filepath.Join(m.jailBase(), "firecracker", id))

	logf, err := os.Create(filepath.Join(m.dir(id), "firecracker.log"))
	if err != nil {
		return 0, "", err
	}

	var netnsPath string
	if n != nil {
		netnsPath = filepath.Join("/var/run/netns", n.NS)
	}
	argv, err := m.jailerArgv(id, netnsPath)
	if err != nil {
		logf.Close()
		return 0, "", err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return 0, "", fmt.Errorf("lanzando jailer: %w", err)
	}
	go func() { _ = cmd.Wait(); logf.Close() }()

	return cmd.Process.Pid, m.jailSock(id), nil
}
