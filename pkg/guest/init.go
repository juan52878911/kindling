package guest

// El init en Go: el /sbin/overlay-init de las imágenes de Docker sin sh.
//
// Hace lo mismo que scripts/minimal-init.sh, paso a paso y con los mismos
// fallos tolerados, pero sin pedir nada a la imagen: ni sh, ni mount, ni
// pivot_root. Es lo que deja importar una imagen distroless, una scratch con
// un binario estático o traefik/whoami. El constructor oci lo pone cuando a la
// imagen le faltan las herramientas del script (cmd/kling/builder_oci.go):
// /sbin/overlay-init es entonces un enlace a /usr/local/bin/kling-guest, y
// kling-guest, al verse llamado así, hace de init (cmd/kling-guest).
//
// Al acabar no ejecuta /entrypoint, que es un script de sh: hace lo que hace
// el /entrypoint del constructor (internal/imagen Entrypoint), cargar
// /etc/kling/env encima de PATH y HOME y ceder el PID 1 al agente, con un
// exec y no en el mismo proceso, para que el agente arranque limpio.
//
// Las llamadas al sistema van por initSys, y así el plan entero (qué se monta,
// dónde y en qué orden; qué se escribe en /etc/hosts) se prueba con un sistema
// de ficheros falso en cualquier plataforma (init_test.go).

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// Los MS_* de <sys/mount.h>, aquí y no de syscall: el paquete compila también
// en macOS, donde syscall no los tiene.
const (
	msRdonly = 0x1
	msNosuid = 0x2
	msNodev  = 0x4
)

const (
	// initPATH y initHOME son los del script y los del /entrypoint: el kernel
	// arranca el PID 1 sin entorno.
	initPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	initHOME = "/root"
	// guestAgentPath es donde el constructor mete el agente.
	guestAgentPath = "/usr/local/bin/kling-guest"
	// guestEnvPath es imagen.EnvPath: las variables de la imagen.
	guestEnvPath = "/etc/kling/env"
	// ociMarker es lo que deja el constructor oci en toda imagen de Docker.
	ociMarker = "/etc/kindling/oci.json"
)

// initSys son las llamadas al sistema del init.
type initSys interface {
	Mount(src, target, fstype string, flags uintptr, data string) error
	Unmount(target string) error
	MkdirAll(p string, perm fs.FileMode) error
	PivotRoot(newRoot, putOld string) error
	Chdir(dir string) error
	ReadFile(p string) ([]byte, error)
	// WriteFile sustituye el contenido; AppendFile añade, creándolo 0644.
	WriteFile(p string, b []byte) error
	AppendFile(p string, b []byte) error
	// Stat y Lstat dicen si p existe, siguiendo o no el último enlace.
	Stat(p string) error
	Lstat(p string) error
	Symlink(target, p string) error
	Exec(path string, argv, env []string) error
}

// Init hace de PID 1 y no vuelve: acaba en el agente o, si algo que el
// script no toleraba falla, sale con un error en la consola (y el kernel entra
// en pánico, como con el script y su set -e).
func Init() {
	log.SetFlags(0)
	log.SetPrefix("kling-guest init: ")
	if os.Getpid() != 1 {
		// Montar y hacer pivot_root fuera de una microVM sería tocar el anfitrión.
		log.Fatal("refusing to run: the init must be PID 1 inside a microVM")
	}
	s, err := osInitSys()
	if err == nil {
		err = runInit(s, os.Environ())
	}
	log.Fatal(err)
}

