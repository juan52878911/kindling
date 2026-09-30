package machine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tras rotar, firecracker.log se queda vacío: lo último que escribió el
// invitado vive en firecracker.log.1, y `kling logs` tiene que seguir
// enseñándolo.
func TestLogsNoSePierdenAlRotar(t *testing.T) {
	m := newTestManager(t)
	id := "logsrot000000000"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)

	var buf bytes.Buffer
	for i := 0; buf.Len() <= consolaMaxBytes; i++ {
		fmt.Fprintf(&buf, "linea %08d del arranque\n", i)
	}
	buf.WriteString("kernel panic: lo que hay que ver\n")
	log := filepath.Join(m.dir(id), "firecracker.log")
	if err := os.WriteFile(log, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rotarConsola(m.dir(id)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(log); fi.Size() != 0 {
		t.Fatalf("la rotación no truncó: %d bytes", fi.Size())
	}

	got, err := m.Logs(id, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "kernel panic: lo que hay que ver") {
		t.Fatalf("justo tras rotar, logs no enseña lo último del invitado: %q", got)
	}

	// Y lo nuevo va detrás de lo rotado.
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("despues\n")
	f.Close()
	got, err = m.Logs(id, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != "kernel panic: lo que hay que ver\ndespues" {
		t.Fatalf("tail=2 tras rotar y escribir = %q", got)
	}
}
