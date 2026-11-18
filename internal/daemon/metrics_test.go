package daemon

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// /metrics expone lo que se ve cuando las cosas van mal: operaciones por
// resultado, rechazos de admisión, latencias, el trabajo del vigilante y los
// eventos perdidos. Antes solo había máquinas por estado y memoria.
func TestMetricsExponeLaTelemetria(t *testing.T) {
	_, h := testServer(t)
	rr := call(t, h, "GET", "/metrics", "")
	if rr.Code != 200 {
		t.Fatalf("GET /metrics = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, serie := range []string{
		`kling_operations_total{op="run",result="ok"}`,
		`kling_operations_total{op="freeze",result="error"}`,
		`kling_admission_rejections_total{code="507"}`,
		`kling_admission_rejections_total{code="503"}`,
		`kling_admission_rejections_total{code="409"}`,
		`kling_admission_rejections_total{code="429"}`,
		`kling_operation_duration_ms_bucket{kind="thaw",le="+Inf"}`,
		`kling_operation_duration_ms_count{kind="boot"}`,
		`kling_gc_evictions_total`,
		`kling_orphan_vmms_killed_total`,
		`kling_events_dropped_total`,
		`kling_disk_free_mib`,
		`kling_pending_mib`,
	} {
		if !strings.Contains(body, serie) {
			t.Errorf("falta la serie %s", serie)
		}
	}
}

// P6: /metrics muestreaba todas las máquinas en cada scrape, también los que
// llegaban a la vez. Ahora una muestra sirve durante plazoMuestraMetrics y
// los scrapes simultáneos esperan a la misma: 50 en paralelo cuestan una
// muestra, no 50. Pasado el plazo, la siguiente vuelve a muestrear.
func TestMetricsMuestraCompartida(t *testing.T) {
	s, h := testServer(t)
	var muestras atomic.Int32
	s.muestrear = func() api.ProcStats {
		muestras.Add(1)
		time.Sleep(20 * time.Millisecond) // lo que tarda leer N smaps_rollup
		return api.ProcStats{Machines: []api.ProcStat{{ID: "aaaa000000000001", Name: "m", PID: 1, PSSMiB: 7}}}
	}
	viejo := plazoMuestraMetrics
	plazoMuestraMetrics = time.Hour
	t.Cleanup(func() { plazoMuestraMetrics = viejo })

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rr := call(t, h, "GET", "/metrics", ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `kling_machine_pss_mib{id="aaaa000000000001"`) {
				t.Errorf("GET /metrics = %d", rr.Code)
			}
		}()
	}
	wg.Wait()
	if n := muestras.Load(); n != 1 {
		t.Fatalf("50 scrapes simultáneos hicieron %d muestras; con la caché, 1", n)
	}
	plazoMuestraMetrics = 0
	call(t, h, "GET", "/metrics", "")
	if n := muestras.Load(); n != 2 {
		t.Fatalf("pasado el plazo no volvió a muestrear: %d muestras", n)
	}
}

// P4: el cerrojo de la muestra se tenía mientras se muestreaba, y una
// lectura colgada dejaba cada scrape esperando para siempre. Ahora esperan
// como mucho esperaMuestraMetrics y se llevan la muestra anterior; la
// colgada sigue siendo una sola, y cuando acaba la usan los siguientes.
func TestMetricsMuestraColgadaNoBloquea(t *testing.T) {
	s, h := testServer(t)
	soltar := make(chan struct{})
	var muestras atomic.Int32
	s.muestrear = func() api.ProcStats {
		muestras.Add(1)
		<-soltar
		return api.ProcStats{Machines: []api.ProcStat{{ID: "aaaa000000000001", Name: "m", PID: 1, PSSMiB: 7}}}
	}
	vp, ve := plazoMuestraMetrics, esperaMuestraMetrics
	plazoMuestraMetrics, esperaMuestraMetrics = time.Hour, 50*time.Millisecond
	t.Cleanup(func() { plazoMuestraMetrics, esperaMuestraMetrics = vp, ve })

	hechos := make(chan int, 10)
	for range 10 {
		go func() { hechos <- call(t, h, "GET", "/metrics", "").Code }()
	}
	for range 10 {
		select {
		case code := <-hechos:
			if code != 200 {
				t.Fatalf("GET /metrics = %d", code)
			}
		case <-time.After(5 * time.Second):
			close(soltar)
			t.Fatal("con la muestra colgada, /metrics no contesta")
		}
	}
	if n := muestras.Load(); n != 1 {
		t.Fatalf("con una muestra colgada se empezaron %d", n)
	}
	close(soltar)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(call(t, h, "GET", "/metrics", "").Body.String(), `kling_machine_pss_mib{id="aaaa000000000001"`) {
		if time.Now().After(deadline) {
			t.Fatal("acabada la muestra, /metrics no la usa")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := muestras.Load(); n != 1 {
		t.Fatalf("muestras: %d", n)
	}
}

// Los cubos del histograma van acumulados, como pide el formato de
// Prometheus; el +Inf es la cuenta total.
func TestEscribirTelemetriaCubosAcumulados(t *testing.T) {
	cubos := make([]int64, len(machine.LimitesDuracionMS)+1)
	cubos[0], cubos[2], cubos[len(cubos)-1] = 1, 2, 3
	tl := machine.Telemetria{
		Duraciones:    map[string]machine.Histograma{machine.DurThaw: {Cubos: cubos, Suma: 42, Cuenta: 6}},
		Rechazos:      map[int]int64{api.StatusDiskFull: 4},
		DiscoLibreMiB: -1,
	}
	var b bytes.Buffer
	escribirTelemetria(&b, tl, 7)
	out := b.String()
	for _, l := range []string{
		`kling_operation_duration_ms_bucket{kind="thaw",le="5"} 1`,
		`kling_operation_duration_ms_bucket{kind="thaw",le="10"} 1`,
		`kling_operation_duration_ms_bucket{kind="thaw",le="25"} 3`,
		`kling_operation_duration_ms_bucket{kind="thaw",le="30000"} 3`,
		`kling_operation_duration_ms_bucket{kind="thaw",le="+Inf"} 6`,
		`kling_operation_duration_ms_sum{kind="thaw"} 42`,
		`kling_admission_rejections_total{code="503"} 4`,
		`kling_events_dropped_total 7`,
	} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("falta %q", l)
		}
	}
	if strings.Contains(out, "kling_disk_free_mib") {
		t.Error("sin poder medir el disco no se inventa un 0")
	}
}

