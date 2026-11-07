package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// geteuid es os.Geteuid; los tests lo cambian para probar el camino de root.
var geteuid = os.Geteuid

// bloquear toma el cerrojo de dir/.lock para esta actualización o vuelta
// atrás, sin esperar. Dos `kling upgrade` sobre la misma raíz (dos personas,
// o un script y una persona) comparten el directorio de descarga, que cada uno
// borra al acabar, y paran y arrancan el mismo servicio: uno borraría lo que
// baja el otro o volvería atrás encima de su actualización. El kernel suelta
// el cerrojo si el proceso muere, también con un SIGKILL.
func bloquear(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if geteuid() == 0 {
		if err := dirSoloDe(dir, 0); err != nil {
			return nil, err
		}
	}
	ruta := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(ruta, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another kling upgrade is running on %s (%s is locked): wait for it to finish", dir, ruta)
		}
		return nil, fmt.Errorf("locking %s: %w", ruta, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// dirSoloDe comprueba que nadie más que uid (y root) pueda cambiar lo que hay
// en dir: dir y cada directorio por encima son de uid o de root y no los
// puede escribir el grupo ni otros, salvo con el bit pegajoso (/tmp), que no
// deja renombrar ni borrar lo ajeno. Como root es lo que separa la descarga
// verificada de lo que se instala: bajo sudo, macOS conserva HOME, y un
// ~/.local/state del usuario le dejaría cambiar los binarios en medio, o las
// copias que instala un -rollback.
func dirSoloDe(dir string, uid int) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return err
	}
	for d := real; ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: cannot read its owner", d)
		}
		if int(st.Uid) != uid && st.Uid != 0 {
			return fmt.Errorf("refusing %s for the upgrade's files: %s is owned by uid %d, not %d", dir, d, st.Uid, uid)
		}
		if fi.Mode().Perm()&0o022 != 0 && (d == real || fi.Mode()&os.ModeSticky == 0) {
			return fmt.Errorf("refusing %s for the upgrade's files: %s is writable by group or others (%v)", dir, d, fi.Mode().Perm())
		}
		if p := filepath.Dir(d); p == d {
			return nil
		}
	}
}
