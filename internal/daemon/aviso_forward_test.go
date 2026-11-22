package daemon

import (
	"errors"
	"strings"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
)

// El daemon deja en el log, al arrancar, el aviso de un FORWARD en DROP sin
// salida para las máquinas; callado si todo va bien o si no puede leer.
func TestAvisoForward(t *testing.T) {
	leer := func(s string, err error) func() (string, error) {
		return func() (string, error) { return s, err }
	}
	faltaAllowlist = func() string { return "" }
	t.Cleanup(func() { faltaAllowlist = knet.FaltaParaAllowlist })
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

// Sin ipset el daemon lo dice al arrancar, también si el FORWARD está bien o
// no se pudo leer; con los dos problemas, los dos avisos.
func TestAvisoSinIpset(t *testing.T) {
	faltaAllowlist = func() string { return "ipset is not installed: -egress allowlist will fail" }
	t.Cleanup(func() { faltaAllowlist = knet.FaltaParaAllowlist })
	a := avisoForward(func() (string, error) { return "-P FORWARD ACCEPT\n", nil })
	if a != "WARNING: ipset is not installed: -egress allowlist will fail" {
		t.Errorf("solo ipset: %q", a)
	}
	if a := avisoForward(func() (string, error) { return "", errors.New("x") }); !strings.Contains(a, "ipset") {
		t.Errorf("reglas ilegibles: %q", a)
	}
	a = avisoForward(func() (string, error) { return "-P FORWARD DROP\n", nil })
	if strings.Count(a, "WARNING: ") != 2 || !strings.Contains(a, "FORWARD policy is DROP") {
		t.Errorf("los dos: %q", a)
	}
}
