//go:build linux

package machine

// La jaula de e2fsprogs en Linux (A1 de la segunda revisión).
//
// e2fsck, resize2fs y debugfs analizan un ext4 que escribió el invitado. Antes
// corrían con el usuario del VMM (el dueño de los overlays y volúmenes cedidos)
// y sin namespace de montajes: un e2fsck explotado leía y escribía los discos
// de todas las máquinas y todas las capas. Ahora no corren directamente: el
// daemon se vuelve a ejecutar a sí mismo (/proc/self/exe) con argv[0] =
// argJaulaE2fs, y ese hijo, todavía root pero ya en namespaces nuevos de
// montajes, red, PID e IPC, monta una raíz mínima (tmpfs de solo lectura con
// /usr, /lib*, /bin y /sbin del host en solo lectura, un /proc de su propio
// namespace de PID y /dev/fd), hace chroot en ella, baja a nobody sin grupos
// suplementarios, se pone el tope de memoria (RLIMIT_AS) y no_new_privs, y
// ejecuta la orden. El disco le llega abierto como descriptor 3: dentro de la
// jaula no hay ninguna ruta que lleve a otra máquina.
//
// El tope de memoria lo pone el hijo con setrlimit, así que ya no depende de
// prlimit (M6): sin root no hay jaula ni cambio de usuario (no hay a quién
// bajar), pero el tope sí.
//
// Hay entornos donde montar no se puede (un contenedor sin CAP_SYS_ADMIN, un
// perfil de AppArmor que prohíbe montar proc). La primera vez se prueba la
// jaula entera (sondaJaulaE2fs); si falla, se avisa una vez, Ajustes() lo
// dice ("e2fs_confinement") y se sigue con lo de antes de la jaula: nobody,
// namespaces de red, PID e IPC y el tope, sin chroot.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// argJaulaE2fs es el argv[0] con que el daemon se ejecuta a sí mismo para
// encerrar una orden de e2fsprogs (ver init abajo).
const argJaulaE2fs = "kling-e2fs-jaula"

// Modos del hijo: jaula (chroot y todo lo demás), simple (sin chroot: el
// daemon sin root o donde la jaula no se puede montar) y sonda (monta la
// jaula, baja de usuario y sale sin ejecutar nada).
const (
	modoJaula  = "jaula"
	modoSimple = "simple"
	modoSonda  = "sonda"
)

// codigoFalloJaula es con lo que sale el hijo si no pudo encerrarse: no es un
// código de e2fsck (revisarExt4 lo trata como error, con su salida).
const codigoFalloJaula = 125

// dirsSistemaJaula son los directorios del host que ve la orden encerrada, en
// solo lectura: lo que necesita el cargador dinámico para llegar a sus
// bibliotecas. Si es un enlace (/bin -> usr/bin), dentro también.
var dirsSistemaJaula = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}

// ficherosSistemaJaula son los ficheros sueltos de /etc que lee: la caché del
// cargador (sin ella, en Debian no encuentra las bibliotecas multiarch) y la
// configuración de e2fsprogs. Los que no existen se saltan.
var ficherosSistemaJaula = []string{"/etc/ld.so.cache", "/etc/e2fsck.conf", "/etc/mke2fs.conf"}

// dispositivosJaula son los dispositivos que puede abrir: los inocuos.
var dispositivosJaula = []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"}

func init() {
	if len(os.Args) > 0 && os.Args[0] == argJaulaE2fs {
		err := hijoJaulaE2fs(os.Args[1:])
		fmt.Fprintf(os.Stderr, "kling: e2fsprogs confinement: %v\n", err)
		os.Exit(codigoFalloJaula)
	}
}

// ordenJaula es lo que el hijo recibe en argv: el modo, a quién bajar (-1:
// no cambiar), el tope de memoria, el directorio sobre el que montar la raíz
// y la orden.
type ordenJaula struct {
	modo     string
	uid, gid int
	mem      int
	raiz     string
	argv     []string // argv[0] es la ruta absoluta del binario
}

func (o ordenJaula) args() []string {
	return append([]string{argJaulaE2fs, o.modo, strconv.Itoa(o.uid), strconv.Itoa(o.gid),
		strconv.Itoa(o.mem), o.raiz, "--"}, o.argv...)
}

// leerOrdenJaula es lo contrario de args (sin el argv[0]).
func leerOrdenJaula(a []string) (ordenJaula, error) {
	var o ordenJaula
	if len(a) < 7 || a[5] != "--" {
		return o, fmt.Errorf("malformed arguments %q", a)
	}
	o.modo, o.raiz, o.argv = a[0], a[4], a[6:]
	var err error
	for i, p := range []*int{&o.uid, &o.gid, &o.mem} {
		if *p, err = strconv.Atoi(a[1+i]); err != nil {
			return o, fmt.Errorf("malformed arguments %q", a)
		}
	}
	return o, nil
}

