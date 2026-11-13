package machine

import (
	"testing"
	"time"

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

// Lo gastado se apunta también mientras la máquina corre, no solo al drenar:
// con 900 MiB gastados de 1000, un daemon que muere sin drenar (kill -9, un
// corte) no le devuelve al siguiente esos 900 MiB, y 200 MiB más no caben.
func TestCuotaShareSobreviveAUnDaemonMuerto(t *testing.T) {
	antes := cadaPuntoControlCuota
	cadaPuntoControlCuota = 5 * time.Millisecond
	t.Cleanup(func() { cadaPuntoControlCuota = antes })

	const tope = 1000 << 20
	cfg := func() ShareConfig { return ShareConfig{MaxBytes: tope, MinFreeBytes: -1} }
	m := &Manager{byID: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "m1", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRW}}},
	}, shareCfg: cfg}
	t.Cleanup(func() { m.stopShares("m1") })
	m.startShares("m1")
	cuotaDe(t, m, "m1", 0).SetUsage(900<<20, 7)

	// Sin drenar: lo apuntado en la máquina es lo que leería de state.json
	// el daemon siguiente.
	var guardada *api.Machine
	for plazo := time.Now().Add(5 * time.Second); ; {
		m.mu.RLock()
		guardada = m.byID["m1"].Clone()
		m.mu.RUnlock()
		if guardada.Shares[0].UsedBytes == 900<<20 || time.Now().After(plazo) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if u := guardada.Shares[0]; u.UsedBytes != 900<<20 || u.UsedFiles != 7 {
		t.Fatalf("sin drenar quedó apuntado %d/%d; quería %d/7", u.UsedBytes, u.UsedFiles, 900<<20)
	}

	m2 := &Manager{byID: map[string]*api.Machine{"m1": guardada}, shareCfg: cfg}
	t.Cleanup(func() { m2.stopShares("m1") })
	m2.startShares("m1")
	if b, _ := cuotaDe(t, m2, "m1", 0).Usage(); b+200<<20 <= tope {
		t.Fatalf("el daemon siguiente empieza en %d MiB: 200 MiB más cabrían en los %d MiB de la cuota", b>>20, tope>>20)
	}
}
