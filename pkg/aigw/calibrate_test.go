package aigw

import (
	"os"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/jev"
)

// Con umbrales que prometen de más (0 = todo confiado) y un VON que solo
// coincide con JEV cuando este está muy seguro, calibrar sube el umbral, mejora
// la concordancia en la mitad de evaluación y escribe el .jev (con copia).
func TestCalibrarMejoraYEscribe(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	path := g.config().Models["commits"].Path
	m, err := jev.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Thresholds {
		m.Thresholds[i] = 0
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}

	r := g.rings["kind"]
	for i := 0; i < 600; i++ {
		p := 0.3 + 0.7*float64(i%100)/100
		teacher := "bug"
		if p < 0.8 && i%3 != 0 {
			teacher = "feat" // por debajo de 0,8 VON discrepa dos de cada tres
		}
		w := 1.0
		if i%5 == 0 {
			w = 10 // auditoría con audit = 0,1
		}
		r.add(sample{pred: "bug", prob: p, teacher: teacher, weight: w, at: time.Now()})
	}

	// En seco: informa y no toca nada.
	rep, err := g.Calibrate(CalibrateRequest{Task: "kind", Target: 0.95, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Improved || rep.Written != "" || rep.Before.Agreement > 0.7 || rep.After.Agreement < 0.95 {
		t.Fatalf("dry run report = %+v", rep)
	}
	if rep.Audited == 0 || rep.Escalated == 0 || rep.Samples != 600 {
		t.Fatalf("sample accounting = %+v", rep)
	}

	rep, err = g.Calibrate(CalibrateRequest{Task: "kind", Target: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Written != path || rep.Backup != path+".prev" {
		t.Fatalf("not written: %+v", rep)
	}
	nm, err := jev.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bug := nm.GoldIndex("bug")
	if nm.Thresholds[bug] < 0.75 || nm.Thresholds[bug] > 0.9 {
		t.Fatalf("new bug threshold = %v", nm.Thresholds[bug])
	}
	if nm.Thresholds[nm.GoldIndex("feat")] != jev.NeverConfident {
		t.Fatalf("a class without samples should never be confident: %v", nm.Thresholds)
	}
	if _, err := os.Stat(path + ".prev"); err != nil {
		t.Fatal(err)
	}
	// La caché sirve ya el nuevo sin releer.
	cm, _ := g.jev.get(path)
	if cm.Thresholds[bug] != nm.Thresholds[bug] {
		t.Fatal("cache still serves the old thresholds")
	}

	// Otra vez con los mismos datos: ya no mejora, no escribe.
	rep, err = g.Calibrate(CalibrateRequest{Task: "kind", Target: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Improved || rep.Written != "" {
		t.Fatalf("second calibration wrote again: %+v", rep)
	}
}

func TestCalibrarSinMuestras(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	rep, err := g.Calibrate(CalibrateRequest{Task: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Improved || rep.Samples != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := g.Calibrate(CalibrateRequest{Task: "nope"}); err == nil {
		t.Fatal("unknown task accepted")
	}
}
