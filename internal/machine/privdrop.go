package machine

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Privileges describe el usuario sin privilegios con el que corre Firecracker.
//
// El daemon necesita root para crear namespaces y dispositivos TAP, pero el VMM
// no: si un invitado hostil llegara a escapar de Firecracker, aterrizaría en un
// UID sin permisos en vez de en root. Es la diferencia entre un incidente y una
// máquina comprometida.
type Privileges struct {
	UID, GID int
	KVMGid   int
	Enabled  bool
	// Motivo, con Enabled en falso, es la causa REAL de que no haya usuario
	// sin privilegios (daemon sin root, -run-as vacío, usuario inexistente,
	// sin grupo kvm), en inglés y con el arreglo. Lo usa el mensaje de
	// bloqueo de jailer (ver decidirJailer): decir "el usuario no existe"
	// cuando el problema es que el daemon no es root despista.
	Motivo string
}

// geteuid y lookupGroup son variables para que los tests simulen un daemon
// sin root o un host sin grupo kvm.
var (
	geteuid     = os.Geteuid
	lookupGroup = user.LookupGroup
)

// resolvePrivileges busca el usuario de servicio. Si no existe, se sigue
// corriendo como root pero avisando: preferimos funcionar a fallar en silencio.
func resolvePrivileges(username string) (*Privileges, string) {
	if username == "" {
		return &Privileges{Motivo: "no unprivileged user is configured (-run-as/KLING_RUN_AS " +
			"is empty; set it to the service user, e.g. kindling)"}, ""
	}
	if geteuid() != 0 {
		return &Privileges{Motivo: "the daemon isn't running as root, so it can't run " +
			"Firecracker as an unprivileged user (start the daemon as root)"}, ""
	}
	u, err := user.Lookup(username)
	if err != nil {
		motivo := fmt.Sprintf("the unprivileged user %q doesn't exist (sudo useradd --system "+
			"--no-create-home --shell /usr/sbin/nologin %s && sudo usermod -aG kvm %s, or pass "+
			"-run-as/KLING_RUN_AS if it's named differently)", username, username, username)
		return &Privileges{Motivo: motivo}, fmt.Sprintf("user %q does not exist: Firecracker will run as root", username)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	kvmGid := -1
	if g, err := lookupGroup("kvm"); err == nil {
		kvmGid, _ = strconv.Atoi(g.Gid)
	}
	if kvmGid < 0 {
		motivo := "the host has no kvm group, so the unprivileged user can't open /dev/kvm " +
			"(load the kvm module so /dev/kvm exists with group kvm, then sudo usermod -aG kvm " +
			username + ")"
		return &Privileges{Motivo: motivo}, "can't find kvm group: Firecracker will run as root"
	}
	return &Privileges{UID: uid, GID: gid, KVMGid: kvmGid, Enabled: true}, ""
}

// Wrap antepone la bajada de privilegios al comando de Firecracker.
//
// --groups fija la lista EXACTA de grupos suplementarios: solo kvm, que es lo
// imprescindible para abrir /dev/kvm. (No se combina con --clear-groups: setpriv
// las considera mutuamente excluyentes.) --inh-caps y --no-new-privs impiden
// heredar o recuperar capacidades del daemon.
func (p *Privileges) Wrap(argv []string) []string {
	if !p.Enabled {
		return argv
	}
	return append([]string{
		"setpriv",
		"--reuid", strconv.Itoa(p.UID),
		"--regid", strconv.Itoa(p.GID),
		"--groups", strconv.Itoa(p.KVMGid),
		"--inh-caps=-all",
		"--no-new-privs",
	}, argv...)
}

// Own cede a Firecracker lo que necesita escribir, y nada más.
func (p *Privileges) Own(paths ...string) error {
	if !p.Enabled {
		return nil
	}
	for _, path := range paths {
		if err := os.Chown(path, p.UID, p.GID); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("granting %s: %w", path, err)
		}
	}
	return nil
}

// OwnFile cede a Firecracker un fichero ya abierto, por su descriptor
// (fchown): para ficheros en directorios que el VMM puede tocar, donde un
// chown por ruta seguiría un enlace que el VMM haya plantado.
func (p *Privileges) OwnFile(f *os.File) error {
	if !p.Enabled {
		return nil
	}
	if err := f.Chown(p.UID, p.GID); err != nil {
		return fmt.Errorf("granting %s: %w", f.Name(), err)
	}
	return nil
}

