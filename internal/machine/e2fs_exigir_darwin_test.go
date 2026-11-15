//go:build darwin

package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// daemon.require_e2fs_jail: sin sandbox-exec la orden no corre, aunque el
// daemon no sea root; por defecto se sigue como siempre.
func TestJaulaE2fsExigidaSinSandboxExec(t *testing.T) {
	antes := sandboxExec
	t.Cleanup(func() { sandboxExec = antes; FijarJaulaE2fsExigida(false) })
	sandboxExec = filepath.Join(t.TempDir(), "no-existe")
	f, err := os.Create(filepath.Join(t.TempDir(), "d.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	FijarJaulaE2fsExigida(true)
	_, err = confinarE2fs(exec.Command("/usr/bin/true"), f)
	if err == nil || !strings.Contains(err.Error(), "require_e2fs_jail") {
		t.Fatalf("con la jaula exigida y sin sandbox-exec: %v", err)
	}
	if got := confinamientoE2fs(); !strings.HasPrefix(got, "blocked") {
		t.Errorf("e2fs_confinement = %q; quería blocked", got)
	}
	if os.Geteuid() == 0 {
		return
	}
	FijarJaulaE2fsExigida(false)
	if _, err := confinarE2fs(exec.Command("/usr/bin/true"), f); err != nil {
		t.Fatalf("por defecto se sigue sin encierro: %v", err)
	}
}
