//go:build darwin

package machine

import (
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// La tabla de procesos sin ps: este mismo proceso sale con su línea de
// órdenes, la misma que daría ps -ww.
func TestTablaProcesosSinPS(t *testing.T) {
	tabla, err := tablaProcesos()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tabla[os.Getpid()], strings.Join(os.Args, " "); got != want {
		t.Fatalf("línea de órdenes propia = %q, quería %q", got, want)
	}
	if len(tabla) < 10 {
		t.Fatalf("solo %d procesos legibles", len(tabla))
	}
	l, err := lineaDeOrdenes(int32(os.Getpid()), make([]byte, procArgsMax))
	if err != nil || l != strings.Join(os.Args, " ") {
		t.Fatalf("lineaDeOrdenes = %q, %v", l, err)
	}
	if _, err := lineaDeOrdenes(1<<30, make([]byte, procArgsMax)); err == nil {
		t.Fatal("un PID que no existe dio línea")
	}
}

func TestParsearProcArgs2(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	b = append(b, "/ruta/kling-vz\x00\x00\x00\x00kling-vz\x00--api-sock\x00/a b/fc.sock\x00PATH=/bin\x00"...)
	got, err := parsearProcArgs2(b)
	if err != nil || got != "kling-vz --api-sock /a b/fc.sock" {
		t.Fatalf("= %q, %v", got, err)
	}
	if _, err := parsearProcArgs2([]byte{1}); err == nil {
		t.Fatal("aceptó un buffer corto")
	}
}

// vmmVivoDe: el PID conocido basta si es el VMM de la máquina; sin pistas,
// con alguien escuchando en su socket se mira la tabla entera; y sin nadie en
// el socket (no existe o rechaza) no hay VMM suyo.
func TestVMMVivoDe(t *testing.T) {
	root, err := os.MkdirTemp("", "kv") // corto: el socket unix tiene tope de ruta
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	m := &Manager{root: root}
	id := "0123456789abcdef"
	if err := os.MkdirAll(m.dir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := m.dir(id) + "/fc.sock"
	falso := exec.Command("/bin/sh", "-c", "sleep 30; :", "kling-hv", argSocketVMM, sock)
	if err := falso.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = falso.Process.Kill(); _ = falso.Wait() })
	pid := falso.Process.Pid
	// Hasta que arranca sh, su línea de órdenes es la del fork.
	plazo := time.Now().Add(5 * time.Second)
	for {
		if l, _ := lineaDeOrdenes(int32(pid), make([]byte, procArgsMax)); strings.Contains(l, sock) {
			break
		}
		if time.Now().After(plazo) {
			t.Fatal("el VMM falso no arrancó")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := m.vmmVivoDe(id, 0, os.Getpid(), pid); got != pid {
		t.Fatalf("con su PID entre las pistas = %d, quiero %d", got, pid)
	}
	if got := m.vmmVivoDe(id); got != 0 {
		t.Fatalf("sin socket = %d, quiero 0", got)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.vmmVivoDe(id, os.Getpid()); got != pid {
		t.Fatalf("con alguien en el socket = %d, quiero %d (la tabla entera)", got, pid)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if got := m.vmmVivoDe(id); got != 0 {
		t.Fatalf("con el socket de un VMM muerto = %d, quiero 0", got)
	}
	if got := m.vmmVivoDe("fedcba9876543210", pid); got != 0 {
		t.Fatalf("el VMM de otra máquina = %d", got)
	}
}