// 5.1: huella, CPU por núcleo, energía, tiempo en el estado y memoria del host.
func TestMetricsPorMaquina(t *testing.T) {
	desde := time.Unix(1000, 0)
	ps := api.ProcStats{MemoryKind: "footprint", AvailableMiB: 4096, Machines: []api.ProcStat{
		{ID: "aa", Name: "web", State: api.StateRunning, PID: 7, FootprintMiB: 120, CPUSeconds: 3, CPUPSeconds: 1,
			CoreSplit: true, EnergyJoules: 2.5, StateSince: &desde},
		{ID: "bb", Name: "pg", State: api.StateWarm, StateSince: &desde},
	}}
	var b bytes.Buffer
	escribirPorMaquina(&b, ps, desde.Add(90*time.Second))
	out := b.String()
	for _, l := range []string{
		"kling_host_mem_available_mib 4096",
		`kling_machine_footprint_mib{id="aa",name="web",state="running"} 120`,
		`kling_machine_cpu_seconds_total{id="aa",name="web",core="p"} 1.000`,
		`kling_machine_cpu_seconds_total{id="aa",name="web",core="e"} 2.000`,
		`kling_machine_energy_joules_total{id="aa",name="web"} 2.500`,
		`kling_machine_state_seconds{id="bb",name="pg",state="frozen"} 90`,
	} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("falta %q en:\n%s", l, out)
		}
	}
	// Sin reparto (Linux): core="all", y sin huella.
	ps.MemoryKind = "pss"
	ps.Machines[0].CoreSplit = false
	b.Reset()
	escribirPorMaquina(&b, ps, desde)
	if out := b.String(); !strings.Contains(out, `core="all"} 3.000`) || strings.Contains(out, "kling_machine_footprint_mib{") {
		t.Errorf("Linux:\n%s", out)
	}
}

// Con política, /metrics lo lee cualquier rol válido (un inquilino que
// monitoriza su uso del host), pero las series por máquina, con nombres e ID
// de todos, solo el admin. Sin rol, nada.
func TestMetricsPorRol(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	admin := como(t, h, uidAdmin, "GET", "/metrics", "")
	if admin.Code != 200 || !strings.Contains(admin.Body.String(), `kling_machine_state_seconds{id="bbbb000000000001"`) {
		t.Fatalf("admin: %d\n%s", admin.Code, admin.Body)
	}
	inq := como(t, h, uidA, "GET", "/metrics", "")
	if inq.Code != 200 {
		t.Fatalf("inquilino: %d %s", inq.Code, inq.Body)
	}
	body := inq.Body.String()
	if !strings.Contains(body, `kling_machines{state="frozen"}`) || !strings.Contains(body, "kling_admission_rejections_total") {
		t.Errorf("inquilino sin las series del host:\n%s", body)
	}
	for _, id := range []string{"aaaa000000000001", "bbbb000000000001", "dddd000000000001"} {
		if strings.Contains(body, id) {
			t.Errorf("inquilino ve la máquina %s:\n%s", id, body)
		}
	}
	if rr := como(t, h, uidNadie, "GET", "/metrics", ""); rr.Code != 403 {
		t.Errorf("sin rol: %d", rr.Code)
	}
}
