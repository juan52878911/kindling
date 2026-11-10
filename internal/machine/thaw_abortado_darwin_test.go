//go:build darwin

package machine

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// M3: un thaw que falla tras lanzar el VMM no contesta hasta verlo morir.
// Antes mandaba el SIGKILL y volvía: un reintento inmediato encontraba el VMM
// moribundo en liveVMs, lo readoptaba («was already running») y la máquina
// acababa failed al morir el proceso.
func TestThawFallidoEsperaAQueMueraSuVMM(t *testing.T) {
	m, _ := managerVZ(t)
	t.Setenv(envFakeVZRechazaCarga, "1")
	id := "ee55ff66aa77bb93"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-abortado", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}

	// El SIGKILL llega tarde, como a un VMM que tarda en soltar su memoria.
	var matado atomic.Int64
	prev := senalMatar
	senalMatar = func(pid int) {
		matado.Store(int64(pid))
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}()
	}
	t.Cleanup(func() { senalMatar = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err == nil {
		t.Fatal("el thaw no vio el rechazo del VMM")
	}
	pid := int(matado.Load())
	if pid == 0 {
		t.Fatal("el thaw fallido no mató su VMM")
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("Thaw volvió con su VMM (pid %d) aún vivo: un reintento lo readoptaría", pid)
	}
}
