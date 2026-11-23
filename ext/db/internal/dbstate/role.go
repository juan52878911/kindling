package dbstate

// Contraseñas de los roles extra de una copia (`kling db role`): mismo
// directorio que la del rol de la aplicación, fichero <rol>.password, con las
// mismas exigencias (0600, normal, de este usuario, escrito y renombrado).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// rolePattern es un nombre de rol admitido (el mismo que valida kling db role).
var rolePattern = lazyre.New(`^[a-z_][a-z0-9_]{0,62}$`)

// RolePasswordPath es el fichero de la contraseña del rol de la copia con ese id.
func RolePasswordPath(id, role string) (string, error) {
	if !rolePattern.MatchString(role) {
		return "", fmt.Errorf("invalid role name %q", role)
	}
	d, err := CopyDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, role+".password"), nil
}

// WriteRolePassword guarda la contraseña de un rol de la copia.
func WriteRolePassword(id, role, password string) error {
	if password == "" || strings.ContainsAny(password, "\r\n") {
		return errors.New("refusing to store an empty or multi-line password")
	}
	final, err := RolePasswordPath(id, role)
	if err != nil {
		return err
	}
	dir := filepath.Dir(final)
	root, _ := Dir()
	for _, d := range []string{root, filepath.Join(root, CopiesDir), dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	tmp := final + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(password + "\n"); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}

// ReadRolePassword lee la contraseña del rol; ErrNoPassword si no hay fichero.
func ReadRolePassword(id, role string) (string, error) {
	p, err := RolePasswordPath(id, role)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoPassword
	}
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %04o): chmod 600 it or remove it", p, st.Mode().Perm())
	}
	if err := ownedByUs(st); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return pw, nil
}

// RemoveRolePassword borra la contraseña del rol. No existir no es un error.
func RemoveRolePassword(id, role string) error {
	p, err := RolePasswordPath(id, role)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RoleLogin es cómo estaba un rol antes de que `kling db role -login` le
// diera LOGIN y una clave de este host: lo que hay que devolverle al quitarlo
// (o si -login falla a medias). Since es la hora del invitado al darle la
// clave: las sesiones abiertas desde entonces por red son las de esa clave.
type RoleLogin struct {
	CanLogin bool   `json:"can_login"`
	Since    string `json:"since"`
}

// RoleLoginPath es el fichero del estado previo del rol: <rol>.login.
func RoleLoginPath(id, role string) (string, error) {
	p, err := RolePasswordPath(id, role)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".password") + ".login", nil
}

// WriteRoleLogin guarda el estado previo del rol (0600, como su clave).
func WriteRoleLogin(id, role string, st RoleLogin) error {
	p, err := RoleLoginPath(id, role)
	if err != nil {
		return err
	}
	root, _ := Dir()
	for _, d := range []string{root, filepath.Join(root, CopiesDir), filepath.Dir(p)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return WritePrivate(p, b)
}

// ReadRoleLogin lee el estado previo del rol; fs.ErrNotExist si no hay (un
// -login de una versión anterior no lo guardaba).
func ReadRoleLogin(id, role string) (RoleLogin, error) {
	var st RoleLogin
	p, err := RoleLoginPath(id, role)
	if err != nil {
		return st, err
	}
	b, err := ReadPrivate(p, 16<<10)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", p, err)
	}
	return st, nil
}

// RemoveRoleLogin borra el estado previo del rol. No existir no es un error.
func RemoveRoleLogin(id, role string) error {
	p, err := RoleLoginPath(id, role)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
