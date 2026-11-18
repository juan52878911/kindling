package machine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func escribirConsola(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// LogsDesde da solo lo escrito tras el cursor (líneas repetidas incluidas, que
// es lo que el ancla de `kling logs -f` perdía), y tras una rotación empieza
// por el principio del fichero nuevo.
func TestLogsDesdeSigueElCursorYLaRotacion(t *testing.T) {
	m := newTestManager(t)
	id := "logscur000000000"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)
	log := filepath.Join(m.dir(id), "firecracker.log")
	escribirConsola(t, log, "tick\ntick\n")

	out, cur, err := m.LogsCola(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out != "tick\ntick" || cur.Offset != 10 || cur.Rotacion != "0" {
		t.Fatalf("cola %q, cursor %+v", out, cur)
	}

	escribirConsola(t, log, "tick\n")
	b, cur, err := m.LogsDesde(id, cur)
	if err != nil || string(b) != "tick\n" || cur.Offset != 15 {
		t.Fatalf("desde: %q %+v %v", b, cur, err)
	}
	b, cur, err = m.LogsDesde(id, cur)
	if err != nil || len(b) != 0 || cur.Offset != 15 {
		t.Fatalf("sin nada nuevo: %q %+v %v", b, cur, err)
	}

	// Rotar: el fichero se trunca y vuelve a crecer más allá del cursor viejo.
	var buf bytes.Buffer
	for i := 0; buf.Len() <= consolaMaxBytes; i++ {
		fmt.Fprintf(&buf, "linea %08d\n", i)
	}
	escribirConsola(t, log, buf.String())
	if err := rotarConsola(m.dir(id)); err != nil {
		t.Fatal(err)
	}
	escribirConsola(t, log, "nuevo 1 tras rotar\nnuevo 2\n")
	b, cur2, err := m.LogsDesde(id, cur)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "nuevo 1 tras rotar\nnuevo 2\n" || cur2.Rotacion == cur.Rotacion {
		t.Fatalf("tras rotar: %q %+v (antes %+v)", b, cur2, cur)
	}
}

// Con más de logMaxBytes nuevos devuelve solo los últimos logMaxBytes.
func TestLogsDesdeAcotaLoNuevo(t *testing.T) {
	m := newTestManager(t)
	id := "logscur000000001"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.addForTest(id)
	log := filepath.Join(m.dir(id), "firecracker.log")
	escribirConsola(t, log, string(bytes.Repeat([]byte("x"), logMaxBytes+100)))
	b, cur, err := m.LogsDesde(id, LogCursor{Offset: 0, Rotacion: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != logMaxBytes || cur.Offset != logMaxBytes+100 {
		t.Fatalf("%d bytes, cursor %+v", len(b), cur)
	}
}

// P2: lo que cuesta una vuelta de `kling logs -f` con una consola llena (4 MiB)
// y nada nuevo: la cola de 2000 líneas (antes) frente al cursor (ahora).
func BenchmarkLogsSondeo(b *testing.B) {
	m := newTestManager(b)
	id := "logscur000000002"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		b.Fatal(err)
	}
	m.addForTest(id)
	var buf bytes.Buffer
	for i := 0; buf.Len() < logMaxBytes; i++ {
		fmt.Fprintf(&buf, "linea %08d del invitado\n", i)
	}
	if err := os.WriteFile(filepath.Join(m.dir(id), "firecracker.log"), buf.Bytes(), 0o644); err != nil {
		b.Fatal(err)
	}
	b.Run("cola", func(b *testing.B) {
		for range b.N {
			if _, err := m.Logs(id, 2000); err != nil {
				b.Fatal(err)
			}
		}
	})
	_, cur, err := m.LogsCola(id, 1)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("cursor", func(b *testing.B) {
		for range b.N {
			if _, _, err := m.LogsDesde(id, cur); err != nil {
				b.Fatal(err)
			}
		}
	})
}
