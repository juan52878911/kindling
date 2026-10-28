//go:build unix

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// trustedExecutable dice si se puede ejecutar el kling-* path, hallado en dir:
// dir, el binario (siguiendo enlaces, como exec) y el directorio donde de verdad
// está tienen que ser del usuario o de root y no tener escritura para grupo ni
// otros. Es lo que ya se le exige a la caché de manifiestos: si otro usuario
// pudo ponerlo o cambiarlo, ejecutarlo en cada `kling …` sería ejecutar su
// código con los permisos de quien lanza kling.
func trustedExecutable(dir, path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	check := []string{filepath.Clean(dir), real}
	if rd := filepath.Dir(real); rd != check[0] {
		check = append(check, rd)
	}
	for _, p := range check {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot tell who owns %s", p)
		}
		if st.Uid != 0 && int(st.Uid) != os.Getuid() {
			return fmt.Errorf("%s belongs to uid %d, not to you or root", p, st.Uid)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by group or others (mode %04o)", p, fi.Mode().Perm())
		}
	}
	return nil
}
