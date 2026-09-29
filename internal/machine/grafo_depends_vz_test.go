//go:build darwin

package machine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// kvProbeFalso sirve GET /kling/probe en un socket Unix como kling-vz: el
// puerto contesta a partir de la llamada abreEn (0 = nunca).
func kvProbeFalso(t *testing.T, abreEn int32) (string, *atomic.Int32, *atomic.Value) {
	t.Helper()
	// Ruta corta: sun_path en macOS son 104 bytes y t.TempDir() se pasa.
	dir, err := os.MkdirTemp("/tmp", "kvp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var llamadas atomic.Int32
	var puerto atomic.Value
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kling/probe" {
			http.NotFound(w, r)
			return
		}
		puerto.Store(r.URL.Query().Get("port"))
		n := llamadas.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if abreEn != 0 && n >= abreEn {
			_, _ = w.Write([]byte(`{"open":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"open":false}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &llamadas, &puerto
}

// En macOS, una arista depends con puerto espera preguntando al kling-vz del
// destino (KlingProbe) hasta que el puerto escucha, sin dirección ni reenvío.
func TestEsperarPuertoDependsEnMac(t *testing.T) {
	m := newTestManager(t)
	sock, llamadas, puerto := kvProbeFalso(t, 3)
	m.mu.Lock()
	m.socket["idb"] = sock
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := esperarPuertoPlataforma(ctx, m, "idb", "", 8081); err != nil {
		t.Fatal(err)
	}
	if n := llamadas.Load(); n < 3 {
		t.Errorf("preguntó %d veces, quería al menos 3", n)
	}
	if p, _ := puerto.Load().(string); p != "8081" {
		t.Errorf("preguntó por el puerto %q", p)
	}

	// Un puerto que no llega a escuchar agota el plazo con un error.
	sock2, _, _ := kvProbeFalso(t, 0)
	m.mu.Lock()
	m.socket["idc"] = sock2
	m.mu.Unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if err := esperarPuertoPlataforma(ctx2, m, "idc", "", 8081); err == nil {
		t.Fatal("un puerto mudo no puede darse por listo")
	}
	if addr, err := direccionListoLocked(nil, 8081); addr != "" || err != nil {
		t.Errorf("macOS no marca a ninguna dirección: %q %v", addr, err)
	}
}
