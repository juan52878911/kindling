package machine

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// H2.1: un plazo agotado al sondear no prueba que no haya agente; solo un RST
// (o el cierre del VMM) lo prueba. Con "no sé", el thaw debe resincronizar.
func TestFreezeSinAgenteSoloConRST(t *testing.T) {
	orig := dialAgente
	t.Cleanup(func() { dialAgente = orig })
	m := newTestManager(t)
	id := maquinaEnFondo(t, m, "http://127.0.0.1:1")
	plazo := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	rst := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, c := range []struct {
		que    string
		err    error
		quiere bool
	}{
		{"plazo agotado", plazo, false},
		{"RST", rst, true},
		{"escucha", nil, false},
	} {
		dialAgente = func(string, time.Duration) (net.Conn, error) {
			if c.err != nil {
				return nil, c.err
			}
			a, b := net.Pipe()
			b.Close()
			return a, nil
		}
		if got := m.sinAgenteAlCongelar(context.Background(), id); got != c.quiere {
			t.Errorf("%s: sinAgente=%v, quería %v", c.que, got, c.quiere)
		}
	}
}

// H3: el plazo crece con los resync en vuelo por encima de NumCPU.
func TestPlazoResyncCarga(t *testing.T) {
	for _, c := range []struct {
		enVuelo, ncpu int
		quiere        time.Duration
	}{
		{0, 8, 6 * time.Second},
		{8, 8, 6 * time.Second},
		{9, 8, 6*time.Second + 250*time.Millisecond},
		{58, 8, 6*time.Second + 50*250*time.Millisecond},
		{1000, 8, 20 * time.Second},
	} {
		if got := plazoResyncCarga(c.enVuelo, c.ncpu); got != c.quiere {
			t.Errorf("plazo(%d,%d)=%v, quería %v", c.enVuelo, c.ncpu, got, c.quiere)
		}
	}
}

// agenteCortaEOF acepta la conexión y la cierra sin contestar (EOF).
func agenteCortaEOF(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// H2.3: un dorado con AllowExec tenía agente por fuerza: si corta la conexión,
// es un fallo. Sin AllowExec ni ConAgente, un EOF también (solo el RST es
// "nadie").
func TestResyncInstanciaCortePorAgente(t *testing.T) {
	for _, c := range []struct {
		que   string
		snap  api.Snapshot
		fallo bool
	}{
		{"AllowExec", api.Snapshot{AllowExec: true}, true},
		{"ConAgente", api.Snapshot{ConAgente: true}, true},
		{"nada, EOF", api.Snapshot{}, runtime.GOOS != "darwin"}, // en macOS un EOF es el reenvío sin nadie
	} {
		m := newTestManager(t)
		id := maquinaEnFondo(t, m, agenteCortaEOF(t))
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		res := m.resyncInstancia(ctx, id, &c.snap)
		cancel()
		if res.fallo != c.fallo {
			t.Errorf("%s: fallo=%v, quería %v", c.que, res.fallo, c.fallo)
		}
	}
	// Un RST sigue siendo "nadie": no es fallo.
	m := newTestManager(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	id := maquinaEnFondo(t, m, url)
	if res := m.resyncInstancia(context.Background(), id, &api.Snapshot{}); res.fallo {
		t.Error("RST sin AllowExec ni ConAgente: no debería ser fallo")
	}
}

// agenteQueFalla contesta 500 las primeras n veces y luego 200.
func agenteQueFalla(t *testing.T, n int32) (string, *atomic.Int32) {
	t.Helper()
	var vistos atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vistos.Add(1) <= n {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"skew_ms":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &vistos
}

// H2.2: un thaw cuyo resync falla reintenta con el plazo largo si hay agente
// conocido; si sigue fallando, avisa y sigue en segundo plano hasta que entra.
func TestResyncTrasThawReintentaYSigueEnFondo(t *testing.T) {
	antes := resyncThawEspera
	resyncThawEspera = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { resyncThawEspera = antes })

	m := newTestManager(t)
	url, vistos := agenteQueFalla(t, 2)
	id := maquinaEnFondo(t, m, url)
	res, aviso := m.resyncTrasThaw(context.Background(), id, true)
	if !res.fallo || aviso == "" {
		t.Fatalf("fallo=%v aviso=%q: quería fallo con aviso", res.fallo, aviso)
	}
	if n := vistos.Load(); n != 2 {
		t.Errorf("%d intentos antes de entregar, quería 2 (corto + largo)", n)
	}
	ej, _ := m.ejecucionDe(id)
	m.resyncThawReintentos(id, ej, true)
	if n := vistos.Load(); n != 3 {
		t.Errorf("%d intentos en total, quería 3 (el de fondo entra)", n)
	}

	// Sin AllowExec ni agente conocido: un solo intento antes de entregar.
	m2 := newTestManager(t)
	url2, vistos2 := agenteQueFalla(t, 1)
	id2 := maquinaEnFondo(t, m2, url2)
	m2.mu.Lock()
	m2.byID[id2].Agent = nil
	m2.mu.Unlock()
	if _, aviso := m2.resyncTrasThaw(context.Background(), id2, false); aviso == "" || vistos2.Load() != 1 {
		t.Errorf("aviso=%q intentos=%d, quería aviso y 1 intento", aviso, vistos2.Load())
	}
}

// H3: la evidencia sale de /proc/<pid>/task/*/schedstat, solo de los hilos vCPU.
func TestDiagnosticoInvitado(t *testing.T) {
	raiz := t.TempDir()
	antes := procRaiz
	procRaiz = raiz
	t.Cleanup(func() { procRaiz = antes })
	tarea := func(tid, comm, sched string) {
		d := filepath.Join(raiz, "4242", "task", tid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "schedstat"), []byte(sched+"\n"), 0o644)
	}
	tarea("4242", "firecracker", "9000000000 0 1")
	tarea("4243", "fc_vcpu 0", "1000000000 200000000 5")
	tarea("4244", "fc_vcpu 1", "2000000000 0 3")
	linea := diagnosticoInvitado(4242, "rx 5 tx 7", func() {
		tarea("4243", "fc_vcpu 0", "1500000000 700000000 9")
	})
	for _, quiere := range []string{"vcpus=2", "cpu_delta=500ms", "wait_delta=500ms", "cpu_total=3.5s", "net=rx 5 tx 7"} {
		if !strings.Contains(linea, quiere) {
			t.Errorf("falta %q en %q", quiere, linea)
		}
	}
	if l := diagnosticoInvitado(9999, "", func() {}); !strings.Contains(l, "no /proc") {
		t.Errorf("sin proceso: %q", l)
	}
}
