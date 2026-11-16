package machine

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
)

// jaulaE2fsExigida es daemon.require_e2fs_jail (FijarJaulaE2fsExigida). El
// cero es lo de siempre: sin jaula se avisa y se sigue con el encierro que
// haya.
var jaulaE2fsExigida atomic.Bool

// FijarJaulaE2fsExigida dice si e2fsck, resize2fs y debugfs se niegan a
// correr sin su jaula entera (daemon.require_e2fs_jail; por defecto no): en
// Linux, el chroot de e2fs_jaula_linux.go con el daemon como root; en macOS,
// sandbox-exec. El daemon lo llama al arrancar, antes de crear el manager.
func FijarJaulaE2fsExigida(exigir bool) { jaulaE2fsExigida.Store(exigir) }

// errSinJaulaE2fs es el error de una orden que no corre porque se exige la
// jaula y no la hay (por qué, en motivo).
func errSinJaulaE2fs(bin, motivo string) error {
	return fmt.Errorf("refusing to run %s without its jail (daemon.require_e2fs_jail is on): %s",
		filepath.Base(bin), motivo)
}
