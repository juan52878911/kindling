package scheduler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// En macOS la máquina trae reenvíos y su puerto de loopback acepta siempre:
// esperar a que escuche tiene que preguntar al daemon (probe_only), no
// conectar a la dirección. Aquí el reenvío acepta pero el daemon dice que
// dentro no escucha nadie: esperarListo tiene que fallar.
func TestEsperarListoConReenviosPreguntaAlDaemon(t *testing.T) {
	reenvio, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer reenvio.Close()

	// En /tmp y no en t.TempDir(): en macOS éste pasa de los 104 bytes de
	// sun_path.
	dir, err := os.MkdirTemp("/tmp", "sch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	var visto api.GuestRequest
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/machines/m1/guest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&visto)
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"message":"nobody is listening on port 8080 inside m1"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	g := New(api.NewClient("unix://"+sock), time.Minute, false, 0)
	mc := &api.Machine{ID: "m1", Name: "m1", Forwards: map[string]string{"8080": reenvio.Addr().String()}}
	err = g.esperarListo(context.Background(), mc, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "nobody is listening") {
		t.Fatalf("err = %v; quería el 504 del daemon", err)
	}
	if !visto.ProbeOnly || visto.WaitMS != 300 {
		t.Fatalf("petición al daemon = %+v; quería probe_only y wait_ms 300", visto)
	}
}
