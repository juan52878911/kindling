package daemon

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Las series de recursos por máquina (despertares, disco, lo frenado por el
// techo de CPU): lo que hace falta para medir desde kindling los cambios de
// frecuencia de los vigilantes y del regulador.
func TestMetricsRecursosPorMaquina(t *testing.T) {
	frenado := 1.5
	ps := api.ProcStats{MemoryKind: "footprint", Machines: []api.ProcStat{
		{ID: "aa", Name: "web", State: api.StateRunning, PID: 7, WakeupsPkgIdle: 40, WakeupsInterrupt: 9,
			DiskReadBytes: 4096, DiskWriteBytes: 512, CPUThrottledSeconds: &frenado},
		{ID: "cc", Name: "fc", State: api.StateRunning, PID: 8, VoluntarySwitches: 77},
		// Sin techo ni disco: ni serie de frenado ni de disco.
		{ID: "dd", Name: "quieta", State: api.StateRunning, PID: 9},
	}}
	var b bytes.Buffer
	escribirPorMaquina(&b, ps, time.Unix(0, 0))
	out := b.String()
	for _, l := range []string{
		`kling_machine_wakeups_total{id="aa",name="web",kind="pkg_idle"} 40`,
		`kling_machine_wakeups_total{id="aa",name="web",kind="interrupt"} 9`,
		`kling_machine_wakeups_total{id="cc",name="fc",kind="voluntary_switch"} 77`,
		`kling_machine_disk_bytes_total{id="aa",name="web",op="read"} 4096`,
		`kling_machine_disk_bytes_total{id="aa",name="web",op="write"} 512`,
		`kling_machine_cpu_throttled_seconds_total{id="aa",name="web"} 1.500`,
		"# TYPE kling_machine_wakeups_total counter",
	} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("falta %q en:\n%s", l, out)
		}
	}
	for _, no := range []string{`kling_machine_cpu_throttled_seconds_total{id="cc"`, `kling_machine_disk_bytes_total{id="dd"`,
		`kling_machine_wakeups_total{id="dd"`, `{id="cc",name="fc",kind="pkg_idle"}`} {
		if strings.Contains(out, no) {
			t.Errorf("sobra %q en:\n%s", no, out)
		}
	}
}

// Lo que gasta el propio daemon y si va con batería.
func TestMetricsDelDaemon(t *testing.T) {
	var b bytes.Buffer
	escribirDaemon(&b, machine.RecursosProceso{HuellaMiB: 42, RSSMiB: 60, CPU: 2500 * time.Millisecond,
		DespReposo: 10, DespInterrupcion: 3, ConDespertares: true}, 17, true)
	out := b.String()
	for _, l := range []string{
		"kling_daemon_footprint_mib 42",
		"kling_daemon_rss_mib 60",
		"kling_daemon_cpu_seconds_total 2.500",
		`kling_daemon_wakeups_total{kind="pkg_idle"} 10`,
		`kling_daemon_wakeups_total{kind="interrupt"} 3`,
		"kling_daemon_goroutines 17",
		"kling_battery_on 1",
	} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("falta %q en:\n%s", l, out)
		}
	}
	b.Reset()
	escribirDaemon(&b, machine.RecursosProceso{}, 1, false)
	if out := b.String(); !strings.Contains(out, "kling_battery_on 0\n") || strings.Contains(out, "kling_daemon_wakeups_total") {
		t.Errorf("sin despertares ni batería:\n%s", out)
	}
}

// /metrics del daemon de verdad trae sus propias cifras: el proceso de la
// prueba hace de daemon y tiene memoria, CPU y goroutines.
func TestMetricsExponeElDaemon(t *testing.T) {
	_, h := testServer(t)
	body := call(t, h, "GET", "/metrics", "").Body.String()
	for _, serie := range []string{"kling_daemon_footprint_mib ", "kling_daemon_goroutines ", "kling_battery_on ",
		`kling_wake_phase_ms_total{kind="thaw",phase="load"}`, `kling_wake_phase_ms_total{kind="resume",phase="finish"}`} {
		if !strings.Contains(body, serie) {
			t.Errorf("falta la serie %s", serie)
		}
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if !strings.Contains(body, "kling_daemon_wakeups_total{") || strings.Contains(body, "kling_daemon_footprint_mib 0\n") {
			t.Errorf("sin despertares o sin huella del daemon:\n%s", body)
		}
	}
}

// La suma por fase de los despertares: la media por fase sale de dividirla
// por la cuenta del histograma de su clase.
func TestEscribirTelemetriaFasesDespertar(t *testing.T) {
	tl := machine.Telemetria{DiscoLibreMiB: -1, FasesMS: map[string]map[string]float64{
		machine.DurThaw: {"load": 12.5, "spawn": 3},
	}}
	var b bytes.Buffer
	escribirTelemetria(&b, tl, 0)
	out := b.String()
	for _, l := range []string{
		`kling_wake_phase_ms_total{kind="thaw",phase="load"} 12.500`,
		`kling_wake_phase_ms_total{kind="thaw",phase="spawn"} 3.000`,
		`kling_wake_phase_ms_total{kind="resume",phase="wait"} 0.000`,
	} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("falta %q en:\n%s", l, out)
		}
	}
}