// ordenEnJaula dice qué orden y con qué usuario correrá c si confinarE2fs la
// encerró; ok=false si no.
func ordenEnJaula(c *exec.Cmd) (ordenJaula, bool) {
	if len(c.Args) == 0 || c.Args[0] != argJaulaE2fs {
		return ordenJaula{}, false
	}
	o, err := leerOrdenJaula(c.Args[1:])
	return o, err == nil
}

// usuarioConfinado es el usuario con que correrá la orden que encerró
// confinarE2fs, o nil si es el del daemon.
func usuarioConfinado(c *exec.Cmd) *syscall.Credential {
	o, ok := ordenEnJaula(c)
	if !ok || o.uid < 0 {
		return nil
	}
	return &syscall.Credential{Uid: uint32(o.uid), Gid: uint32(o.gid), Groups: []uint32{}}
}

// envolverEnJaula hace que c, en vez de su binario, ejecute el daemon como
// hijo de la jaula con o. Las Cloneflags las pone quien llama.
func envolverEnJaula(c *exec.Cmd, o ordenJaula) {
	o.argv = append([]string{c.Path}, c.Args[1:]...)
	c.Args = o.args()
	c.Path = "/proc/self/exe"
}

// nsJaula son los namespaces de la orden encerrada. Sin el de montajes en
// modo simple: si la jaula no se puede montar, crear el namespace puede ser
// lo que falla.
const (
	nsSinMontajes = syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC
	nsJaula       = nsSinMontajes | syscall.CLONE_NEWNS
)

// dirRaizJaula crea el directorio vacío (de root, 0700) sobre el que el hijo
// monta su raíz. En el namespace del host queda vacío: el tmpfs es del hijo.
func dirRaizJaula() (string, error) {
	return os.MkdirTemp("", "kling-e2fs-")
}

// sondaJaulaE2fs monta una vez la jaula entera, sin ejecutar nada en ella, y
// recuerda si se pudo. Variable para los tests.
var sondaJaulaE2fs = sync.OnceValue(func() error {
	err := probarJaula()
	if err != nil {
		log.Printf("warning: e2fsck, resize2fs and debugfs can't be confined in a chroot here (%v): "+
			"they run as nobody in their own network, PID and IPC namespaces but see the host's files; "+
			"see SECURITY.md", err)
	}
	return err
})

func probarJaula() error {
	raiz, err := dirRaizJaula()
	if err != nil {
		return err
	}
	defer os.Remove(raiz)
	c := exec.Command("/proc/self/exe")
	c.Args = ordenJaula{modo: modoSonda, uid: uidNadieE2fsck, gid: uidNadieE2fsck, mem: memE2fsck,
		raiz: raiz, argv: []string{"-"}}.args()
	c.Env = entornoE2fsck
	c.SysProcAttr = &syscall.SysProcAttr{Cloneflags: nsJaula}
	out, err := c.CombinedOutput()
	if err != nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return errors.New(strings.TrimPrefix(s, "kling: e2fsprogs confinement: "))
		}
		return err
	}
	return nil
}

// confinamientoE2fs dice cómo corren e2fsck y compañía en este daemon (para
// Ajustes y kling doctor).
func confinamientoE2fs() string {
	if os.Geteuid() != 0 {
		return "rlimit (daemon without root: same user, no namespaces)"
	}
	if err := sondaJaulaE2fs(); err != nil {
		return "nobody+namespaces, no chroot (" + err.Error() + ")"
	}
	return "chroot+nobody+namespaces"
}

// hijoJaulaE2fs es el hijo: se encierra según a y ejecuta la orden. Solo
// vuelve si algo falla.
func hijoJaulaE2fs(a []string) error {
	o, err := leerOrdenJaula(a)
	if err != nil {
		return err
	}
	// no_new_privs es de cada hilo y el exec lo hereda del que lo hace: los
	// dos en el mismo.
	runtime.LockOSThread()
	bin := o.argv[0]
	switch o.modo {
	case modoJaula, modoSonda:
		if o.modo == modoSonda {
			bin = ""
		}
		if bin, err = montarJaula(o.raiz, bin); err != nil {
			return err
		}
		if err := syscall.Chroot(o.raiz); err != nil {
			return fmt.Errorf("chroot: %w", err)
		}
		if err := syscall.Chdir("/"); err != nil {
			return err
		}
	case modoSimple:
	default:
		return fmt.Errorf("unknown mode %q", o.modo)
	}
	if o.uid >= 0 {
		if err := syscall.Setgroups([]int{}); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(o.gid); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(o.uid); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: uint64(o.mem), Max: uint64(o.mem)}); err != nil {
		return fmt.Errorf("RLIMIT_AS: %w", err)
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	if o.modo == modoSonda {
		// Lo que la orden va a necesitar: su disco por /dev/fd y el cargador.
		for _, p := range []string{"/dev/fd/0", "/usr"} {
			if _, err := os.Stat(p); err != nil {
				return err
			}
		}
		os.Exit(0)
	}
	return syscall.Exec(bin, o.argv, os.Environ())
}

