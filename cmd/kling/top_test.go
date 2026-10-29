package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// En macOS kling top enseña la memoria (huella) y el %CPU de cada máquina, con
// la parte en núcleos P, y no "no /proc".
func TestTopEnMacEnseñaMemoriaYCPU(t *testing.T) {
	antes := &api.ProcStats{Machines: []api.ProcStat{
		{ID: "aaaaaaaaaaaa1", Name: "web", State: api.StateRunning, PID: 7, CPUSeconds: 10, CPUPSeconds: 2, CoreSplit: true},
	}}
	ahora := &api.ProcStats{MemoryKind: "footprint", AvailableMiB: 9000, TotalMiB: 16384, TotalPSSMiB: 140,
		Machines: []api.ProcStat{
			{ID: "aaaaaaaaaaaa1", Name: "web", State: api.StateRunning, PID: 7, PSSMiB: 140, FootprintMiB: 140,
				CPUSeconds: 10.5, CPUPSeconds: 2.1, CoreSplit: true},
		}}
	var b bytes.Buffer
	printTop(&b, ahora, antes, time.Second, "unix:///x", time.Time{})
	out := b.String()
	for _, quiero := range []string{"MEM", "CPU", "ON P-CORES", "140 MiB", "50.0%", "20%", "~9000 MiB available of 16384 MiB"} {
		if !strings.Contains(out, quiero) {
			t.Errorf("falta %q en:\n%s", quiero, out)
		}
	}
	if strings.Contains(out, "no /proc") {
		t.Errorf("sigue diciendo no /proc:\n%s", out)
	}

	// Un daemon anterior (sin CPU) se ve como antes, sin columna de CPU.
	b.Reset()
	printTop(&b, &api.ProcStats{AvailableMiB: 100, FreeMiB: 50, Machines: []api.ProcStat{
		{ID: "bbbbbbbbbbbb", State: api.StateRunning, PID: 3, PSSMiB: 30}}}, nil, 0, "x", time.Time{})
	if out := b.String(); strings.Contains(out, "CPU") || !strings.Contains(out, "PSS") || !strings.Contains(out, "MemAvailable 100") {
		t.Errorf("daemon anterior:\n%s", out)
	}
}
