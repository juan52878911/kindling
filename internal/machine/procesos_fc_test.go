//go:build linux

package machine

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// En Linux, vmmVivoDe ya no escanea /proc en cada thaw: con el socket rancio
// de un VMM que murió (el caso de toda warm) contesta que no hay ninguno sin
// mirar la tabla de procesos; con el PID conocido, lo reconoce por su línea
// de órdenes. Solo si alguien escucha en su socket escanea, como antes.
func TestVMMVivoDeNoEscaneaProcConElSocketRancio(t *testing.T) {
	m := newTestManager(t)
	id := "f1f1000000000001"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.dir(id), "fc.sock")
	// El socket de un VMM que ya murió: el fichero queda, nadie escucha.
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()

	antes := escaneosProc.Load()
	if pid := m.vmmVivoDe(id, 0); pid != 0 {
		t.Fatalf("vmmVivoDe = %d con el socket rancio; quería 0", pid)
	}
	if n := escaneosProc.Load() - antes; n != 0 {
		t.Fatalf("vmmVivoDe escaneó /proc %d veces con el socket rancio", n)
	}

	// Un VMM vivo que no está entre las pistas, escuchando en su socket: se
	// encuentra escaneando.
	_ = os.Remove(sock)
	l, err = net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cmd, _ := lanzarProcesoFalso(t, argSocketVMM, sock)
	var pid int
	for i := 0; i < 400 && pid == 0; i++ {
		pid = m.vmmVivoDe(id)
		if pid == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("vmmVivoDe = %d; quería el VMM vivo %d", pid, cmd.Process.Pid)
	}

	// Con su PID como pista, sin escanear.
	antes = escaneosProc.Load()
	if got := m.vmmVivoDe(id, cmd.Process.Pid); got != cmd.Process.Pid {
		t.Fatalf("vmmVivoDe con la pista = %d; quería %d", got, cmd.Process.Pid)
	}
	if n := escaneosProc.Load() - antes; n != 0 {
		t.Fatalf("vmmVivoDe escaneó /proc %d veces teniendo el PID como pista", n)
	}
}

// Un proceso de otro usuario con la línea de órdenes de un VMM no es el VMM:
// ni liveVMs lo cuenta, ni vmmVivoDe lo toma como pista, ni adopt lo adopta.
// La línea la escribe quien lanza el proceso; el dueño, no.
func TestLiveVMsNoSeFiaDeLaLineaDeOtroUsuario(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hace falta root para lanzar un proceso como otro usuario")
	}
	m := newTestManager(t)
	id := "f1f1000000000002"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.dir(id), "fc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Mismo proceso falso que lanzarProcesoFalso, pero como nobody.
	cmd := exec.Command("/bin/sh", "-c", "read -r _", "x", argSocketVMM, sock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso como nobody: %v", err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	// Que el exec haya terminado y /proc muestre ya la línea nueva.
	for i := 0; i < 400; i++ {
		if b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); strings.Contains(string(b), sock) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := m.liveVMs()[id]; got != 0 {
		t.Errorf("liveVMs()[%s] = %d: tomó por VMM un proceso de nobody", id, got)
	}
	if got := m.vmmVivoDe(id, pid); got != 0 {
		t.Errorf("vmmVivoDe = %d: tomó por VMM un proceso de nobody", got)
	}
	if _, ok := m.adopt(&api.Machine{ID: id, PID: pid}); ok {
		t.Error("adopt adoptó un proceso de nobody")
	}
}