// prSetNoNewPrivs es PR_SET_NO_NEW_PRIVS (linux/prctl.h): el paquete syscall
// no lo trae.
const prSetNoNewPrivs = 38

// montarJaula monta en raiz la raíz de la orden y devuelve la ruta de bin
// tal como se ve dentro. Corre en el hijo, con root y en su propio namespace
// de montajes: nada de esto se ve en el host.
func montarJaula(raiz, bin string) (string, error) {
	// Antes que nada: que ningún montaje se propague al host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return "", fmt.Errorf("making mounts private: %w", err)
	}
	if err := syscall.Mount("tmpfs", raiz, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=1m,mode=0755"); err != nil {
		return "", fmt.Errorf("tmpfs root: %w", err)
	}
	var montados []string
	for _, d := range dirsSistemaJaula {
		fi, err := os.Lstat(d)
		if err != nil {
			continue
		}
		dst := filepath.Join(raiz, d)
		if fi.Mode()&os.ModeSymlink != 0 {
			destino, err := os.Readlink(d)
			if err != nil {
				return "", err
			}
			if err := os.Symlink(destino, dst); err != nil {
				return "", err
			}
			continue
		}
		if !fi.IsDir() {
			continue
		}
		if err := enlazarSoloLectura(d, dst, true); err != nil {
			return "", err
		}
		montados = append(montados, d)
	}
	// El binario: si no está bajo lo de arriba (/usr/local/sbin sí; una
	// instalación en /opt no), su directorio también, en solo lectura. La
	// sonda no ejecuta nada (bin vacío).
	real := ""
	if bin != "" {
		var err error
		if real, err = filepath.EvalSymlinks(bin); err != nil {
			return "", fmt.Errorf("resolving %s: %w", bin, err)
		}
		if dir := filepath.Dir(real); !bajoAlguno(dir, montados) {
			if err := enlazarSoloLectura(dir, filepath.Join(raiz, dir), true); err != nil {
				return "", err
			}
		}
	}
	if err := os.Mkdir(filepath.Join(raiz, "etc"), 0o755); err != nil {
		return "", err
	}
	for _, f := range ficherosSistemaJaula {
		if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := enlazarSoloLectura(f, filepath.Join(raiz, f), false); err != nil {
			return "", err
		}
	}
	// e2fsck mira si el disco está montado: /proc/mounts, o /etc/mtab.
	if err := os.Symlink("/proc/self/mounts", filepath.Join(raiz, "etc", "mtab")); err != nil {
		return "", err
	}
	// Un /proc nuevo: con el namespace de PID propio solo se ve a sí mismo.
	// Es lo que da /dev/fd/3.
	if err := os.Mkdir(filepath.Join(raiz, "proc"), 0o555); err != nil {
		return "", err
	}
	if err := syscall.Mount("proc", filepath.Join(raiz, "proc"), "proc",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return "", fmt.Errorf("mounting /proc: %w", err)
	}
	if err := os.Mkdir(filepath.Join(raiz, "dev"), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink("/proc/self/fd", filepath.Join(raiz, "dev", "fd")); err != nil {
		return "", err
	}
	for _, d := range dispositivosJaula {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		dst := filepath.Join(raiz, d)
		if err := os.WriteFile(dst, nil, 0o644); err != nil {
			return "", err
		}
		// Sin MS_NODEV: es un dispositivo. Solo los que no dan nada.
		if err := syscall.Mount(d, dst, "", syscall.MS_BIND, ""); err != nil {
			return "", fmt.Errorf("bind %s: %w", d, err)
		}
	}
	if err := syscall.Mount("", raiz, "", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return "", fmt.Errorf("read-only root: %w", err)
	}
	return real, nil
}

// enlazarSoloLectura monta src en dst (que crea: directorio si dir, fichero
// vacío si no) en solo lectura, sin suid ni dispositivos.
func enlazarSoloLectura(src, dst string, dir bool) error {
	var err error
	if dir {
		err = os.MkdirAll(dst, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
		err = os.WriteFile(dst, nil, 0o644)
	}
	if err != nil {
		return err
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", src, err)
	}
	if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("read-only %s: %w", src, err)
	}
	return nil
}

// bajoAlguno dice si dir es uno de dirs o está debajo.
func bajoAlguno(dir string, dirs []string) bool {
	for _, d := range dirs {
		if dir == d || strings.HasPrefix(dir, d+"/") {
			return true
		}
	}
	return false
}
