package machine

import (
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// cuotaDe devuelve la cuota de la carpeta tag de la ejecución en curso de id.
func cuotaDe(t *testing.T, m *Manager, id string, tag int) interface {
	Usage() (int64, int64)
	SetUsage(int64, int64)
} {
	t.Helper()
	m.sup().mu.Lock()
	run := m.sup().runs[id]
	m.sup().mu.Unlock()
	if run == nil {
		t.Fatal("startShares no lanzó la carpeta viva")
	}
	for _, tg := range run.tags {
		if tg.tag == tag && tg.quota != nil {
			return tg.quota
		}
	}
	t.Fatalf("la carpeta %d no tiene cuota", tag)
	return nil
}

// La cuota de una carpeta rw no vuelve a cero al congelar y descongelar, ni al
// parar y arrancar, ni con otro daemon: el gateway congela por inactividad, y
// cada ciclo regalaba la cuota entera otra vez (900 + congelar + 900 + ...).
func TestCuotaShareSobreviveCongelarYParar(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "m1", State: api.StateRunning,
			Shares: []api.ShareAttachment{
				{Source: t.TempDir(), Mount: "/r", Mode: share.ModeRO},
				{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRW},
			}},
	}}
	t.Cleanup(func() { m.drainShares(0) })
	m.startShares("m1")
	// Lo que habría escrito el invitado en esta ejecución.
	cuotaDe(t, m, "m1", 1).SetUsage(900, 3)

	// Con la máquina en marcha, inspect lo enseña.
	mc, _ := m.get("m1")
	m.decorarShares(mc)
	if u := mc.Shares[1]; u.UsedBytes != 900 || u.UsedFiles != 3 {
		t.Fatalf("en marcha, used = %d/%d; quería 900/3", u.UsedBytes, u.UsedFiles)
	}

	m.drainSharesOf("m1") // congelar
	if s := m.byID["m1"].Shares[1]; s.UsedBytes != 900 || s.UsedFiles != 3 {
		t.Fatalf("tras congelar se guardó %d/%d; quería 900/3", s.UsedBytes, s.UsedFiles)
	}
	if s := m.byID["m1"].Shares[0]; s.UsedBytes != 0 {
		t.Fatalf("la carpeta ro no tiene cuota y guardó %d", s.UsedBytes)
	}
	m.startShares("m1") // descongelar
	if b, f := cuotaDe(t, m, "m1", 1).Usage(); b != 900 || f != 3 {
		t.Fatalf("al descongelar la cuota empieza en %d/%d; quería 900/3", b, f)
	}

	cuotaDe(t, m, "m1", 1).SetUsage(950, 4)
	m.stopShares("m1") // parar (o morir el VMM)
	m.startShares("m1")
	if b, f := cuotaDe(t, m, "m1", 1).Usage(); b != 950 || f != 4 {
		t.Fatalf("al arrancar otra vez la cuota empieza en %d/%d; quería 950/4", b, f)
	}

	cuotaDe(t, m, "m1", 1).SetUsage(990, 5)
	m.drainShares(0) // apagar el daemon
	if s := m.byID["m1"].Shares[1]; s.UsedBytes != 990 || s.UsedFiles != 5 {
		t.Fatalf("al apagar el daemon se guardó %d/%d; quería 990/5", s.UsedBytes, s.UsedFiles)
	}
}
