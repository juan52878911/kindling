package daemon

import (
	"bytes"
	"strings"
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
