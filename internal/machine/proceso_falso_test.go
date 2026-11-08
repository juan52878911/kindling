package machine

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// lanzarProcesoFalso lanza un proceso que pasa por VMM en las pruebas: vive
// hasta el Cleanup y lleva args en su línea de órdenes (p. ej. argSocketVMM y
// la ruta del socket). fin se cierra cuando muere y ya está recogido.
//
// Tres trampas que daban pruebas intermitentes:
//   - En macOS /bin/sh es un lanzador que hace exec de bash con el mismo argv;
//     durante ese exec kern.procargs2 da EIO/EINVAL, y la línea ya llevaba los
//     args antes, así que esperar a verlos no bastaba. Por eso /bin/bash.
//   - Con sleep, el shell hace fork y el hijo lleva la misma línea hasta su
//     exec: liveVMs podía quedarse con él. read es del propio shell y espera
//     sin hijos mientras la tubería siga abierta.
//   - Sin cmd.Wait, el proceso muerto queda zombi y Kill(pid, 0) lo sigue
//     viendo: quien espera a que muera (waitGone) agota su plazo.
func lanzarProcesoFalso(t *testing.T, args ...string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := procesoFalsoSinRecoger(t, args...)
	fin := make(chan struct{})
	go func() { _ = cmd.Wait(); close(fin) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
		}
	})
	return cmd, fin
}

// procesoFalsoSinRecoger es lanzarProcesoFalso sin el cmd.Wait: para las
// pruebas que se lo dan a quien lo espera de verdad (esperarVMM), que tiene
// que ser el único en recogerlo para leer cómo murió. El Cleanup lo mata.
func procesoFalsoSinRecoger(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	shell := "/bin/sh"
	if runtime.GOOS == "darwin" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, append([]string{"-c", "read -r _"}, args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Process.Kill() })
	return cmd
}
