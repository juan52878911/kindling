//go:build darwin

package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// M6: sin sandbox-exec la orden no se encierra, pero ya no en silencio:
// Ajustes lo dice, y con el daemon como root se niega.
func TestConfinarE2fsSinSandboxExec(t *testing.T) {
	antes := sandboxExec
	t.Cleanup(func() { sandboxExec = antes })
	sandboxExec = filepath.Join(t.TempDir(), "no-existe")
	f, err := os.Create(filepath.Join(t.TempDir(), "d.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := exec.Command("/usr/bin/true")
	_, err = confinarE2fs(c, f)
	if os.Geteuid() == 0 {
		if err == nil {
			t.Fatal("como root y sin sandbox-exec, se dejó correr sin encierro")
		}
	} else if err != nil {
		t.Fatalf("sin root: %v", err)
	}
	if got := confinamientoE2fs(); !strings.Contains(got, "missing") {
		t.Errorf("e2fs_confinement = %q; quería que dijera que falta sandbox-exec", got)
	}
	if got := newTestManager(t).Ajustes()["e2fs_confinement"]; got != confinamientoE2fs() {
		t.Errorf("Ajustes no lo dice: %q", got)
	}
}
