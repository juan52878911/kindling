package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsMaquinaNoExiste(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Logs("nada", 10); err == nil {
		t.Fatal("quería un error para una máquina que no existe")
	}
}

func TestLogsDevuelveLasUltimasNLineasPedidas(t *testing.T) {
	m := newTestManager(t)
	id := "logs0000000000000000"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)

	if err := os.WriteFile(filepath.Join(m.dir(id), "firecracker.log"),
		[]byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := m.Logs(id, 2); err != nil {
		t.Fatal(err)
	} else if got != "l4\nl5" {
		t.Fatalf("tail=2 = %q, quería %q", got, "l4\nl5")
	}

	if got, err := m.Logs(id, 0); err != nil {
		t.Fatal(err)
	} else if got != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("tail=0 (todo, hasta el tope) = %q", got)
	}
}

// TestLogsAcotaLineas: aunque se pida tail=0 ("todo"), Logs() nunca devuelve
// más de logMaxLines. Es M-07/D-04: la consola es la escritura de un
// invitado, no del daemon.
func TestLogsAcotaLineas(t *testing.T) {
	m := newTestManager(t)
	id := "manylines0000000000"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)

	var b strings.Builder
	b.WriteString("PRIMERA-NO-DEBE-VERSE\n")
	for i := 0; i < logMaxLines+500; i++ {
		fmt.Fprintf(&b, "linea-%d\n", i)
	}
	b.WriteString("ULTIMA-SI-DEBE-VERSE\n")
	if err := os.WriteFile(filepath.Join(m.dir(id), "firecracker.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := m.Logs(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(got, "\n")); n > logMaxLines {
		t.Fatalf("Logs() devolvió %d líneas, quería <= logMaxLines (%d)", n, logMaxLines)
	}
	if strings.Contains(got, "PRIMERA-NO-DEBE-VERSE") {
		t.Fatal("Logs() no debe traer nada por delante del tope de líneas")
	}
	if !strings.Contains(got, "ULTIMA-SI-DEBE-VERSE") {
		t.Fatal("Logs() debe traer lo último escrito")
	}
}

// TestLogsAcotaBytes: un fichero con pocas líneas pero varios MiB (por
// ejemplo, una sola línea larguísima, o un invitado que escribe con pocos
// saltos de línea) tampoco se carga entero: leerCola corta desde el final por
// bytes, no solo por líneas.
func TestLogsAcotaBytes(t *testing.T) {
	m := newTestManager(t)
	id := "bytelog00000000000"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)

	linea := strings.Repeat("z", 64<<10) // 64 KiB por línea
	var b strings.Builder
	b.WriteString("PRIMERA:" + linea + "\n")
	for i := 0; i < 80; i++ { // 80 * 64 KiB = 5 MiB > logMaxBytes (4 MiB)
		fmt.Fprintf(&b, "linea-%d:%s\n", i, linea)
	}
	b.WriteString("ULTIMA:" + linea + "\n")
	if err := os.WriteFile(filepath.Join(m.dir(id), "firecracker.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := m.Logs(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) > logMaxBytes {
		t.Fatalf("Logs() devolvió %d bytes, quería <= logMaxBytes (%d)", len(got), logMaxBytes)
	}
	if strings.Contains(got, "PRIMERA:") {
		t.Fatal("Logs() no debe traer nada por delante del tope de bytes desde el final")
	}
	if !strings.Contains(got, "ULTIMA:") {
		t.Fatal("Logs() debe traer lo último escrito")
	}
}
