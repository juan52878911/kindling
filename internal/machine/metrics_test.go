package machine

import (
	"os"
	"strconv"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// ProcStats mide las máquinas vivas en paralelo: cada una con lo suyo, las
// paradas sin medir, y sin carreras (-race).
func TestProcStatsEnParalelo(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}, socket: map[string]string{}}
	for i := range 3 * paraleloStats {
		id := "m" + strconv.Itoa(i)
		mc := &api.Machine{ID: id, Name: id, State: api.StateRunning, PID: os.Getpid()}
		if i%3 == 0 {
			mc.State, mc.PID = api.StateStopped, 0
		}
		m.byID[id] = mc
	}
	ps := m.ProcStats()
	if len(ps.Machines) != 3*paraleloStats {
		t.Fatalf("%d máquinas, want %d", len(ps.Machines), 3*paraleloStats)
	}
	if ps.ByState[string(api.StateStopped)] != paraleloStats {
		t.Fatalf("ByState = %v", ps.ByState)
	}
	var total int64
	for _, st := range ps.Machines {
		total += st.PSSMiB
		if st.PID == 0 {
			if st.CPUSeconds != 0 || st.PSSMiB != 0 {
				t.Fatalf("%s parada y medida: %+v", st.ID, st)
			}
			continue
		}
		if st.CPUSeconds <= 0 {
			t.Fatalf("%s viva sin CPU: %+v", st.ID, st)
		}
	}
	if total != ps.TotalPSSMiB {
		t.Fatalf("TotalPSSMiB = %d, suma %d", ps.TotalPSSMiB, total)
	}
}
