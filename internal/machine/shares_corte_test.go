package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// runFalso pone en el supervisor una ejecución de carpetas de id con una sola
// goroutine. Si atascada, esa goroutine no hace caso del corte (una operación
// colgada en el host) hasta que el test acaba.
func runFalso(t *testing.T, m *Manager, id string, atascada bool) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &shareRun{cancel: cancel, done: make(chan struct{}), drain: make(chan struct{})}
	soltar := make(chan struct{})
	t.Cleanup(func() { close(soltar); <-run.done })
	run.wg.Add(1)
	go func() {
		defer run.wg.Done()
		if atascada {
			<-soltar
			return
		}
		<-ctx.Done()
	}()
	go func() { run.wg.Wait(); close(run.done) }()
	sup := m.sup()
	sup.mu.Lock()
	sup.runs[id] = run
	sup.mu.Unlock()
	return ctx
}

// F7: al parar, las carpetas vivas se cortaban antes de pedirle al invitado que
// parase su servicio. Lo que el servicio escribiera al recibir su señal, y lo
// que el agente tuviera diferido (sale al cerrar los ficheros), fallaba con la
// sesión ya cortada. Ahora el servicio para con las carpetas aún conectadas.
func TestKillParaElServicioAntesDeCortarLasCarpetas(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	ctx := runFalso(t, m, id, false)
	var paradas, cortadaAntes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == api.GuestServiceStopPath {
			paradas.Add(1)
			if ctx.Err() != nil {
				cortadaAntes.Add(1)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	pid, _ := procesoVivo(t)
	m.byID[id] = &api.Machine{ID: id, Name: "svc", State: api.StateRunning, PID: pid,
		Agent:    &api.GuestAgent{Caps: []string{api.GuestCapReady, api.GuestCapService}},
		Forwards: map[string]string{strconv.Itoa(api.GuestPort): strings.TrimPrefix(srv.URL, "http://")}}

	if !m.kill(id) {
		t.Fatal("kill no mató el proceso")
	}
	if paradas.Load() != 1 {
		t.Fatalf("%d paradas del servicio, want 1", paradas.Load())
	}
	if cortadaAntes.Load() != 0 {
		t.Fatal("las carpetas vivas se cortaron antes de parar el servicio")
	}
	if ctx.Err() == nil {
		t.Fatal("kill no cortó las carpetas vivas")
	}
}

// R7: una carpeta cuyo host no contesta (NFS colgado) dejaba stop, freeze y rm
// esperando para siempre a que terminase su sesión. Ahora se espera un plazo.
func TestCorteDeCarpetasConPlazo(t *testing.T) {
	viejo := shareCutWait
	shareCutWait = 50 * time.Millisecond
	t.Cleanup(func() { shareCutWait = viejo })
	for _, c := range []struct {
		nombre string
		cortar func(m *Manager, id string)
	}{
		{"stopShares", (*Manager).stopShares},
		{"drainSharesOf", (*Manager).drainSharesOf},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			m := newTestManager(t)
			id := newID()
			runFalso(t, m, id, true)
			hecho := make(chan struct{})
			go func() { c.cortar(m, id); close(hecho) }()
			// drainSharesOf espera además su cierre ordenado (shareDrainWait).
			select {
			case <-hecho:
			case <-time.After(shareDrainWait + 3*time.Second):
				t.Fatalf("%s sigue esperando a una carpeta atascada", c.nombre)
			}
		})
	}
}