// EnsureReadable deja un árbol de activos de SOLO LECTURA para el VMM (kernel,
// imágenes, capas, snapshots dorados): dueño root, grupo el del VMM, 0640 y
// directorios 0750. El VMM los lee por grupo y no puede reescribirlos.
//
// Antes esto era `chmod -R a+rX`, que es una forma cómoda de decir "legible para
// todo el mundo". Y aquí dentro puede haber secretos: `kling add -env` los hornea
// como líneas `export CLAVE=valor` en la capa de la imagen (/etc/kling/env), así que
// cualquier cuenta del anfitrión los sacaba con `strings *.layer.ext4 | grep
// export`, sin root y sin montar nada. Después fue dueño = usuario del VMM, que
// es lectura Y escritura para él: un Firecracker comprometido podía reescribir
// el vmlinux, la base o el mem.file de un dorado, y con eso todas las microVMs
// futuras del host. Con dueño root, no.
//
// Lchown y solo sobre ficheros y directorios: un enlace simbólico plantado en
// el árbol no puede llevar el chown a un fichero de fuera.
func (p *Privileges) EnsureReadable(dir string) {
	if !p.Enabled {
		return
	}
	p.recorrer(dir, 0)
}

// EnsureWritable es EnsureReadable para datos que el VMM sí escribe (los
// volúmenes): el dueño es el usuario del VMM.
func (p *Privileges) EnsureWritable(dir string) {
	if !p.Enabled {
		return
	}
	p.recorrer(dir, p.UID)
}

func (p *Privileges) recorrer(dir string, uid int) {
	_ = filepath.Walk(dir, func(ruta string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil {
			return nil // un fichero que desaparece a mitad no es motivo de parada
		}
		switch {
		case fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), ".recipe.json"):
			// La receta puede llevar secretos (kling add -env) y el VMM no la
			// lee nunca: solo root.
			_ = os.Lchown(ruta, 0, 0)
			_ = os.Chmod(ruta, 0o600)
		case fi.IsDir():
			_ = os.Lchown(ruta, uid, p.GID)
			_ = os.Chmod(ruta, 0o750)
		case fi.Mode().IsRegular():
			_ = os.Lchown(ruta, uid, p.GID)
			_ = os.Chmod(ruta, 0o640)
		}
		return nil
	})
}

// EnsureImageReadable deja legibles para el VMM los ficheros de una imagen
// recién construida (la imagen o su capa), sin tocar la receta, que puede llevar
// secretos. Lo llama el daemon tras cada construcción: un constructor no tiene
// por qué saber con qué usuario corre Firecracker, y sin esto la imagen se
// construye bien y luego no arranca por "permission denied". Dueño root: ver
// EnsureReadable.
func (m *Manager) EnsureImageReadable(name string) {
	if !m.priv.Enabled || !validName.MatchString(name) {
		return
	}
	for _, p := range []string{m.imagePath(name), m.layerPath(name)} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			_ = os.Lchown(p, 0, m.priv.GID)
			_ = os.Chmod(p, 0o640)
		}
	}
}

// restringirRaiz cierra los directorios de datos a los demás usuarios del
// host: 0750 con el grupo del VMM (0700 si no hay usuario de servicio) para
// machines/, snapshots/, volumes/ e images/, y 0700 para jails/ (el VMM entra
// ya dentro de su chroot y no los recorre). Antes eran 0755: cualquier cuenta
// local leía el mem.file de una máquina congelada —la RAM del invitado, con lo
// que tuviera dentro— porque Firecracker lo crea 0644.
func restringirRaiz(root string, p *Privileges) {
	modo, gid := os.FileMode(0o700), 0
	if p.Enabled {
		modo, gid = 0o750, p.GID
	}
	for _, d := range []string{"machines", "snapshots", "volumes", "images"} {
		ruta := filepath.Join(root, d)
		if fi, err := os.Lstat(ruta); err == nil && fi.IsDir() {
			_ = os.Lchown(ruta, 0, gid)
			_ = os.Chmod(ruta, modo)
		}
	}
	if fi, err := os.Lstat(filepath.Join(root, "jails")); err == nil && fi.IsDir() {
		_ = os.Chmod(filepath.Join(root, "jails"), 0o700)
	}
}

// cerrarVolcado quita la lectura a los demás de un volcado recién escrito
// (mem.file y snap.file): Firecracker los crea 0644. Sin tocar el dueño.
func cerrarVolcado(dir string) {
	for _, f := range []string{"mem.file", "snap.file"} {
		ruta := filepath.Join(dir, f)
		if fi, err := os.Lstat(ruta); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o007 != 0 {
			_ = os.Chmod(ruta, fi.Mode().Perm()&^0o007)
		}
	}
}

// cerrarVolcadosExistentes aplica cerrarVolcado a las máquinas y dorados que ya
// estaban en disco al arrancar el daemon (los de versiones anteriores).
func cerrarVolcadosExistentes(root string) {
	for _, d := range []string{"machines", "snapshots"} {
		entradas, _ := os.ReadDir(filepath.Join(root, d))
		for _, e := range entradas {
			if e.IsDir() {
				cerrarVolcado(filepath.Join(root, d, e.Name()))
			}
		}
	}
}
