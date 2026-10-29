//go:build darwin

package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// B12: si quien pidió el thaw se va con la memoria ya cargada, el thaw acaba
// igual y la máquina se resincroniza. Antes, con el ctx cancelado, resyncGuest
// volvía sin llamar al agente y la máquina quedaba running con el reloj y el
// CSPRNG de cuando se congeló.
func TestVZThawCanceladoTrasCargarResincroniza(t *testing.T) {
	m, _ := managerVZ(t)
	var resyncs atomic.Int32
	agente := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestResyncPath {
			http.NotFound(w, r)
			return
		}
		resyncs.Add(1)
		_, _ = w.Write([]byte(`{"skew_ms":0}`))
	}))
	agente.Listener.Close()
	agente.Listener = escucharReservado(t)
	agente.Start()
	defer agente.Close()
	t.Setenv(envFakeVZGuest, strings.TrimPrefix(agente.URL, "http://"))

	id := "0123abcd4567ef8a"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-cancel", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m.pruebasAntesDeResync = cancel
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if out.State != api.StateRunning {
		t.Fatalf("estado = %s", out.State)
	}
	if n := resyncs.Load(); n != 1 {
		t.Fatalf("el agente recibió %d resync con el ctx cancelado, quería 1", n)
	}
}
