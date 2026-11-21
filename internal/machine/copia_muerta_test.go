package machine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// dosCopias monta un Manager con las máquinas "a" y "b", cuyos agentes son ha
// y hb.
func dosCopias(t *testing.T, ha, hb http.Handler) *Manager {
	t.Helper()
	m := &Manager{byID: map[string]*api.Machine{}}
	for id, h := range map[string]http.Handler{"a": ha, "b": hb} {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		m.byID[id] = &api.Machine{ID: id, Name: id, Image: "img",
			Forwards: map[string]string{"8080": strings.TrimPrefix(srv.URL, "http://")}}
	}
	return m
}

// agenteMudo no contesta hasta que se cierra liberar; el cierre debe
// registrarse DESPUÉS de dosCopias, para que httptest no espere a un
// manejador bloqueado (los Cleanup van en orden inverso).
func agenteMudo(liberar chan struct{}, vistos *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vistos.Add(1)
		select {
		case <-liberar:
		case <-r.Context().Done():
		}
	})
}

func agenteVivo() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"skew_ms":0}`))
	})
}

// intentoDe devuelve un intento de restauración falso: el n-ésimo usa la
// máquina ids[n] y el VMM pids[n], y hace lo que runFrom con el resultado de
// resyncCopia.
func intentoDe(m *Manager, snap *api.Snapshot, ids []string, pids []int, n *int) func() (*api.Machine, error) {
	return func() (*api.Machine, error) {
		i := *n
		*n++
		res, err := m.resyncCopia(context.Background(), ids[i], snap, pids[i])
		if err != nil {
			esperarDiagnostico(res.diag)
			return nil, err
		}
		return &api.Machine{ID: ids[i]}, nil
	}
}

func fingirProc(t *testing.T, pid string) {
	t.Helper()
	raiz := t.TempDir()
	d := filepath.Join(raiz, pid, "task", pid)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(d, "comm"), []byte("fc_vcpu 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "schedstat"), []byte("1000000 0 1\n"), 0o644)
	antesProc, antesRed, antesEsp := procRaiz, leerRedInvitado, diagEspera
	procRaiz = raiz
	leerRedInvitado = func(string) string { return "neigh[FAILED]" }
	diagEspera = func() {}
	t.Cleanup(func() { procRaiz, leerRedInvitado, diagEspera = antesProc, antesRed, antesEsp })
}

// Una copia muda se descarta y otra se entrega; el diagnóstico de la descartada
// recibe el pid de SU VMM (antes leía un 0 de la máquina: "no pid to inspect").
func TestCopiaMudaSeSustituyePorOtra(t *testing.T) {
	t.Setenv("KLING_RESYNC_FIRST_MS", "200")
	fingirProc(t, "4242")
	var mudos atomic.Int32
	liberar := make(chan struct{})
	m := dosCopias(t, agenteMudo(liberar, &mudos), agenteVivo())
	t.Cleanup(func() { close(liberar) })
	log := capturarLog(t)
	antes := CopiasMuertas()
	snap := &api.Snapshot{Name: "dorado", CreatedAt: time.Now(), ConAgente: true}
	var n int
	mc, err := conCopiaDeRepuesto(context.Background(), "web", intentoDe(m, snap, []string{"a", "b"}, []int{4242, 4343}, &n))
	if err != nil || mc == nil || mc.ID != "b" {
		t.Fatalf("mc=%v err=%v, quería la segunda copia", mc, err)
	}
	if n != 2 || mudos.Load() != 1 {
		t.Errorf("intentos=%d POST a la muda=%d, quería 2 y 1", n, mudos.Load())
	}
	if d := CopiasMuertas() - antes; d != 1 {
		t.Errorf("copias descartadas: %d, quería 1", d)
	}
	out := log.String()
	if strings.Contains(out, "no pid to inspect") || !strings.Contains(out, "vcpus=1") || !strings.Contains(out, "net=neigh[FAILED]") {
		t.Errorf("el diagnóstico no usó el pid de la primera copia: %q", out)
	}
}

// Dos copias mudas: el error de hoy, en menos de 2 plazos y un margen (no 12 s).
func TestDosCopiasMudasFallanRapido(t *testing.T) {
	t.Setenv("KLING_RESYNC_FIRST_MS", "200")
	fingirProc(t, "4242")
	var mudos atomic.Int32
	liberar := make(chan struct{})
	m := dosCopias(t, agenteMudo(liberar, &mudos), agenteMudo(liberar, &mudos))
	t.Cleanup(func() { close(liberar) })
	capturarLog(t)
	snap := &api.Snapshot{Name: "dorado", CreatedAt: time.Now(), ConAgente: true}
	var n int
	ini := time.Now()
	_, err := conCopiaDeRepuesto(context.Background(), "web", intentoDe(m, snap, []string{"a", "b"}, []int{4242, 4242}, &n))
	if !errors.Is(err, errCopiaMuerta) {
		t.Fatalf("err=%v, quería errCopiaMuerta", err)
	}
	if d := time.Since(ini); d > 2*200*time.Millisecond+600*time.Millisecond {
		t.Errorf("tardó %v", d)
	}
	if n != 2 {
		t.Errorf("intentos=%d, quería exactamente 2 (una repetición)", n)
	}
}

// Un fallo que no es de copia muerta no se repite.
func TestOtroErrorNoRepite(t *testing.T) {
	n := 0
	_, err := conCopiaDeRepuesto(context.Background(), "web", func() (*api.Machine, error) {
		n++
		return nil, errors.New("disk full")
	})
	if err == nil || n != 1 {
		t.Fatalf("err=%v intentos=%d", err, n)
	}
}

// persistirYa en paralelo: no alarga la creación y no se devuelve antes de que
// acabe; su error se propaga.
func TestEnParaleloNoAlargaYEspera(t *testing.T) {
	var hecho atomic.Bool
	ini := time.Now()
	esperar := enParalelo(func() error {
		time.Sleep(50 * time.Millisecond)
		hecho.Store(true)
		return errors.New("fsync")
	})
	time.Sleep(50 * time.Millisecond) // el resto de la creación
	if err := esperar(); err == nil || !hecho.Load() {
		t.Fatalf("devolvió antes de acabar o perdió el error: err=%v hecho=%v", err, hecho.Load())
	}
	if d := time.Since(ini); d > 95*time.Millisecond {
		t.Errorf("la creación se alargó a %v (quería ~50 ms, no 100)", d)
	}
	if err := esperar(); err == nil {
		t.Error("la segunda espera perdió el error")
	}
}
