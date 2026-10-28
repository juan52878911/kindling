//go:build darwin

package machine

import (
	"os"
	"os/exec"
	"slices"
	"testing"
)

// La huella de los hijos de un proceso se lee sin cgo: un hijo que duerme
// aparece entre los de este proceso y pesa algo.
func TestHuellaHijos(t *testing.T) {
	if huellaBytes(os.Getpid()) <= 0 {
		t.Fatal("la huella de este proceso no se lee")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if h := hijosDe(os.Getpid()); !slices.Contains(h, cmd.Process.Pid) {
		t.Fatalf("hijos %v sin %d", h, cmd.Process.Pid)
	}
	if n := huellaHijosBytes(os.Getpid()); n <= 0 {
		t.Fatalf("huella de los hijos: %d", n)
	}
	if huellaHijosBytes(0) != 0 || len(hijosDe(1<<30)) != 0 {
		t.Fatal("un PID que no existe tiene hijos")
	}
}
