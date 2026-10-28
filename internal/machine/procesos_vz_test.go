//go:build darwin

package machine

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
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
