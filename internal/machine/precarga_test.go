package machine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Un thaw que aborta corta su precarga: precargar mira el ctx entre trozo y
// trozo y no sigue leyendo un volcado que ya nadie va a cargar.
func TestPrecargarParaConElCtx(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.file")
	if err := os.WriteFile(path, make([]byte, 8*precargaTrozo), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := precargar(context.Background(), path); n != 8*precargaTrozo {
		t.Fatalf("precargar leyó %d bytes de %d", n, 8*precargaTrozo)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := precargar(ctx, path); n != 0 {
		t.Fatalf("con el ctx cancelado precargar leyó %d bytes; quería 0", n)
	}
}

// Las precargas de thaw tienen tope: en una tormenta de thaws, las que no
// caben no se lanzan. Cada precarga se queda abriendo una FIFO sin escritor,
// así que ocupa su turno hasta que la prueba la suelta.
func TestPrecargarEnFondoTieneTope(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "mem.file")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if n := precargasEnCurso.Load(); n != 0 {
		t.Fatalf("%d precargas en curso antes de empezar", n)
	}
	lanzadas := 0
	for range 4 * precargasMax {
		if precargarEnFondo(context.Background(), fifo) {
			lanzadas++
		}
	}
	// Con un escritor, la FIFO se abre: las que esperan ven un fichero de
	// tamaño 0 y acaban. Se cierra cuando han acabado todas, para que ninguna
	// se quede esperando a otro escritor.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if lanzadas != precargasMax {
		t.Fatalf("se lanzaron %d precargas a la vez; el tope es %d", lanzadas, precargasMax)
	}
	limite := time.Now().Add(5 * time.Second)
	for precargasEnCurso.Load() != 0 {
		if time.Now().After(limite) {
			t.Fatalf("%d precargas siguen en curso", precargasEnCurso.Load())
		}
		time.Sleep(time.Millisecond)
	}
}
