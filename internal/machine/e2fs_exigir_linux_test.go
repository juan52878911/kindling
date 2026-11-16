//go:build linux

package machine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// daemon.require_e2fs_jail: si la jaula no se puede montar (o el daemon no
// es root), la orden no corre y el disco no se cede a nadie; por defecto se
// sigue sin chroot, como siempre.
func TestJaulaE2fsExigida(t *testing.T) {
	sondaE2fsDePrueba(t, func() error { return errors.New("mount proc: permission denied") }, time.Hour)
	t.Cleanup(func() { FijarJaulaE2fsExigida(false) })
	f, err := os.Create(filepath.Join(t.TempDir(), "d.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dueño := func() uint32 {
		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Uid
	}
	antes := dueño()

	FijarJaulaE2fsExigida(true)
	_, err = confinarE2fs(exec.Command("/bin/true"), f)
	if err == nil || !strings.Contains(err.Error(), "require_e2fs_jail") {
		t.Fatalf("con la jaula exigida y sin jaula: %v", err)
	}
	if dueño() != antes {
		t.Fatal("se cedió el disco a una orden que no iba a correr")
	}
	if got := confinamientoE2fs(); !strings.HasPrefix(got, "blocked") {
		t.Errorf("e2fs_confinement = %q; quería blocked", got)
	}

	FijarJaulaE2fsExigida(false)
	deshacer, err := confinarE2fs(exec.Command("/bin/true"), f)
	if err != nil {
		t.Fatalf("por defecto se sigue sin chroot: %v", err)
	}
	deshacer()
}
