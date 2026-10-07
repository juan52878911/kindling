package daemon

import (
	"bytes"
	"strings"
	"testing"

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
