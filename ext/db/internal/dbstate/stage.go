package dbstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// StagePassword deja la contraseña nueva en password.new (0600) SIN tocar la
// vigente. Es la primera mitad de WritePassword: kling db rotate la usa para
// cambiar la clave de la base y solo después convertir el fichero en el
// definitivo (CommitStaged); si el cambio falla, DiscardStaged y la anterior
// sigue intacta.
func StagePassword(id, password string) error {
	if password == "" || strings.ContainsAny(password, "\r\n") {
		return errors.New("refusing to store an empty or multi-line password")
	}
	dir, err := CopyDir(id)
	if err != nil {
		return err
	}
	root, _ := Dir()
	for _, d := range []string{root, filepath.Join(root, CopiesDir), dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	tmp := filepath.Join(dir, "password.new")
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
	return nil
}

// CommitStaged convierte password.new en la contraseña vigente (rename atómico).
func CommitStaged(id string) error {
	dir, err := CopyDir(id)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "password.new"), filepath.Join(dir, "password"))
}

// DiscardStaged borra la contraseña a medias. No existir no es un error.
func DiscardStaged(id string) {
	if dir, err := CopyDir(id); err == nil {
		_ = os.Remove(filepath.Join(dir, "password.new"))
	}
}
