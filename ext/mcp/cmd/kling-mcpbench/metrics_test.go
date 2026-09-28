package main

import (
	"strings"
	"testing"
	"time"
)

// Lo que escribe internal/daemon/metrics.go, con un nombre que obliga a
// deshacer escapes (esc() en el daemon).
const metricsDaemon = `# HELP kling_machines Number of microVMs by state.
# TYPE kling_machines gauge
kling_machines{state="running"} 3
kling_machines{state="paused"} 0
kling_machines{state="frozen"} 7
kling_machines{state="created"} 0
# HELP kling_available_mib Host MemAvailable in MiB (/proc/meminfo).
# TYPE kling_available_mib gauge
kling_available_mib 12000
kling_free_mib 800
kling_total_pss_mib 310
# TYPE kling_machine_pss_mib gauge
kling_machine_pss_mib{id="aaa",name="tb-0-x1",service="tb-0",from="tb-0",state="running"} 100
kling_machine_pss_mib{id="bbb",name="raro \"a\\b\"\nc",service="tb-1",from="tb-1",state="running"} 60
kling_machine_pss_mib{id="ccc",name="otro",service="",from="github",state="running"} 150
`

func TestParseMetricsDelDaemon(t *testing.T) {
	ms, err := parseMetrics(strings.NewReader(metricsDaemon))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 10 {
		t.Fatalf("%d métricas, quería 10", len(ms))
	}
	var raro metric
	for _, m := range ms {
		if m.Labels["id"] == "bbb" {
			raro = m
		}
	}
	if want := "raro \"a\\b\"\nc"; raro.Labels["name"] != want {
		t.Errorf("name = %q, quería %q (escapes sin deshacer)", raro.Labels["name"], want)
	}
	if raro.Labels["service"] != "tb-1" || raro.Value != 60 {
		t.Errorf("tras el nombre escapado se leyó mal el resto: %+v", raro)
	}

	s := reduce(ms, map[string]bool{"tb-0": true, "tb-1": true})
	if s.Live != 2 || s.TargetPSS != 160 || s.TotalPSS != 310 || s.AvailMiB != 12000 || s.Frozen != 7 {
		t.Fatalf("reduce = %+v", s)
	}
}

// Una línea que no se entiende es un error, no un cero silencioso.
func TestParseMetricsRechazaBasura(t *testing.T) {
	for _, in := range []string{
		"kling_x{state=\"a\" 3\n",
		"kling_x{state=a} 3\n",
		"kling_x abc\n",
		"kling_x{state=\"a\"}\n",
		"soloNombre\n",
	} {
		if _, err := parseMetrics(strings.NewReader(in)); err == nil {
			t.Errorf("parseMetrics(%q) no falló", in)
		}
	}
}

func TestParseMetricsMarcaDeTiempoYSinEtiquetas(t *testing.T) {
	ms, err := parseMetrics(strings.NewReader("a 1.5 1700000000000\nb{} 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Value != 1.5 || ms[1].Name != "b" || ms[1].Value != 2 {
		t.Fatalf("%+v", ms)
	}
}

func TestParsePSI(t *testing.T) {
	in := "some avg10=3.25 avg60=1.00 avg300=0.50 total=12345\nfull avg10=1.50 avg60=0.20 avg300=0.10 total=678\n"
	some, full, ok := parsePSI(strings.NewReader(in))
	if !ok || some != 3.25 || full != 1.5 {
		t.Fatalf("parsePSI = %v %v %v", some, full, ok)
	}
	if _, _, ok := parsePSI(strings.NewReader("")); ok {
		t.Error("sin línea some debería dar ok=false")
	}
}

func TestSummarizeSamplesYTiempoACero(t *testing.T) {
	sec := time.Second
	ss := []sample{
		{At: 1 * sec, Live: 1, IDs: []string{"a"}, TargetPSS: 50, TotalPSS: 300, AvailMiB: 9000, PSISome: 0},
		{At: 2 * sec, Live: 3, IDs: []string{"a", "b", "c"}, TargetPSS: 150, TotalPSS: 400, AvailMiB: 8000, PSISome: 2.5},
		// fin de la ronda en 2.5 s
		{At: 3 * sec, Live: 2, IDs: []string{"b", "c"}, TargetPSS: 100, TotalPSS: 350, AvailMiB: 7000, PSISome: 4},
		{At: 5 * sec, Live: 0, TotalPSS: 250, AvailMiB: 9500, PSISome: 1},
		{At: 6 * sec, Live: 0, TotalPSS: 250, AvailMiB: 9500, PSISome: 1},
	}
	h := summarizeSamples(ss, 2500*time.Millisecond)
	if h.Samples != 2 || h.PeakLive != 3 || h.DistinctMachines != 3 || h.PeakTargetPSSMiB != 150 {
		t.Errorf("ronda: %+v", h)
	}
	if h.PeakTotalPSSMiB != 400 || h.MinAvailableMiB != 7000 || h.MaxPSISomeAvg10 != 4 {
		t.Errorf("host (incluye la espera): %+v", h)
	}
	if h.TimeToZeroMS == nil || *h.TimeToZeroMS != 2500 {
		t.Errorf("time_to_zero = %v, quería 2500", h.TimeToZeroMS)
	}

	h = summarizeSamples(ss[:3], 2500*time.Millisecond)
	if h.TimeToZeroMS != nil {
		t.Errorf("sin llegar a cero, time_to_zero debe ser nil, no %v", *h.TimeToZeroMS)
	}
}
