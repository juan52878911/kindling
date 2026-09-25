package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/jev"
)

// fakeJEVGuest es un kling-jev de mentira: contesta lo que diga resp y guarda
// la última petición que le llegó.
type fakeJEVGuest struct {
	srv   *httptest.Server
	resp  jevGuestResponse
	calls atomic.Int64
	last  jevGuestRequest
}

func newFakeJEVGuest(t *testing.T) *fakeJEVGuest {
	f := &fakeJEVGuest{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&f.last)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func microVMConfig(t *testing.T, model *ModelConfig, task *TaskConfig) *Config {
	t.Helper()
	cfg := &Config{
		Models: map[string]*ModelConfig{"commits": model, "smol": {Kind: KindVON, Snapshot: "von-smol"}},
		Tasks:  map[string]*TaskConfig{"kind": task},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestClassifyMicroVMConfident comprueba que una tarea con backend microvm
// pregunta a la réplica (no carga ningún .jev local) y devuelve su respuesta
// tal cual, como si JEV hubiera contestado en proceso.
func TestClassifyMicroVMConfident(t *testing.T) {
	fj := newFakeJEVGuest(t)
	fj.resp = jevGuestResponse{Label: "fix", Prob: 0.9, Threshold: 0.5, Confident: true, Decision: jev.DecisionConfident}
	reps := &fakeReplicas{addr: strings.TrimPrefix(fj.srv.URL, "http://")}
	cfg := microVMConfig(t, &ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "jev-commits"}, &TaskConfig{JEV: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Label != "fix" || resp.Source != "jev" || resp.Escalate {
		t.Fatalf("got %+v", resp)
	}
	if fj.calls.Load() != 1 {
		t.Fatalf("expected 1 call to the replica, got %d", fj.calls.Load())
	}
	if fj.last.Text != "fix the bug" {
		t.Fatalf("replica got %+v", fj.last)
	}
}

// TestClassifyMicroVMEscalates comprueba que, con la cascada forzada, una
// tarea microvm escala a VON con las etiquetas que mandó la réplica (no las
// de un jev.Model que aquí no existe).
func TestClassifyMicroVMEscalates(t *testing.T) {
	fj := newFakeJEVGuest(t)
	fj.resp = jevGuestResponse{
		Label: "fix", Prob: 0.4, Threshold: 0.5, Confident: false, Decision: jev.DecisionEscalate,
		Candidates: []jev.ClassProb{{Label: "fix", Prob: 0.4}, {Label: "feat", Prob: 0.3}, {Label: "docs", Prob: 0.3}},
	}
	ll := newFakeLlama(t)
	ll.set("feat")
	reps := &multiReplicas{byAddr: map[string]string{
		"jev-commits": strings.TrimPrefix(fj.srv.URL, "http://"),
		"von-smol":    strings.TrimPrefix(ll.srv.URL, "http://"),
	}}
	cfg := microVMConfig(t,
		&ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "jev-commits"},
		&TaskConfig{JEV: "commits", EscalateTo: "smol", EscalateForce: true})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "not sure"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Label != "feat" || resp.Source != "von" {
		t.Fatalf("got %+v", resp)
	}
}

// TestClassifyMicroVMWakeError comprueba que, si no hay réplica disponible
// (el daemon no contesta, no cabe...), la tarea responde 503 y no un pánico.
func TestClassifyMicroVMWakeError(t *testing.T) {
	reps := &fakeReplicas{fail: errors.New("no daemon")}
	cfg := microVMConfig(t, &ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "jev-commits"}, &TaskConfig{JEV: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
}

// TestConfigBackendValidation comprueba las reglas nuevas de backend: solo
// aplica a jev, sus valores son inprocess o microvm, y cada uno exige lo suyo
// (path o snapshot, nunca los dos).
func TestConfigBackendValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Tasks: map[string]*TaskConfig{"k": {JEV: "m"}}}
	}
	cases := []struct {
		name string
		m    ModelConfig
		ok   bool
	}{
		{"inprocess needs path", ModelConfig{Kind: KindJEV, Backend: BackendInProcess}, false},
		{"inprocess with snapshot rejected", ModelConfig{Kind: KindJEV, Path: "m.jev", Snapshot: "x"}, false},
		{"microvm needs snapshot", ModelConfig{Kind: KindJEV, Backend: BackendMicroVM}, false},
		{"microvm with path rejected", ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "s", Path: "m.jev"}, false},
		{"microvm ok", ModelConfig{Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "s"}, true},
		{"bad backend", ModelConfig{Kind: KindJEV, Backend: "gpu", Path: "m.jev"}, false},
		{"backend on von rejected", ModelConfig{Kind: KindVON, Backend: BackendMicroVM, Snapshot: "s"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base()
			m := c.m
			cfg.Models = map[string]*ModelConfig{"m": &m}
			err := cfg.Validate()
			if (err == nil) != c.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// TestNeedsDaemonMicroVM comprueba que un registro con una tarea jev en
// microvm sí necesita daemon, a diferencia de uno solo con jev en proceso.
func TestNeedsDaemonMicroVM(t *testing.T) {
	cfg := &Config{Models: map[string]*ModelConfig{"m": {Kind: KindJEV, Backend: BackendMicroVM, Snapshot: "s"}}}
	if !cfg.NeedsDaemon() {
		t.Fatal("a microvm-backed jev model should need the daemon")
	}
	cfg2 := &Config{Models: map[string]*ModelConfig{"m": {Kind: KindJEV, Path: "m.jev"}}}
	if cfg2.NeedsDaemon() {
		t.Fatal("an in-process-only registry should not need the daemon")
	}
}

// multiReplicas reparte una réplica distinta según el nombre del dorado: hace
// falta cuando una prueba tiene a la vez un JEV en microvm y un VON (dos
// invitados de mentira distintos).
type multiReplicas struct{ byAddr map[string]string }

func (m *multiReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	addr, ok := m.byAddr[snap]
	if !ok {
		return nil, errors.New("no fake replica for " + snap)
	}
	return &Replica{Addr: addr, Release: func() {}, Drop: func() {}}, nil
}
