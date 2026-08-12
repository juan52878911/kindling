package machine

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitGone tiene que volver EN CUANTO el proceso desaparece, no esperar al tope.
//
// Es lo que hace que congelar para hacer sitio funcione: el gateway reintenta en
// cuanto Freeze retorna, y si retornara antes de que la RAM estuviera libre
// volvería a chocar con "no cabe". Comprobado que ese era el fallo en el
// laboratorio: el reintento llegaba un segundo tras el SIGKILL.
func TestWaitGoneVuelveAlMorirElProceso(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // recoge el zombi, como hace el arranque real

	_ = syscall.Kill(pid, syscall.SIGKILL)
	inicio := time.Now()
	waitGone(pid, 5*time.Second)
	d := time.Since(inicio)

	if d >= 5*time.Second {
		t.Fatalf("waitGone agotó el tope (%v): no detectó que el proceso murió", d)
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Error("waitGone volvió pero el proceso sigue ahí")
	}
	t.Logf("detectó la muerte en %v", d)
}

// Un pid que ya no existe se resuelve al instante, sin esperar.
func TestWaitGoneConProcesoYaMuertoNoEspera(t *testing.T) {
	inicio := time.Now()
	waitGone(2147483000, time.Second) // un pid que no existe
	if d := time.Since(inicio); d > 200*time.Millisecond {
		t.Errorf("esperó %v por un proceso inexistente", d)
	}
}
