package daemon

// El proceso del constructor (R4 de la tercera revisión): que el plazo de la
// construcción lo acote de verdad y que su salida no crezca sin tope en la
// memoria del daemon.

import (
	"bytes"
	"errors"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// maxSalidaConstructor es lo que se guarda de la salida de un constructor:
// el final, que es donde está el error.
const maxSalidaConstructor = 1 << 20

// esperaConstructor es lo que Run espera a las tuberías tras acabar (o
// matar) el constructor. Sin él, un nieto que heredó la salida dejaba Run
// colgado y, con él, los cerrojos de construcción hasta reiniciar el daemon.
// Variable para los tests.
var esperaConstructor = 5 * time.Second

// colaSalida guarda en buf solo los últimos max bytes que se le escriben.
type colaSalida struct {
	mu  sync.Mutex
	buf *bytes.Buffer
	max int
}

func (c *colaSalida) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if len(p) > c.max {
		p = p[len(p)-c.max:]
	}
	if sobra := c.buf.Len() + len(p) - c.max; sobra > 0 {
		c.buf.Next(sobra)
	}
	c.buf.Write(p)
	return n, nil
}

// prepararProcesoConstructor pone al constructor en su propio grupo de
// procesos, hace que el plazo mate el grupo entero (no solo el hijo directo)
// y acota la espera a sus tuberías.
func prepararProcesoConstructor(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return matarGrupo(cmd) }
	cmd.WaitDelay = esperaConstructor
}

func matarGrupo(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// correrConstructor ejecuta cmd con su salida en salida (solo el último MiB).
// Si el constructor acabó bien pero algo suyo seguía con la salida abierta,
// se mata ese resto de su grupo y la construcción cuenta como buena.
func correrConstructor(cmd *exec.Cmd, salida *bytes.Buffer) error {
	prepararProcesoConstructor(cmd)
	cola := &colaSalida{buf: salida, max: maxSalidaConstructor}
	cmd.Stdout, cmd.Stderr = cola, cola
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// El grupo sigue existiendo (alguien tiene la tubería), así que su
		// número no es de nadie más.
		_ = matarGrupo(cmd)
		if cmd.ProcessState != nil && cmd.ProcessState.Success() {
			log.Printf("builder: %s exited but left processes holding its output; killed them", cmd.Path)
			return nil
		}
	}
	return err
}
