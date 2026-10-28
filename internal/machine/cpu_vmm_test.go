package machine

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// cpuVMM lee la CPU de un proceso de verdad (aquí, el del propio test): en
// macOS con el reparto entre núcleos P y E, en Linux solo el total.
func TestCPUVMMDelPropioProceso(t *testing.T) {
	t0 := time.Now()
	for x := 0; time.Since(t0) < 200*time.Millisecond; x++ {
	}
	u, ok := cpuVMM(os.Getpid(), "")
	if !ok {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			t.Fatal("no leyó la CPU del propio proceso")
		}
		t.Skip("sin lectura de CPU en este sistema")
	}
	if u.total < 100*time.Millisecond || u.total > time.Hour {
		t.Fatalf("CPU = %v tras gastar ~200 ms", u.total)
	}
	if runtime.GOOS == "darwin" {
		if !u.conNucleos || u.p > u.total {
			t.Fatalf("reparto P/E = %+v", u)
		}
	} else if u.conNucleos {
		t.Fatal("fuera de macOS no hay reparto P/E")
	}
}
