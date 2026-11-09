package machine

import (
	"math"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Cada despertar suma sus fases a las de su clase, para que /metrics diga
// dónde se va el tiempo y no solo cuánto dura.
func TestDespertarSumaLasFases(t *testing.T) {
	m := &Manager{}
	antes := m.Telemetria().FasesMS
	tel.despertar(DurThaw, &api.WakePhases{WaitMS: 0.25, LoadMS: 5.5, SpawnMS: 2, FinishMS: 1, TotalMS: 9})
	tel.despertar(DurResume, &api.WakePhases{LoadMS: 3, TotalMS: 3})
	despues := m.Telemetria().FasesMS
	for _, c := range []struct {
		kind, fase string
		quiero     float64
	}{
		{DurThaw, "wait", 0.25}, {DurThaw, "load", 5.5}, {DurThaw, "spawn", 2}, {DurThaw, "finish", 1},
		{DurThaw, "net", 0}, {DurResume, "load", 3}, {DurResume, "spawn", 0},
	} {
		if d := despues[c.kind][c.fase] - antes[c.kind][c.fase]; math.Abs(d-c.quiero) > 1e-9 {
			t.Errorf("%s/%s sumó %v, quería %v", c.kind, c.fase, d, c.quiero)
		}
	}
	if len(despues[DurThaw]) != len(FasesDespertar) || len(despues[DurResume]) != len(FasesDespertar) {
		t.Errorf("faltan fases: %v", despues)
	}
}

// Todas las fases de api.WakePhases están en FasesDespertar, en su orden: un
// campo nuevo en WakePhases que no se sume aquí no aparecería en /metrics.
func TestFasesDespertarCubreWakePhases(t *testing.T) {
	f := api.WakePhases{WaitMS: 1, CheckMS: 2, StoreMS: 3, MirrorMS: 4, MemoryMS: 5, NetMS: 6, SpawnMS: 7,
		SocketMS: 8, LoadMS: 9, ForwardsMS: 10, ResyncMS: 11, CgroupMS: 12, FinishMS: 13}
	for i, v := range msFases(&f) {
		if v != float64(i+1) {
			t.Fatalf("fase %s = %v, quería %d", FasesDespertar[i], v, i+1)
		}
	}
}

// El daemon se mide a sí mismo: memoria y CPU, y despertares donde la
// plataforma los da.
func TestRecursosDaemon(t *testing.T) {
	r := RecursosDaemon()
	if !plataformaConProc() {
		t.Skip("sin /proc ni proc_pid_rusage")
	}
	if r.HuellaMiB <= 0 || r.RSSMiB <= 0 || r.CPU <= 0 {
		t.Errorf("sin memoria o CPU: %+v", r)
	}
	if !r.ConDespertares || r.DespReposo+r.DespInterrupcion+r.CambiosVoluntarios == 0 {
		t.Errorf("sin despertares: %+v", r)
	}
}
