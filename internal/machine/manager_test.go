package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// En macOS delegacionCgroups devuelve ("", nil): no hay cgroups y el techo de
// CPU lo pone kling-vz. Cada arranque del daemon avisaba de que faltaban los
// controladores de memoria y de procesos: un aviso falso.
func TestRevisarControladoresSinRaizNoAvisa(t *testing.T) {
	mem, pids, avisos := revisarControladores("")
	if mem || pids || len(avisos) != 0 {
		t.Fatalf("sin raíz de cgroups: mem=%v pids=%v avisos=%q; quiero false, false y ninguno", mem, pids, avisos)
	}
}

func TestRevisarControladoresAvisaDeLosQueFaltan(t *testing.T) {
	dir := t.TempDir()
	escribir := func(lista string) {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(lista), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escribir("cpu memory pids\n")
	if mem, pids, avisos := revisarControladores(dir); !mem || !pids || len(avisos) != 0 {
		t.Fatalf("con todos: mem=%v pids=%v avisos=%q", mem, pids, avisos)
	}
	escribir("cpu\n")
	if mem, pids, avisos := revisarControladores(dir); mem || pids || len(avisos) != 2 {
		t.Fatalf("solo cpu: mem=%v pids=%v avisos=%q; quiero dos avisos", mem, pids, avisos)
	}
}
