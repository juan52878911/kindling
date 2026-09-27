package scheduler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonAlive imita lo justo del daemon para distinguir GET /machines (List,
// recorre el disco de TODAS las máquinas) de GET /machines/<id> (Get, una
// sola). Registra si List() se llegó a llamar.
type daemonAlive struct {
	maquinas    map[string]*api.Machine
	listLlamado atomic.Bool
}

func (d *daemonAlive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/machines" {
		d.listLlamado.Store(true)
		l := []*api.Machine{}
		for _, mc := range d.maquinas {
			l = append(l, mc)
		}
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	if mc := d.maquinas[filepath.Base(r.URL.Path)]; mc != nil {
		_ = json.NewEncoder(w).Encode(mc)
		return
	}
	http.NotFound(w, r)
}

// gwConDaemonAlive levanta daemonAlive en un socket unix y un planificador que
// habla con él.
func gwConDaemonAlive(t *testing.T, d *daemonAlive) *Scheduler {
	t.Helper()
	// /tmp y no t.TempDir(): la ruta de un socket unix no puede pasar de ~104
	// bytes, y la de TempDir en macOS se acerca.
	dir, err := os.MkdirTemp("/tmp", "sch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: d}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return New(api.NewClient("unix://"+sock), time.Minute, false, 0)
}

// G-02: alive debe usar Get(id), no List(): List recorre el disco de TODAS
// las máquinas del daemon y ensure() llamaba a alive en cada thaw de una
// instancia con la caché de vida caducada, bajo el candado del servicio. Con
// 200+ máquinas era el tráfico dominante del socket del daemon.
func TestAliveUsaGetNoList(t *testing.T) {
	d := &daemonAlive{maquinas: map[string]*api.Machine{
		"m1": {ID: "m1", State: api.StateRunning},
	}}
	g := gwConDaemonAlive(t, d)

	if !g.alive(context.Background(), "m1") {
		t.Fatal("alive(m1) = false; la máquina está running")
	}
	if d.listLlamado.Load() {
		t.Error("alive llamó a List(); debe usar Get(id)")
	}
}

// Una máquina que ya no está running (o que Get no encuentra) no está viva.
func TestAliveFalseSiNoEstaRunning(t *testing.T) {
	d := &daemonAlive{maquinas: map[string]*api.Machine{
		"m1": {ID: "m1", State: api.StateWarm},
	}}
	g := gwConDaemonAlive(t, d)

	if g.alive(context.Background(), "m1") {
		t.Error("alive(m1) = true; la máquina está congelada, no corriendo")
	}
	if g.alive(context.Background(), "no-existe") {
		t.Error("alive(no-existe) = true; Get debía fallar con 404")
	}
	if d.listLlamado.Load() {
		t.Error("alive llamó a List() en algún caso; debe usar Get(id) siempre")
	}
}
