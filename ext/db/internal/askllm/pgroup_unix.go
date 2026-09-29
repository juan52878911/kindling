//go:build unix

package askllm

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup pone al hijo en su propio grupo y hace que cancelar el
// contexto mate el grupo entero (opencode lanza procesos hijos).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
