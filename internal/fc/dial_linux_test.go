package fc

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// servirEn pone un Firecracker falso a escuchar en sock.
func servirEn(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); ln.Close() })
}

func dirCorto(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// M3: un socket cambiado por un enlace simbólico (al de otro VMM) se rechaza
// en vez de seguirlo.
func TestDialVMMNoSigueUnEnlaceEnElSocket(t *testing.T) {
	dir := dirCorto(t)
	otro := filepath.Join(dir, "otro", "fc.sock")
	servirEn(t, otro)
	if err := New(otro).Ping(context.Background()); err != nil {
		t.Fatalf("el socket de verdad: %v", err)
	}
	suplantado := filepath.Join(dir, "a", "fc.sock")
	os.MkdirAll(filepath.Dir(suplantado), 0o755)
	if err := os.Symlink(otro, suplantado); err != nil {
		t.Fatal(err)
	}
	err := New(suplantado).Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("un enlace en lugar del socket se siguió: %v", err)
	}
}

// M3: el socket de una jaula (run/firecracker.socket) que atiende un proceso
// sin jaula —aquí, el propio test— se rechaza; también si se llega a él por un
// run/ cambiado por un enlace a la run/ de otra jaula.
func TestDialVMMExigeQueElParEsteEnSuJaula(t *testing.T) {
	dir := dirCorto(t)
	jaulaB := filepath.Join(dir, "b", "root")
	sockB := filepath.Join(jaulaB, "run", "firecracker.socket")
	servirEn(t, sockB)
	err := New(sockB).Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not jailed") {
		t.Fatalf("un socket de jaula atendido sin jaula pasó: %v", err)
	}

	jaulaA := filepath.Join(dir, "a", "root")
	os.MkdirAll(jaulaA, 0o755)
	if err := os.Symlink(filepath.Join(jaulaB, "run"), filepath.Join(jaulaA, "run")); err != nil {
		t.Fatal(err)
	}
	if err := New(filepath.Join(jaulaA, "run", "firecracker.socket")).Ping(context.Background()); err == nil {
		t.Fatal("el socket de A por un run/ enlazado a la jaula de B pasó")
	}

	// Con la comprobación apagada (solo tests), el mismo socket contesta: lo
	// que falla arriba es la comprobación, no la conexión.
	ComprobarJaula = false
	defer func() { ComprobarJaula = true }()
	if err := New(sockB).Ping(context.Background()); err != nil {
		t.Fatalf("sin comprobar: %v", err)
	}
}

// Conectar por /proc/self/fd/N no depende del largo de la ruta.
func TestDialVMMRutaLargaSinEnlaceCorto(t *testing.T) {
	dir := dirCorto(t)
	sock := filepath.Join(dir, strings.Repeat("a", 60), strings.Repeat("b", 60), "fc.sock")
	cwd, _ := os.Getwd()
	os.MkdirAll(filepath.Dir(sock), 0o755)
	if err := os.Chdir(filepath.Dir(sock)); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", "fc.sock")
	_ = os.Chdir(cwd)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := New(sock).Ping(context.Background()); err != nil {
		t.Fatalf("ruta de %d bytes: %v", len(sock), err)
	}
}
