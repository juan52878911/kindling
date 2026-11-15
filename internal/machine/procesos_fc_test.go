//go:build linux

package machine

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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