// runInit es minimal-init.sh. Solo vuelve si algo falla.
func runInit(s initSys, environ []string) error {
	must := func(err error, what string) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	if err := must(s.Mount("proc", "/proc", "proc", 0, ""), "mounting /proc"); err != nil {
		return err
	}
	if err := must(s.Mount("sysfs", "/sys", "sysfs", 0, ""), "mounting /sys"); err != nil {
		return err
	}
	_ = s.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")

	dev := "/dev/vdb"
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "OVERLAY_DEV="); ok && v != "" {
			dev = v
		}
	}
	if err := must(s.MkdirAll("/overlay", 0o755), "creating /overlay"); err != nil {
		return err
	}
	if err := must(s.Mount(dev, "/overlay", "ext4", 0, ""), "mounting the overlay disk "+dev); err != nil {
		return err
	}
	for _, d := range []string{"/overlay/upper", "/overlay/work", "/overlay/merged"} {
		if err := must(s.MkdirAll(d, 0o755), "creating "+d); err != nil {
			return err
		}
	}

	// La capa de servicio, si el anfitrión engancha una (kling.layer): de
	// lower por delante de la base, como en el script.
	lower := "/"
	cmdline, _ := s.ReadFile("/proc/cmdline")
	if layer := initLayerDev(string(cmdline)); layer != "" {
		if err := must(s.MkdirAll("/overlay/svc", 0o755), "creating /overlay/svc"); err != nil {
			return err
		}
		if err := must(s.Mount(layer, "/overlay/svc", "ext4", msRdonly, ""), "mounting the service layer "+layer); err != nil {
			return err
		}
		lower = "/overlay/svc/upper:/"
	}
	if err := must(s.Mount("overlay", "/overlay/merged", "overlay", 0,
		"lowerdir="+lower+",upperdir=/overlay/upper,workdir=/overlay/work"), "mounting the overlay"); err != nil {
		return err
	}
	if err := must(s.MkdirAll("/overlay/merged/rom", 0o755), "creating /overlay/merged/rom"); err != nil {
		return err
	}
	if err := must(s.Chdir("/overlay/merged"), "entering the overlay"); err != nil {
		return err
	}
	if err := must(s.PivotRoot(".", "rom"), "pivot_root"); err != nil {
		return err
	}
	_ = s.Chdir("/")

	// Dentro de la nueva raíz, todo tolera fallos, como en el script.
	for _, d := range []string{"/proc", "/sys", "/dev", "/tmp", "/run"} {
		_ = s.MkdirAll(d, 0o755)
	}
	_ = s.Mount("proc", "/proc", "proc", 0, "")
	_ = s.Mount("sysfs", "/sys", "sysfs", 0, "")
	_ = s.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	// /tmp y /run: los de la imagen en una de Docker; tmpfs en las bases.
	if s.Stat(ociMarker) != nil {
		_ = s.Mount("tmpfs", "/tmp", "tmpfs", 0, "")
		_ = s.Mount("tmpfs", "/run", "tmpfs", 0, "")
	}
	for _, d := range []string{"/rom/proc", "/rom/sys", "/rom/dev"} {
		_ = s.Unmount(d)
	}

	// El contrato de runtime de Docker: /dev/fd, /dev/std*, /dev/shm.
	for _, l := range [][2]string{{"fd", "/proc/self/fd"}, {"stdin", "/proc/self/fd/0"},
		{"stdout", "/proc/self/fd/1"}, {"stderr", "/proc/self/fd/2"}} {
		if p := "/dev/" + l[0]; s.Stat(p) != nil && s.Lstat(p) != nil {
			_ = s.Symlink(l[1], p)
		}
	}
	if s.MkdirAll("/dev/shm", 0o755) == nil {
		_ = s.Mount("tmpfs", "/dev/shm", "tmpfs", msNosuid|msNodev, "mode=1777")
	}

	hn := initHostname(s)
	initHosts(s, hn)

	env := initEnv(s, environ)
	argv := []string{guestAgentPath, "-listen", ":8080"}
	return must(s.Exec(guestAgentPath, argv, env), "starting "+guestAgentPath)
}

// initLayerDev es el valor de kling.layer en la línea de comandos del kernel
// ("" si no está): por palabras y entera, como el script.
func initLayerDev(cmdline string) string {
	for _, tok := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(tok, api.LayerBootParam+"="); ok {
			return v
		}
	}
	return ""
}

// initHostname es el nombre de la máquina: el que fije el kernel
// (ip=...:<nombre>:...) o, si no hay uno de verdad (vacío, "(none)" o una
// IPv4), "kindling", que se escribe en el kernel.
func initHostname(s initSys) string {
	b, _ := s.ReadFile("/proc/sys/kernel/hostname")
	hn, _, _ := strings.Cut(string(b), "\n")
	hn = strings.TrimSpace(hn)
	if hn == "(none)" || strings.Trim(hn, "0123456789.") == "" {
		hn = "kindling"
		_ = s.WriteFile("/proc/sys/kernel/hostname", []byte(hn+"\n"))
	}
	return hn
}

