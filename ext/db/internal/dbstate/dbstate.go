// Package dbstate es lo que `kling db` guarda en el host: la contraseña de cada
// copia, y nada más. La contraseña no existe en ningún otro sitio (el invitado
// solo tiene su verificador SCRAM), así que este fichero es la credencial.
//
//	$KLING_DB_STATE (por defecto ~/.local/state/kling-db)   0700
//	├── <plantilla>/password, conn.env      los de scripts/db-golden.sh
//	└── copies/<id de la máquina>/password  0600, uno por copia
//
// Va por ID y no por nombre a propósito: un nombre se reutiliza (reset, rm y
// run con el mismo nombre) y el fichero de una copia no puede valer para otra.
// Va bajo copies/ para no chocar con el directorio de una plantilla: un nombre
// de plantilla puede ser 16 cifras hexadecimales, como un id. Una plantilla
// llamada "copies" tampoco choca: sus ficheros quedarían junto a los
// directorios <id>, no dentro.
package dbstate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CopiesDir es el subdirectorio de las copias.
const CopiesDir = "copies"

// idPattern es un id de máquina de kindling (8 bytes en hex; se admite más
// largo por si crece). Es lo único que entra en una ruta.
var idPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// Dir es la raíz del estado.
func Dir() (string, error) {
	if d := os.Getenv("KLING_DB_STATE"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory for the kling db state: %w (set KLING_DB_STATE)", err)
	}
	return filepath.Join(home, ".local", "state", "kling-db"), nil
}

// CopyDir es el directorio de la copia con ese id.
func CopyDir(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid machine id %q", id)
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, CopiesDir, id), nil
}

// PasswordPath es el fichero de la contraseña de la copia con ese id.
func PasswordPath(id string) (string, error) {
	d, err := CopyDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "password"), nil
}

// WritePassword guarda la contraseña de la copia: directorios 0700, fichero
// 0600, escrito aparte y renombrado para que nunca quede a medias.
func WritePassword(id, password string) error {
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
	return os.Rename(tmp, filepath.Join(dir, "password"))
}

// ErrNoPassword: la copia no tiene contraseña en este host (no la preparó
// `kling db`, la preparó otro host, o no terminó de prepararse).
var ErrNoPassword = errors.New("no password for this copy on this host")

// ReadPassword lee la contraseña de la copia y exige que el fichero sea
// normal (no un enlace), 0600 como mucho y de este usuario.
func ReadPassword(id string) (string, error) {
	p, err := PasswordPath(id)
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

// HasPassword dice si la copia tiene contraseña válida en este host.
func HasPassword(id string) error {
	_, err := ReadPassword(id)
	return err
}

// Remove borra lo que haya de la copia. No existir no es un error.
func Remove(id string) error {
	dir, err := CopyDir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
