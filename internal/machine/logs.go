package machine

import (
	"fmt"
	"os"
	"strings"
)

// Logs devuelve la consola serie de una microVM.
//
// Es la única ventana al interior mientras no haya red ni agente dentro: si una
// herramienta no arranca, la razón está aquí.
func (m *Manager) Logs(ref string, tail int) (string, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return "", fmt.Errorf("machine %q does not exist", ref)
	}
	b, err := os.ReadFile(m.dir(mc.ID) + "/firecracker.log")
	if err != nil {
		return "", fmt.Errorf("no log for %s: %w", mc.Name, err)
	}
	if tail <= 0 {
		return string(b), nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}
