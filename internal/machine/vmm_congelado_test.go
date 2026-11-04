package machine

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El VMM que un Freeze dejó vivo tras el SIGKILL no se readopta: Thaw lo mata
// antes de arrancar el nuevo, y si no muere no descongela.
func TestThawMataElVMMDeSuCongelado(t *testing.T) {
	m := newTestManager(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }() // lo recoge, como esperarVMM
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	mc := &api.Machine{ID: "aa11bb22cc33dd44", Name: "m"}
	if m.esVMMDeCongelado(mc.ID, pid) {
		t.Fatal("without a mark, any VMM is a candidate for re-adoption")
	}
	m.vmmDeCongelado.Store(mc.ID, pid)
	if m.esVMMDeCongelado(mc.ID, pid+1) {
		t.Fatal("another pid is not the frozen VMM")
	}
	if _, ok := m.vmmDeCongelado.Load(mc.ID); ok {
		t.Fatal("the mark of a VMM that is gone must be forgotten")
	}

	// Uno que no muere: no se descongela.
	m.vmmDeCongelado.Store(mc.ID, pid)
	prevSenal, prevPlazo := senalMatar, plazoMuerte
	senalMatar, plazoMuerte = func(int) {}, 20*time.Millisecond
	t.Cleanup(func() { senalMatar, plazoMuerte = prevSenal, prevPlazo })
	if !m.esVMMDeCongelado(mc.ID, pid) {
		t.Fatal("the frozen VMM was not recognised")
	}
	if err := m.thawVMMViejo(mc, pid); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("a VMM that survives SIGKILL: %v", err)
	}

	// Con el SIGKILL de verdad muere, y la marca se va.
	senalMatar, plazoMuerte = prevSenal, 5*time.Second
	if err := m.thawVMMViejo(mc, pid); err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("the old VMM is still alive")
	}
	if _, ok := m.vmmDeCongelado.Load(mc.ID); ok {
		t.Fatal("the mark outlived its VMM")
	}
}
