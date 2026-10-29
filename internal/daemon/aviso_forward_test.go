package daemon

import (
	"errors"
	"strings"
	"testing"
)

// El daemon deja en el log, al arrancar, el aviso de un FORWARD en DROP sin
// salida para las máquinas; callado si todo va bien o si no puede leer.
func TestAvisoForward(t *testing.T) {
	leer := func(s string, err error) func() (string, error) {
		return func() (string, error) { return s, err }
	}
	if a := avisoForward(leer("", errors.New("permission denied"))); a != "" {
		t.Errorf("unreadable rules: %q", a)
	}
	if a := avisoForward(leer("-P FORWARD ACCEPT\n", nil)); a != "" {
		t.Errorf("policy ACCEPT: %q", a)
	}
	a := avisoForward(leer("-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n", nil))
	if !strings.HasPrefix(a, "WARNING: ") || !strings.Contains(a, "sudo iptables -A FORWARD -i vh-+") {
		t.Errorf("policy DROP: %q", a)
	}
}
