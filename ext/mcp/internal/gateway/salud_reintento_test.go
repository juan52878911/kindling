package gateway

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Si anotar un fallo de salud no llega al daemon (503), el siguiente fallo
// del mismo servicio lo vuelve a intentar. Antes el estado en memoria ya decía
// "roto" y no se escribía nunca más: un PUT fallido y cinco fallos sin anotar.
func TestLaSaludSeReintentaSiNoSePudoAnotar(t *testing.T) {
	dir, err := os.MkdirTemp("", "kg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var puts atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if puts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	g := New(api.NewClient(sock), 0, false, 0, "")
	esperar := func(cond func() bool) {
		t.Helper()
		for fin := time.Now().Add(2 * time.Second); time.Now().Before(fin); time.Sleep(5 * time.Millisecond) {
			if cond() {
				return
			}
		}
	}
	olvidado := func() bool {
		g.saludMu.Lock()
		defer g.saludMu.Unlock()
		_, hay := g.saludVista["s"]
		return !hay
	}

	g.anotarFallo("s", errors.New("restore failed"))
	esperar(func() bool { return puts.Load() == 1 && olvidado() })
	for i := 0; i < 5; i++ {
		g.anotarFallo("s", errors.New("restore failed"))
		esperar(func() bool { return puts.Load() >= 2 })
	}
	time.Sleep(50 * time.Millisecond)
	if n := puts.Load(); n != 2 {
		t.Fatalf("PUTs = %d, want 2 (the failed one and one retry, then nothing while it doesn't change)", n)
	}
}
