package machine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

func TestMotivoEnCola(t *testing.T) {
	casos := []struct{ cola, quiero string }{
		{"[ 1.0] reboot: Restarting system\nkling-hv: the guest stopped: the guest rebooted (SYSTEM_RESET)\n",
			"the guest stopped: the guest rebooted (SYSTEM_RESET)"},
		{"kling-vz: instance started in 30 ms\nkling-vz: the VM stopped with an error: boom\r\n",
			"the VM stopped with an error: boom"},
		{"kling-hv: the guest stopped", "the guest stopped"},
		// La última manda: la del invitado va antes que la del VMM.
		{"kling-hv: the guest stopped: forged\nkling-hv: the guest stopped: real\n", "the guest stopped: real"},
		// Sin línea de fin (Firecracker, un kill -9): nada.
		{"kling-hv: instance started in 30 ms\n[ 0.1] something\n", ""},
		{"", ""},
		// Saneada: sin caracteres de control.
		{"kling-hv: the guest stopped: a\x1b[31mb\x07\n", "the guest stopped: a[31mb"},
	}
	for _, c := range casos {
		if got := motivoEnCola([]byte(c.cola)); got != c.quiero {
			t.Errorf("motivoEnCola(%q) = %q, want %q", c.cola, got, c.quiero)
		}
	}
	largo := "kling-hv: the guest stopped: " + strings.Repeat("x", 1000)
	if got := motivoEnCola([]byte(largo)); len([]rune(got)) > maxMotivoFinVMM+1 {
		t.Errorf("not bounded: %d runes", len([]rune(got)))
	}
}

// Un VMM que sale solo (exit 0) tras reiniciarse el invitado: el evento dice
// por qué, y LastErr se queda en errProcesoDesaparecido (lo compara gc.go).
func TestVMMQueTerminaDiceElMotivo(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	sub, baja := m.bus.Subscribe()
	defer baja()
	id := "dead0000000000b2"
	cmd := vmmQueSeVe(t, m, id)
	corriendoCon(m, id, cmd.Process.Pid)
	logf, err := abrirConsola(m.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logf.WriteString("kling-hv: the guest stopped: the guest rebooted (SYSTEM_RESET)\n"); err != nil {
		t.Fatal(err)
	}
	hecho := make(chan struct{})
	go func() { m.esperarVMM(id, cmd, logf); close(hecho) }()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("esperarVMM no volvió")
	}
	if mc := estadoDeId(t, m, id); mc.State != api.StateFailed || mc.LastErr != errProcesoDesaparecido {
		t.Fatalf("state=%s err=%q, want failed and %q exactly", mc.State, mc.LastErr, errProcesoDesaparecido)
	}
	select {
	case ev := <-sub:
		if !strings.Contains(ev.Message, "the guest rebooted (SYSTEM_RESET)") || !strings.Contains(ev.Message, "killed") {
			t.Fatalf("event message %q, want the exit status and the VMM's reason", ev.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no EvFailed")
	}
}

// Una consola que no es la del daemon (un enlace plantado) no se lee.
func TestMotivoFinVMMNoSigueEnlaces(t *testing.T) {
	m := newTestManager(t)
	id := "dead0000000000c3"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	fuera := filepath.Join(t.TempDir(), "host")
	if err := os.WriteFile(fuera, []byte("kling-hv: the guest stopped: from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fuera, filepath.Join(m.dir(id), "firecracker.log")); err != nil {
		t.Fatal(err)
	}
	if got := m.motivoFinVMM(id); got != "" {
		t.Fatalf("read through a link: %q", got)
	}
}

// Un restore que falla con la API del VMM cortada (EOF) porque el VMM murió
// dice que murió y lo último de su consola; con el VMM vivo, o con otro
// error, el error no cambia.
func TestVMMCaidoEnLaRestauracion(t *testing.T) {
	m := newTestManager(t)
	id := "dead0000000000c3"
	if err := os.MkdirAll(m.dir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	logf, err := abrirConsola(m.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logf.WriteString("[ 0.1] guest line\nFIXME: \"Handle no active virtual interrupt, EOIR1 0x1b\" line 209\n\n"); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	muerto := exec.Command("true")
	if err := muerto.Run(); err != nil {
		t.Fatal(err)
	}
	eof := fmt.Errorf("opening port forwards: %w", io.EOF)
	got := m.vmmCaido(eof, id, muerto.Process.Pid)
	if !errors.Is(got, io.EOF) || !strings.Contains(got.Error(), "exited during the restore") ||
		!strings.Contains(got.Error(), `EOIR1 0x1b" line 209`) {
		t.Errorf("dead VMM: %v", got)
	}
	vivo := exec.Command("sleep", "30")
	if err := vivo.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vivo.Process.Kill(); _ = vivo.Wait() }()
	if got := m.vmmCaido(eof, id, vivo.Process.Pid); got != eof {
		t.Errorf("live VMM: %v, want the error unchanged", got)
	}
	otro := errors.New("repointing overlay: no such drive")
	if got := m.vmmCaido(otro, id, muerto.Process.Pid); got != otro {
		t.Errorf("other error: %v, want it unchanged", got)
	}
}