// initHosts deja en /etc/hosts "127.0.0.1 localhost" y "127.0.1.1 <hn>" si
// no están, sin tocar lo que haya (ver el script: solo IPv4, el nombre en
// 127.0.1.1 como en Debian). Una línea final sin \n se cierra antes de añadir.
func initHosts(s initSys, hn string) {
	b, _ := s.ReadFile("/etc/hosts")
	hasLH, hasHN := false, false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		ip := f[0]
		// Los nombres hasta el primer "#", como ${names%%#*}.
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " \t"), ip))
		rest, _, _ = strings.Cut(rest, "#")
		for _, n := range strings.Fields(rest) {
			if n == "localhost" && ip == "127.0.0.1" {
				hasLH = true
			}
			if n == hn {
				hasHN = true
			}
		}
	}
	if hasLH && hasHN {
		return
	}
	var add strings.Builder
	if len(b) > 0 && b[len(b)-1] != '\n' && strings.TrimSpace(lastLine(string(b))) != "" {
		add.WriteString("\n")
	}
	if !hasLH {
		add.WriteString("127.0.0.1\tlocalhost\n")
	}
	if !hasHN {
		add.WriteString("127.0.1.1\t" + hn + "\n")
	}
	_ = s.AppendFile("/etc/hosts", []byte(add.String()))
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// initEnv es el entorno del agente: el del kernel con PATH y HOME, y encima
// las variables de la imagen (/etc/kling/env), como hace el /entrypoint.
func initEnv(s initSys, environ []string) []string {
	env := setEnv(setEnv(append([]string{}, environ...), "PATH", initPATH), "HOME", initHOME)
	b, err := s.ReadFile(guestEnvPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("warning: %s: %v", guestEnvPath, err)
		}
		return env
	}
	vars, bad := parseEnvFile(string(b))
	for _, n := range bad {
		// Solo el número de línea: el valor puede ser un secreto.
		log.Printf("warning: %s: line %d is not an export KEY='value'; ignored", guestEnvPath, n)
	}
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		env = setEnv(env, k, v)
	}
	return env
}

// parseEnvFile lee lo que escribe imagen.EnvFile: líneas
// `export KEY='valor'` con las comillas simples de sh (imagen.SQ: una comilla
// dentro del valor cierra el tramo, va escapada con \ y abre otro). Un valor
// con saltos de línea (un ENV de la imagen) sigue entre comillas en las
// líneas siguientes, como lo lee sh. Devuelve las variables (KEY=valor) y los
// números de las líneas (donde empieza cada una) que no entiende. No es un
// intérprete de sh: solo lo que se puede escribir ahí.
func parseEnvFile(s string) (vars []string, bad []int) {
	for _, r := range envRecords(s) {
		t := strings.TrimSpace(r.text)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(t, "export "), "=")
		val, okv := shUnquote(v)
		if !ok || !okv || !validEnvKey(k) {
			bad = append(bad, r.line)
			continue
		}
		vars = append(vars, k+"="+val)
	}
	return vars, bad
}

// envRecord es una orden del fichero de entorno y la línea donde empieza.
type envRecord struct {
	line int
	text string
}

// envRecords parte s en órdenes como sh: por saltos de línea fuera de
// comillas simples (y no escapados con \); un comentario llega hasta el
// final de su línea. Una comilla sin cerrar se queda en la última, que
// shUnquote rechaza.
func envRecords(s string) []envRecord {
	var out []envRecord
	start, line, startLine := 0, 1, 1
	inQuote, comment, empty := false, false, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n':
			line++
			if !inQuote {
				out = append(out, envRecord{startLine, s[start:i]})
				start, startLine, comment, empty = i+1, line, false, true
			}
		case comment || inQuote && c != '\'':
		case c == '\'':
			inQuote = !inQuote
			empty = false
		case c == '\\' && i+1 < len(s) && s[i+1] != '\n':
			i++
			empty = false
		case c == '#' && empty:
			comment = true
		case c != ' ' && c != '\t':
			empty = false
		}
	}
	if start < len(s) {
		out = append(out, envRecord{startLine, s[start:]})
	}
	return out
}

func validEnvKey(k string) bool {
	if k == "" || (k[0] >= '0' && k[0] <= '9') {
		return false
	}
	for _, c := range k {
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// shUnquote quita las comillas de una palabra de sh hecha de tramos entre
// comillas simples y caracteres escapados con \ (lo que produce imagen.SQ), o
// de una palabra sin comillas ni espacios.
func shUnquote(w string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(w); i++ {
		switch c := w[i]; c {
		case '\'':
			j := strings.IndexByte(w[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			out.WriteString(w[i+1 : i+1+j])
			i += j + 1
		case '\\':
			if i+1 >= len(w) {
				return "", false
			}
			i++
			out.WriteByte(w[i])
		case ' ', '\t', '"', '$', '`', ';', '&', '|', '<', '>', '(', ')':
			return "", false
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), true
}
