package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// metodoClon es lo que da un clon que sale bien en esta plataforma.
const metodoClon = api.CloneClonefile

// clonarRuta clona src en dst (que no debe existir) con fclonefileat(2), sin
// lanzar `cp -c`: un fork+exec por disco en cada restore y cada fork. El clon
// es de quien lo crea (CLONE_NOOWNERCOPY), como la copia de cp sin -p, y
// conserva el modo del origen, como cp. El error es el de la llamada
// (ENOTSUP fuera de APFS, EXDEV entre volúmenes, EEXIST si dst ya existe): el
// llamante decide si cae a copia.
func clonarRuta(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dfd, err := syscall.Open(filepath.Dir(dst), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(dfd)
	return fclonefileat(int(in.Fd()), dfd, filepath.Base(dst), cloneNoOwnerCopy)
}

// clonNoSoportado dice si err de clonarRuta es que ahí no se puede clonar
// (otro sistema de ficheros, otro volumen), no un fallo de verdad.
func clonNoSoportado(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EXDEV)
}

// clonarVolFichero clona src en dst (que NO debe existir) con clonefile(2) de
// APFS (clonarRuta).
//
// Se comprueba antes lo que clonefile exige —APFS, y el mismo volumen para
// origen y destino— y si no se cumple se contesta errSinClon, igual que Linux
// con EOPNOTSUPP o EXDEV; si aun así la llamada dice ENOTSUP o EXDEV, también.
// Nunca cae a una copia: un "clon" que en realidad copió 50 GiB es justo el
// engaño que esta función existe para evitar (`cp -c`, que se usaba antes,
// sí caía en silencio).
func clonarVolFichero(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s: %w", dst, os.ErrExist)
	}
	var a, b syscall.Stat_t
	if err := syscall.Stat(src, &a); err != nil {
		return err
	}
	if err := syscall.Stat(filepath.Dir(dst), &b); err != nil {
		return err
	}
	if a.Dev != b.Dev {
		return fmt.Errorf("%w: source and destination are on different volumes", errSinClon)
	}
	if fs := tipoFS(src); fs != "apfs" {
		return fmt.Errorf("%w: %s does not support clonefile", errSinClon, fs)
	}
	if err := clonarRuta(src, dst); err != nil {
		if clonNoSoportado(err) {
			return fmt.Errorf("%w: clonefile: %w", errSinClon, err)
		}
		return fmt.Errorf("clonefile %s: %w", filepath.Base(src), err)
	}
	return nil
}

// tipoFS devuelve el nombre del sistema de ficheros de path (apfs, hfs…).
func tipoFS(path string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ""
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
