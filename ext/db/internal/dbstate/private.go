package dbstate

// Ficheros privados: los que no son una contraseña de copia pero tampoco
// debe leer nadie más (definiciones de informes, listas de claves de una
// clase, resultados de un informe).

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// EnsureDir crea el subdirectorio sub del estado (y el propio estado), 0700,
// y devuelve su ruta.
func EnsureDir(sub string) (string, error) {
	if !lockPattern.MatchString(sub) {
		return "", fmt.Errorf("invalid state subdirectory %q", sub)
	}
	root, err := Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, sub)
	for _, d := range []string{root, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// WritePrivate escribe data en path, 0600: en un fichero nuevo del mismo
// directorio (O_EXCL) que después se renombra. Nunca queda a medias, nunca con
// otros permisos y, si path era un enlace, se sustituye el enlace, no su
// destino.
func WritePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kling-db.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op tras el rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadPrivate lee path (hasta max bytes) con las exigencias de una
// contraseña: fichero normal (no un enlace), 0600 como mucho y de este
// usuario. fs.ErrNotExist si no está.
func ReadPrivate(path string, max int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %04o): chmod 600 it or remove it", path, st.Mode().Perm())
	}
	if err := ownedByUs(st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return b, nil
}

// IsNotExist dice si err es "no existe".
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
