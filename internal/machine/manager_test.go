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

// C9-01: el disco de la capa (los pesos de un modelo) lleva en el arranque en
// frío una ráfaga del tamaño de la capa: la carga ya no la fija el limitador
// de 128 MiB/s (7,7 s medidos para 1 GiB). El caudal sostenido no cambia, y
// los otros discos (base, overlay, volúmenes) siguen sin ráfaga. Solo en frío
// por construcción: es el único camino que hace SetDrive de la capa; una
// restauración hereda el estado del cubo del volcado, con la ráfaga gastada.
func TestCapaConRafagaSoloEnFrio(t *testing.T) {
	capa := filepath.Join(t.TempDir(), "capa.ext4")
	if err := os.WriteFile(capa, make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	l := limiteCapa(capa)
	if l == nil || l.Bandwidth == nil {
		t.Fatal("la capa sin limitador")
	}
	if l.Bandwidth.Size != diskBytesPerSec || l.Bandwidth.RefillTime != 1000 {
		t.Errorf("caudal sostenido %+v, quería el de siempre", *l.Bandwidth)
	}
	if l.Bandwidth.OneTimeBurst != 3<<20 {
		t.Errorf("ráfaga %d, quería el tamaño de la capa %d", l.Bandwidth.OneTimeBurst, 3<<20)
	}
	// Sin poder leer la capa, sin ráfaga: el limitador de siempre.
	if l := limiteCapa(filepath.Join(t.TempDir(), "no-esta")); l.Bandwidth.OneTimeBurst != 0 {
		t.Errorf("ráfaga %d para una capa que no existe", l.Bandwidth.OneTimeBurst)
	}
	if b := limiteDisco().Bandwidth; b.OneTimeBurst != 0 || b.Size != diskBytesPerSec {
		t.Errorf("los otros discos: %+v, quería sin ráfaga", *b)
	}
}
