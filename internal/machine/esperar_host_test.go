//go:build darwin

package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// R3-2: la comprobación de las barreras del host corre en paralelo con el
// montaje de la red y quien la lanza debe esperarla antes de que el invitado
// pueda emitir. Estos tests, contra el kling-vz falso, fallan si se quita la
// espera de runFromUna (antes de Resume) o la de thaw (antes de cargar).

// barreraCaida hace que la comprobación aparte falle al esperarla y cuenta
// los desmontajes de la red.
func barreraCaida(t *testing.T) *atomic.Int32 {
	t.Helper()
	oldA, oldD, oldM := montarRedHostAparte, desmontarRedHost, montarRedHost
	t.Cleanup(func() { montarRedHostAparte, desmontarRedHost, montarRedHost = oldA, oldD, oldM })
	montarRedHost = nil
	montarRedHostAparte = func(*knet.Net, knet.Egress, []string, int) (func() error, error) {
		return func() error { return errors.New("barrera caída") }, nil
	}
	var desmontadas atomic.Int32
	desmontarRedHost = func(*knet.Net) { desmontadas.Add(1) }
	return &desmontadas
}

func TestThawNoCargaSiFallanLasBarrerasDelHost(t *testing.T) {
	m, logPath := managerVZ(t)
	desmontadas := barreraCaida(t)
	id := "ee55ff66aa77bb01"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-barrera", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err == nil || !strings.Contains(err.Error(), "barrera caída") {
		t.Fatalf("thaw con la barrera caída: %v, quería su error", err)
	}
	ls := llamadas(t, logPath)
	if i := indice(ls, "PUT /snapshot/load"); i >= 0 {
		t.Fatalf("se cargó el volcado con las barreras del host caídas:\n%s", strings.Join(ls, "\n"))
	}
	if i := indice(ls, "PATCH /vm"); i >= 0 {
		t.Fatalf("se reanudó el invitado con las barreras del host caídas:\n%s", strings.Join(ls, "\n"))
	}
	if desmontadas.Load() == 0 {
		t.Fatal("la red quedó montada tras el fallo de las barreras")
	}
	if _, ok := m.redMontada.Load(id); ok {
		t.Fatal("la red fallida sigue constando como montada")
	}
}

func TestRunFromNoReanudaSiFallanLasBarrerasDelHost(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "0") // dorado de prueba sin firma
	m, logPath := managerVZ(t)
	desmontadas := barreraCaida(t)
	m.snapPending = map[string]int{}
	snapDir := escribirSnapshot(t, m, "dorado", api.Snapshot{VMM: m.origenActual().vmm, MemMiB: 64, VCPUs: 1,
		CreatedAt: time.Now()})
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(snapDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.runFrom(ctx, api.RunRequest{From: "dorado", Egress: "none"})
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err == nil || !strings.Contains(err.Error(), "barrera caída") {
		t.Fatalf("run -from con la barrera caída: %v, quería su error", err)
	}
	ls := llamadas(t, logPath)
	if indice(ls, "PUT /snapshot/load") < 0 {
		t.Fatalf("la prueba no llegó a cargar el volcado (no ejercita la espera):\n%s", strings.Join(ls, "\n"))
	}
	for _, l := range ls {
		if strings.HasPrefix(l, "PATCH /vm") && strings.Contains(l, "Resumed") || l == "PATCH /vm" {
			t.Fatalf("se reanudó el invitado con las barreras del host caídas:\n%s", strings.Join(ls, "\n"))
		}
	}
	if desmontadas.Load() == 0 {
		t.Fatal("la red quedó montada tras el fallo de las barreras")
	}
}
