//go:build darwin

package machine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/fc"
)

// klingVZRespuestas es un kling-vz falso que contesta a cada PUT
// /kling/forwards con el siguiente mapa de la lista (el último se repite).
func klingVZRespuestas(t *testing.T, respuestas ...map[string]string) *fc.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kfw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	var mu sync.Mutex
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kling/forwards" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		fwd := respuestas[0]
		if len(respuestas) > 1 {
			respuestas = respuestas[1:]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"forwards": fwd})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return fc.New(sock)
}

// Un kling-vz tomado por su invitado no puede quedarse con el reenvío vivo de
// otra máquina: ni anunciándolo al arrancar (sin reserva previa, un kling-vz
// anterior) ni cambiando, tras arrancar, el que abrió antes de que corriera
// el invitado.
func TestReenviosDeOtraMaquinaSeRechazan(t *testing.T) {
	m := newTestManager(t)
	victima := m.addForTest("victima")
	m.addForTest("m1")
	m.mu.Lock()
	victima.Forwards = map[string]string{"22": "127.0.0.1:29001"}
	m.mu.Unlock()
	ctx := context.Background()

	// Sin reserva previa (un kling-vz anterior: "not up yet" se toma por
	// error y se sigue), el de después de arrancar es el de la víctima.
	c := klingVZRespuestas(t, map[string]string{"22": "127.0.0.1:29001", "8080": "127.0.0.1:29002"})
	if err := m.abrirReenvios(ctx, c, "m1"); err == nil || !strings.Contains(err.Error(), "victima") {
		t.Fatalf("reenvío de otra máquina aceptado: %v", err)
	}
	if fwd := m.byID["m1"].Forwards; len(fwd) != 0 {
		t.Fatalf("se guardaron los reenvíos rechazados: %v", fwd)
	}

	// Con reserva previa buena, y después otro puerto: se rechaza aunque el
	// nuevo no sea de nadie.
	bueno := map[string]string{"22": "127.0.0.1:29010", "8080": "127.0.0.1:29011"}
	movido := map[string]string{"22": "127.0.0.1:29012", "8080": "127.0.0.1:29011"}
	c = klingVZRespuestas(t, bueno, movido)
	if err := m.reservarReenvios(ctx, c, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := m.abrirReenvios(ctx, c, "m1"); err == nil || !strings.Contains(err.Error(), "after the guest started") {
		t.Fatalf("reenvío movido tras arrancar aceptado: %v", err)
	}

	// Y la reserva previa tampoco puede ser de otra.
	c = klingVZRespuestas(t, map[string]string{"22": "127.0.0.1:29001", "8080": "127.0.0.1:29011"})
	if err := m.reservarReenvios(ctx, c, "m1"); err == nil || !strings.Contains(err.Error(), "victima") {
		t.Fatalf("reserva sobre el reenvío de otra máquina aceptada: %v", err)
	}

	// Lo normal: los mismos antes y después.
	c = klingVZRespuestas(t, bueno)
	if err := m.reservarReenvios(ctx, c, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := m.abrirReenvios(ctx, c, "m1"); err != nil {
		t.Fatal(err)
	}
	if got := m.byID["m1"].Forwards["22"]; got != bueno["22"] {
		t.Fatalf("Forwards[22] = %q, quería %q", got, bueno["22"])
	}
}
