package machine

import (
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// S11: dos máquinas que montan rw la misma carpeta del host (los nodos de una
// arista share de un grafo, o dos -share de un mismo directorio) gastan de
// UNA cuota, no de una cada una: si no, N nodos escribían N veces el tope.
func TestCuotaShareComunEntreMaquinas(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{byID: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: dir, Mount: "/w", Mode: share.ModeRW, UsedBytes: 300}}},
		"b": {ID: "b", Name: "b", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: dir, Mount: "/w", Mode: share.ModeRW, UsedBytes: 700}}},
	}}
	t.Cleanup(func() { m.drainShares(0) })
	m.startShares("a")
	m.startShares("b")
	qa, qb := cuotaDe(t, m, "a", 0), cuotaDe(t, m, "b", 0)
	// Empieza en lo más que dejó guardado cualquiera de las dos.
	if b, _ := qa.Usage(); b != 700 {
		t.Fatalf("uso al arrancar = %d, quería 700", b)
	}
	qa.SetUsage(900, 2)
	if b, f := qb.Usage(); b != 900 || f != 2 {
		t.Fatalf("lo que gasta a no lo ve b: %d/%d", b, f)
	}

	m.stopShares("a")
	m.stopShares("b")
	m.sup().mu.Lock()
	quedan := len(m.sup().cuotas)
	m.sup().mu.Unlock()
	if quedan != 0 {
		t.Fatalf("quedan %d cuotas compartidas sin máquinas que las usen", quedan)
	}
	for _, id := range []string{"a", "b"} {
		if s := m.byID[id].Shares[0]; s.UsedBytes != 900 {
			t.Fatalf("%s guardó %d, quería 900", id, s.UsedBytes)
		}
	}
}
