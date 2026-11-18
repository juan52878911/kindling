//go:build darwin

package hvvm

import (
	"runtime"
	"testing"
	"time"
)

// R6: el tope de CPU cobra el proceso entero, no solo los hilos de vCPU: la
// E/S de los discos, la red y vsock corre en hilos propios (y la pila de red
// en Go) y trabaja para el invitado. Aquí no hay VM, así que con la cuenta de
// antes (solo vCPU) no subía nada.
func TestCPUTimeCobraTodoElProceso(t *testing.T) {
	antes, err := CPUTime()
	if err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	fin := time.Now().Add(200 * time.Millisecond)
	x := 0
	for time.Now().Before(fin) {
		x++
	}
	despues, err := CPUTime()
	if err != nil {
		t.Fatal(err)
	}
	d := despues - antes
	t.Logf("%v of CPU charged for 200 ms of spinning outside the vCPUs (%d loops)", d, x)
	if d < 20*time.Millisecond {
		t.Fatalf("200 ms of work in a non-vCPU thread charged %v, want at least 20 ms", d)
	}
}
