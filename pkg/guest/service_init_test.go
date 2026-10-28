package guest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOtherProcs(t *testing.T) {
	root := t.TempDir()
	add := func(pid, stat string) {
		d := filepath.Join(root, pid)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	}
	add("1", "1 (init) S 0 1")
	add("2", "2 (kthreadd) S 0 0")
	add("5", "5 (kworker) S 2 0")       // hilo del kernel
	add("9", "9 (agent) S 1 9")         // el agente (self)
	add("10", "10 (zz) Z 1 10")         // zombi
	add("11", "11 (exec) S 9 11")       // hijo del agente
	add("20", "20 (my (svc) x) S 1 20") // comm raro: cuenta
	os.WriteFile(filepath.Join(root, "cmdline"), nil, 0o644)
	if n := otherProcs(root, 9); n != 1 {
		t.Errorf("otherProcs = %d, want 1", n)
	}
}
