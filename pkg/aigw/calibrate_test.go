package aigw

import (
	"errors"
	"net/http"
	"os"
	"strings"
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

// TestCalibrarRechazaMicroVM comprueba que calibrar una tarea con un modelo
// jev backend microvm falla con un mensaje claro (reentrenar y volver a
// desplegar) en vez del "unavailable" genérico de intentar cargar un Path
// vacío: un modelo microvm no tiene .jev local que reescribir (docs/jev-serverless.md).
func TestCalibrarRechazaMicroVM(t *testing.T) {
	cfg := microVMConfig(t,
		&ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "jev-commits"},
		&TaskConfig{JEV: "commits", EscalateTo: "smol", EscalateForce: true})
	g, err := New(Options{Config: cfg, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Calibrate(CalibrateRequest{Task: "kind"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
	if !strings.Contains(err.Error(), "kling jev deploy") || !strings.Contains(err.Error(), "retraining") {
		t.Fatalf("error should point at retrain+redeploy: %v", err)
	}
}

// Si VON discrepa de todo, "no contestar nunca" cumpliría el objetivo sin
// prometer nada: no se escribe.
func TestCalibrarNoApagaJEV(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	r := g.rings["kind"]
	for i := 0; i < 400; i++ {
		r.add(sample{pred: "bug", prob: 1, teacher: "feat", weight: 1})
	}
	rep, err := g.Calibrate(CalibrateRequest{Task: "kind", Target: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Improved || rep.Written != "" || rep.Overall != 0 || !strings.Contains(rep.Reason, "never answer") {
		t.Fatalf("report = %+v", rep)
	}
}
