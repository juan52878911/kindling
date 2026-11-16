package main

import (
	"log"
	"os/exec"
	"time"
)

// hijoVigilado es un proceso de larga vida del puente —el servidor MCP HTTP
// compartido, el Chromium compartido— cuya muerte se VE.
//
// Antes se lanzaban y se olvidaban: si morían (el oom-killer, un fallo), el
// puente seguía apuntando a ellos y cada petición daba 502 hasta que alguien
// llamaba a /reset. Con fin, quien los usa sabe si siguen vivos y los relanza.
type hijoVigilado struct {
	cmd *exec.Cmd
	// fin se cierra cuando el proceso ha salido y está recogido.
	fin chan struct{}
}

// lanzarVigilado arranca cmd en su propio grupo (StartTracked) y lo espera en
// segundo plano. Por el cosechador y no con Wait a secas: siendo PID 1, el
// wait4(-1) del cosechador puede llevárselo antes, y entonces el estado llega
// por su canal (ver waitFor).
func lanzarVigilado(cmd *exec.Cmd, nombre string) (*hijoVigilado, error) {
	exitCh, err := procReaper.StartTracked(cmd)
	if err != nil {
		return nil, err
	}
	h := &hijoVigilado{cmd: cmd, fin: make(chan struct{})}
	pid := cmd.Process.Pid
	go func() {
		err := waitFor(cmd, exitCh)
		procReaper.Forget(pid)
		log.Printf("%s (pid %d) exited: %v", nombre, pid, err)
		close(h.fin)
	}()
	return h, nil
}

// vivo dice si el proceso sigue corriendo. nil cuenta como muerto.
func (h *hijoVigilado) vivo() bool {
	if h == nil {
		return false
	}
	select {
	case <-h.fin:
		return false
	default:
		return true
	}
}

// matar mata el grupo entero y espera, con plazo, a que quede recogido: sin
// plazo, un hijo que no muere (D en un montaje colgado) bloquearía el apagado.
func (h *hijoVigilado) matar() {
	if h == nil {
		return
	}
	matarGrupo(h.cmd)
	select {
	case <-h.fin:
	case <-time.After(esperaHijoProxy):
	}
}
