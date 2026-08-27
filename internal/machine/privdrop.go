package machine

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
}

// resolvePrivileges busca el usuario de servicio. Si no existe, se sigue
// corriendo como root pero avisando: preferimos funcionar a fallar en silencio.
func resolvePrivileges(username string) (*Privileges, string) {
	if username == "" || os.Geteuid() != 0 {
		return &Privileges{}, ""
	}
	u, err := user.Lookup(username)
	if err != nil {
		return &Privileges{}, fmt.Sprintf("user %q does not exist: Firecracker will run as root", username)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	kvmGid := -1
	if g, err := user.LookupGroup("kvm"); err == nil {
		kvmGid, _ = strconv.Atoi(g.Gid)
	}
	if kvmGid < 0 {
		return &Privileges{}, "can't find kvm group: Firecracker will run as root"
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

// EnsureReadable deja las imágenes accesibles en solo lectura para el VMM —y
// SOLO para el VMM.
//
// Antes esto era `chmod -R a+rX`, que es una forma cómoda de decir "legible para
// todo el mundo". Y aquí dentro puede haber secretos: `kling add -env` los hornea
// como líneas `export CLAVE=valor` en el /entrypoint de la imagen, así que
// cualquier cuenta del anfitrión los sacaba con `strings *.layer.ext4 | grep
// export`, sin root y sin montar nada. La incoherencia saltaba a la vista: la
// receta con esos mismos valores se guarda 0600 y la imagen quedaba 0644.
//
// El VMM corre como el usuario de servicio, así que basta con dárselo a él.
func (p *Privileges) EnsureReadable(dir string) {
	if !p.Enabled {
		return
	}
	_ = filepath.Walk(dir, func(ruta string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil {
			return nil // un fichero que desaparece a mitad no es motivo de parada
		}
		_ = os.Chown(ruta, p.UID, p.GID)
		if fi.IsDir() {
			_ = os.Chmod(ruta, 0o750)
		} else if fi.Mode().IsRegular() {
			_ = os.Chmod(ruta, 0o640)
		}
		return nil
	})
}
