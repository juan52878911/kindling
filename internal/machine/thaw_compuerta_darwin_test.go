//go:build darwin

package machine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// P3: con la memoria cargada, el thaw suelta la compuerta de lanzamiento
// antes del resync, que espera al invitado y no al host.
func TestThawSueltaLaCompuertaAntesDelResync(t *testing.T) {
	m, _ := managerVZ(t)
	m.launchGate = make(chan struct{}, 1)
	id := "c0e7a00000000001"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-compuerta", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}
	libre := false
	m.pruebasAntesDeResync = func() {
		select {
		case m.launchGate <- struct{}{}:
			<-m.launchGate
			libre = true
		default:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if !libre {
		t.Fatal("el thaw retuvo la compuerta de lanzamiento durante el resync")
	}
}

// Medición de P3: cuánto retiene un thaw la compuerta de lanzamiento con un
// agente que tarda 200 ms en resincronizar (un invitado lento bajo carga).
// Es lo que espera el thaw siguiente cuando la compuerta está llena.
// go test ./internal/machine -run x -bench ThawConResyncLento -benchtime 5x
func BenchmarkThawConResyncLento(b *testing.B) {
	var retenida time.Duration
	for b.Loop() {
		b.StopTimer()
		m, ids := thawsPreparados(b, 1)
		b.StartTimer()
		hecho := make(chan struct{})
		go func() {
			defer close(hecho)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			out, err := m.Thaw(ctx, ids[0])
			if err != nil {
				b.Error(err)
				return
			}
			matarVMM(out.PID)
		}()
		// En cuanto el thaw la toma, el siguiente espera su turno.
		for len(m.launchGate) == 0 {
			time.Sleep(100 * time.Microsecond)
		}
		t0 := time.Now()
		m.launchGate <- struct{}{}
		retenida += time.Since(t0)
		<-m.launchGate
		<-hecho
	}
	b.ReportMetric(float64(retenida.Milliseconds())/float64(b.N), "gate_ms/op")
}

// thawsPreparados deja n máquinas warm con su volcado sellado en un manager
// con kling-vz falso, la compuerta en 1 y un agente que tarda 200 ms en
// contestar al resync.
func thawsPreparados(b *testing.B, n int) (*Manager, []string) {
	b.Helper()
	agente := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestResyncPath {
			http.NotFound(w, r)
			return
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"skew_ms":0}`))
	}))
	b.Cleanup(agente.Close)
	b.Setenv(envFakeVZGuest, strings.TrimPrefix(agente.URL, "http://"))
	m, _ := managerVZ(b)
	m.launchGate = make(chan struct{}, 1)
	var ids []string
	for i := range n {
		id := fmt.Sprintf("c0e7b%011x", i)
		dir := m.dir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		volcadoSellado(b, dir)
		m.byID[id] = &api.Machine{ID: id, Name: "vz-lento-" + id[11:], State: api.StateWarm, Egress: "none",
			IP: knet.GuestIP, CreatedAt: time.Now()}
		ids = append(ids, id)
	}
	return m, ids
}

// P2: el sello se retira a la vez que arranca el VMM. Si el VMM no llega a
// arrancar, no se cargó nada: el volcado vuelve a quedar sellado.
func TestThawQueNoArrancaConservaElSello(t *testing.T) {
	m, _ := managerVZ(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	id := "c0e7a00000000002"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-sin-vmm", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := m.Thaw(ctx, id); err == nil {
		matarVMM(out.PID)
		t.Fatal("el thaw arrancó sin VMM")
	}
	if _, err := os.Stat(filepath.Join(dir, marcaOK)); err != nil {
		t.Fatalf("sin cargar nada, el volcado perdió su sello: %v", err)
	}
	if err := volcadoValido(dir, nil); err != nil {
		t.Fatalf("sin cargar nada, el volcado debería seguir valiendo: %v", err)
	}
}
