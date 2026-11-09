package machine

import (
	"strings"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
)

// Una allowlist con un sufijo multiinquilino (github.io) deja un aviso en el
// log del daemon al montar la red, venga de donde venga la petición; sin
// allowlist, o con dominios corrientes, no dice nada.
func TestMontarRedAvisaDeAllowMultiinquilino(t *testing.T) {
	m := newTestManager(t)
	oldM, oldD := montarRedHost, desmontarRedHost
	t.Cleanup(func() { montarRedHost, desmontarRedHost = oldM, oldD })
	montarRedHost = func(*knet.Net, knet.Egress, []string, int) error { return nil }
	desmontarRedHost = func(*knet.Net) {}

	casos := []struct {
		egress  knet.Egress
		domains []string
		avisa   bool
	}{
		{knet.EgressAllowlist, []string{"github.com", "github.io"}, true},
		{knet.EgressAllowlist, []string{"github.com", "pypi.org"}, false},
		{knet.EgressNone, []string{"github.io"}, false},
	}
	for i, c := range casos {
		buf := capturarLog(t)
		id := "c0ffee00000000a" + string(rune('0'+i))
		if err := m.montarRed(knet.Plan(20+i, id), id, c.egress, c.domains); err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(buf.String(), "shared hosting suffix")
		if got != c.avisa {
			t.Errorf("%s %v: aviso=%v, quería %v; log: %q", c.egress, c.domains, got, c.avisa, buf.String())
		}
	}
}
