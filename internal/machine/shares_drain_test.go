package machine

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// Al apagar el daemon, drainShares corta los bucles de las carpetas vivas (aquí
// uno que reintenta porque la máquina aún no es alcanzable) sin esperar a su
// backoff, y después ya no se lanza ninguno: el daemon siguiente es quien
// vuelve a conectarlas.
func TestDrainSharesParaLosBuclesYNoRelanza(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "m1", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRW}}},
	}}
	m.startShares("m1")
	m.sup().mu.Lock()
	run := m.sup().runs["m1"]
	m.sup().mu.Unlock()
	if run == nil {
		t.Fatal("startShares no lanzó la carpeta viva")
	}
	// Que el bucle haya fallado ya alguna vez y esté en su backoff.
	time.Sleep(300 * time.Millisecond)

	t0 := time.Now()
	m.drainShares(5 * time.Second)
	select {
	case <-run.done:
	default:
		t.Fatal("drainShares volvió con el bucle de la carpeta aún vivo")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("drainShares tardó %s: esperó al backoff en vez de cortar", d)
	}

	m.sup().mu.Lock()
	delete(m.sup().runs, "m1")
	m.sup().mu.Unlock()
	m.startShares("m1")
	m.sup().mu.Lock()
	despues := m.sup().runs["m1"]
	m.sup().mu.Unlock()
	if despues != nil {
		t.Fatal("tras drainShares se lanzó otra vez la carpeta")
	}
	// Idempotente: una segunda llamada (Close tras Close) no entra en pánico.
	m.drainShares(time.Second)
}

// Congelar cierra en orden SOLO las carpetas de esa máquina y no deja el
// supervisor apagado: al descongelar se vuelven a lanzar.
func TestDrainSharesOfSoloEsaMaquina(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "m1", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRW}}},
		"m2": {ID: "m2", Name: "m2", State: api.StateRunning,
			Shares: []api.ShareAttachment{{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRO}}},
	}}
	m.startShares("m1")
	m.startShares("m2")
	m.sup().mu.Lock()
	r1, r2 := m.sup().runs["m1"], m.sup().runs["m2"]
	m.sup().mu.Unlock()
	time.Sleep(300 * time.Millisecond)

	t0 := time.Now()
	m.drainSharesOf("m1")
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("drainSharesOf tardó %s", d)
	}
	select {
	case <-r1.done:
	default:
		t.Fatal("la carpeta de m1 sigue viva")
	}
	select {
	case <-r2.done:
		t.Fatal("drainSharesOf(m1) paró también la de m2")
	default:
	}
	m.startShares("m1")
	m.sup().mu.Lock()
	otra := m.sup().runs["m1"]
	m.sup().mu.Unlock()
	if otra == nil || otra == r1 {
		t.Fatal("tras congelar, m1 no vuelve a lanzar su carpeta")
	}
	m.drainShares(5 * time.Second)
}
