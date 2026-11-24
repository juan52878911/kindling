package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// esperarQue repite cond hasta que se cumple o vence el plazo.
func esperarQue(t *testing.T, plazo time.Duration, que string, cond func() bool) {
	t.Helper()
	for limite := time.Now().Add(plazo); !cond(); {
		if time.Now().After(limite) {
			t.Fatalf("no pasó a tiempo: %s", que)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// repuestosDe cuenta los repuestos listos del servicio.
func repuestosDe(g *Scheduler, service string) int { return g.Status(service).Spares }

// gwRepuesto arma un planificador contra el daemon de mentira con n repuestos
// por servicio y los ganchos que dejan ver el orden en que se calienta cada
// uno (después de AfterRestore).
func gwRepuesto(t *testing.T, n int) (*Scheduler, *daemonServicio, *atomic.Int32) {
	t.Helper()
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.SpareFor = func(string) int { return n }
	calentados := &atomic.Int32{}
	g.WarmSpare = func(context.Context, string, string) error {
		calentados.Add(1)
		return nil
	}
	t.Cleanup(func() { g.DrainSpares(context.Background()) })
	return g, d, calentados
}

// C9-08: con SpareFor = 1 hay siempre un repuesto restaurado y calentado. La
// petición que llega lo reclama sin restaurar en su camino, y el planificador
// repone otro en segundo plano.
func TestRepuestoSeMantieneYSeRepone(t *testing.T) {
	g, d, calentados := gwRepuesto(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.FillSpares(ctx, "svc")
	esperarQue(t, 3*time.Second, "un repuesto listo", func() bool { return repuestosDe(g, "svc") == 1 })
	if n := calentados.Load(); n != 1 {
		t.Fatalf("WarmSpare se llamó %d veces; quería 1 (antes de darlo por listo)", n)
	}
	antes := len(d.pedidasCopia())
	e, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if w := e.wake.Load(); w == nil || w.How != "spare" {
		t.Fatalf("la petición no usó el repuesto: %+v", w)
	}
	if e.machineID != "r1" {
		t.Fatalf("instancia %s; quería el repuesto r1", e.machineID)
	}
	esperarQue(t, 3*time.Second, "el repuesto repuesto", func() bool { return repuestosDe(g, "svc") == 1 })
	if got := len(d.pedidasCopia()) - antes; got != 1 {
		t.Errorf("POST /machines tras reclamar = %d; quería 1 (la reposición, fuera de la petición)", got)
	}
}

// Un repuesto no es una instancia ocupada: no está en el mapa de instancias,
// no cuenta para el tope de réplicas ni tiene trabajo en vuelo, y acquire no
// lo adopta como «una que ya corría».
func TestRepuestoNoCuentaComoOcupada(t *testing.T) {
	g, _, _ := gwRepuesto(t, 1)
	g.MaxReplicas = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.FillSpares(ctx, "svc")
	esperarQue(t, 3*time.Second, "un repuesto listo", func() bool { return repuestosDe(g, "svc") == 1 })
	st := g.Status("svc")
	if st.Warm {
		t.Error("Status dice que el servicio tiene primaria despierta con solo un repuesto")
	}
	if n := len(g.InflightByMachine()); n != 0 {
		t.Errorf("InflightByMachine tiene %d máquinas con solo un repuesto", n)
	}
	g.mu.Lock()
	n := len(g.entriesLocked("svc"))
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("el repuesto cuenta como instancia (%d en el mapa)", n)
	}
	// Con el tope en 1, la primaria (el repuesto reclamado) cabe.
	if _, err := g.ensure(ctx, "svc"); err != nil {
		t.Fatalf("ensure con tope 1 y un repuesto: %v", err)
	}
}

// Drain (y DrainSpares, que llama el gateway de IA al cerrar) borra los
// repuestos: nunca los congela.
func TestRepuestoSeBorraAlDrenar(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.SpareFor = func(string) int { return 2 }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.FillSpares(ctx, "svc")
	esperarQue(t, 3*time.Second, "dos repuestos", func() bool { return repuestosDe(g, "svc") == 2 })
	g.Drain(ctx)
	for _, id := range []string{"r1", "r2"} {
		if d.existe(id) {
			t.Errorf("el repuesto %s sigue vivo tras drenar", id)
		}
	}
	if n := d.contar("POST /machines/r"); n != 0 {
		t.Errorf("drenar hizo %d llamadas de congelar/pausar sobre los repuestos", n)
	}
	// Cerrado, no se repone nada.
	g.FillSpares(ctx, "svc")
	time.Sleep(50 * time.Millisecond)
	if n := len(d.pedidasCopia()); n != 2 {
		t.Errorf("POST /machines = %d tras drenar; no debía restaurarse nada más", n)
	}
}

// El repuesto pasa por AfterRestore y luego por WarmSpare antes de estar
// disponible; si WarmSpare falla, se descarta (se borra) en vez de servirse
// frío.
func TestRepuestoCalentadoAntesDeListo(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.SpareFor = func(string) int { return 1 }
	var mu sync.Mutex
	var orden []string
	g.AfterRestore = func(_ context.Context, _ string, mc *api.Machine) error {
		mu.Lock()
		orden = append(orden, "after:"+mc.ID)
		mu.Unlock()
		return nil
	}
	soltar := make(chan struct{})
	falla := atomic.Bool{}
	g.WarmSpare = func(_ context.Context, _, addr string) error {
		mu.Lock()
		orden = append(orden, "warm")
		mu.Unlock()
		<-soltar
		if falla.Load() {
			return errors.New("warmup answered 500")
		}
		return nil
	}
	t.Cleanup(func() { g.DrainSpares(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	falla.Store(true)
	g.FillSpares(ctx, "svc")
	esperarQue(t, 3*time.Second, "WarmSpare llamado", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(orden) == 2
	})
	if n := repuestosDe(g, "svc"); n != 0 {
		t.Fatalf("el repuesto está disponible (%d) antes de acabar de calentarse", n)
	}
	mu.Lock()
	if strings.Join(orden, ",") != "after:r1,warm" {
		t.Errorf("orden %v; quería AfterRestore y luego WarmSpare", orden)
	}
	mu.Unlock()
	close(soltar)
	esperarQue(t, 3*time.Second, "el repuesto que no se calentó, borrado", func() bool { return !d.existe("r1") })
	if n := repuestosDe(g, "svc"); n != 0 {
		t.Errorf("un repuesto cuyo calentamiento falló quedó disponible (%d)", n)
	}
}
