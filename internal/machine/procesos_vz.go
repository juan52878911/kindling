//go:build darwin

package machine

// Qué VMM vivo es de qué máquina, en macOS. No hay /proc: se pregunta a ps,
// que lee la misma tabla del kernel que Activity Monitor. Es más caro que
// recorrer /proc (un proceso por consulta), por eso liveVMs se sigue llamando
// fuera del candado, igual que en Linux.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// tablaPS ejecuta ps con los argumentos dados y devuelve su salida. Con plazo:
// un ps colgado no debe congelar el vigilante.
func tablaPS(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", args...).Output()
	return string(out), err
}

// liveVMs devuelve qué microVM posee cada kling-vz vivo, reconociéndolo por el
// socket de su línea de órdenes.
func (m *Manager) liveVMs() map[string]int {
	out, err := tablaPS("-axww", "-o", "pid=,command=")
	if err != nil {
		return map[string]int{}
	}
	return vmmsDeTabla(parsearPS(out), filepath.Join(m.root, "machines")+"/")
}

// adopt comprueba que el PID apuntado sigue siendo el VMM de esta máquina: los
// PID se reciclan, así que no basta con que exista.
func (m *Manager) adopt(mc *api.Machine) (string, bool) {
	if mc.PID <= 0 {
		return "", false
	}
	out, err := tablaPS("-ww", "-o", "command=", "-p", strconv.Itoa(mc.PID))
	if err != nil {
		return "", false
	}
	sock := m.dir(mc.ID) + "/fc.sock"
	if !strings.Contains(out, sock) {
		return "", false
	}
	if _, err := os.Stat(sock); err != nil {
		return "", false
	}
	return sock, true
}
