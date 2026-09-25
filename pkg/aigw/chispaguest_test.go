package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// fakeDeployLookup es chispaDeployLookup de mentira: los tests fijan las
// etiquetas de cada dorado a mano, sin necesitar un daemon de verdad.
type fakeDeployLookup map[string]ChispaDeployRecord

func (f fakeDeployLookup) chispaLabels(_ context.Context, snapshot string) (ChispaDeployRecord, error) {
	rec, ok := f[snapshot]
	if !ok {
		return ChispaDeployRecord{}, fmt.Errorf("no deploy record for %q", snapshot)
	}
	return rec, nil
}

// fakeChispaGuest es un kling-chispa de mentira: contesta lo que diga resp y guarda
// la última petición que le llegó.
type fakeChispaGuest struct {
	srv   *httptest.Server
	resp  chispaGuestResponse
	calls atomic.Int64
	last  chispaGuestRequest
}

func newFakeChispaGuest(t *testing.T) *fakeChispaGuest {
	f := &fakeChispaGuest{}
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
// pregunta a la réplica (no carga ningún .chispa local) y devuelve su respuesta
// tal cual, como si Chispa hubiera contestado en proceso.
func TestClassifyMicroVMConfident(t *testing.T) {
	fj := newFakeChispaGuest(t)
	fj.resp = chispaGuestResponse{Label: "fix", Prob: 0.9, Threshold: 0.5, Confident: true, Decision: chispa.DecisionConfident}
	reps := &fakeReplicas{addr: strings.TrimPrefix(fj.srv.URL, "http://")}
	cfg := microVMConfig(t, &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}, &TaskConfig{Chispa: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
	resp, err := g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Label != "fix" || resp.Source != "chispa" || resp.Escalate {
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
// de un chispa.Model que aquí no existe).
func TestClassifyMicroVMEscalates(t *testing.T) {
	fj := newFakeChispaGuest(t)
	fj.resp = chispaGuestResponse{
		Label: "fix", Prob: 0.4, Threshold: 0.5, Confident: false, Decision: chispa.DecisionEscalate,
		Candidates: []chispa.ClassProb{{Label: "fix", Prob: 0.4}, {Label: "feat", Prob: 0.3}, {Label: "docs", Prob: 0.3}},
	}
	ll := newFakeLlama(t)
	ll.set("feat")
	reps := &multiReplicas{byAddr: map[string]string{
		"chispa-commits": strings.TrimPrefix(fj.srv.URL, "http://"),
		"von-smol":       strings.TrimPrefix(ll.srv.URL, "http://"),
	}}
	cfg := microVMConfig(t,
		&ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"},
		&TaskConfig{Chispa: "commits", EscalateTo: "smol", EscalateForce: true})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
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
	cfg := microVMConfig(t, &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}, &TaskConfig{Chispa: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	// El registro de despliegue sí resuelve: lo que falla es conseguir una
	// réplica, no las etiquetas. Así la prueba sigue comprobando de verdad el
	// camino de wakeError, no deployLookupError (los dos dan 503 igual, pero
	// por razones distintas).
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
	_, err = g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
}

// TestClassifyMicroVMNoDeployRecord comprueba que, sin registro de despliegue
// (dorado hecho antes de esta protección, o daemon sin la anotación), la tarea
// falla con un mensaje claro en vez de fiarse de lo que mande el invitado.
func TestClassifyMicroVMNoDeployRecord(t *testing.T) {
	fj := newFakeChispaGuest(t)
	fj.resp = chispaGuestResponse{Label: "fix", Prob: 0.9, Threshold: 0.5, Confident: true, Decision: chispa.DecisionConfident}
	reps := &fakeReplicas{addr: strings.TrimPrefix(fj.srv.URL, "http://")}
	cfg := microVMConfig(t, &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}, &TaskConfig{Chispa: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{} // sin entrada para "chispa-commits"
	_, err = g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
	if fj.calls.Load() != 0 {
		t.Fatalf("should not have called the replica without a label set to validate against, got %d calls", fj.calls.Load())
	}
}

// TestConfigBackendValidation comprueba las reglas nuevas de backend: solo
// aplica a chispa, sus valores son inprocess o microvm, y cada uno exige lo suyo
// (path o snapshot, nunca los dos).
func TestConfigBackendValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Tasks: map[string]*TaskConfig{"k": {Chispa: "m"}}}
	}
	cases := []struct {
		name string
		m    ModelConfig
		ok   bool
	}{
		{"inprocess needs path", ModelConfig{Kind: KindChispa, Backend: BackendInProcess}, false},
		{"inprocess with snapshot rejected", ModelConfig{Kind: KindChispa, Path: "m.chispa", Snapshot: "x"}, false},
		{"microvm needs snapshot", ModelConfig{Kind: KindChispa, Backend: BackendMicroVM}, false},
		{"microvm with path rejected", ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "s", Path: "m.chispa"}, false},
		{"microvm ok", ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "s"}, true},
		{"bad backend", ModelConfig{Kind: KindChispa, Backend: "gpu", Path: "m.chispa"}, false},
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

// TestValidateGuestReplyAdversarial comprueba que validateGuestReply rechaza
// cada campo que un invitado hostil (o simplemente desincronizado tras un
// redeploy) podría mandar fuera de lo que el registro de despliegue promete.
// Antes de esto, todos estos casos pasaban tal cual a gateway.go.
func TestValidateGuestReplyAdversarial(t *testing.T) {
	labels := []string{"fix", "feat", "docs"}
	valid := func() chispaGuestResponse {
		return chispaGuestResponse{Label: "fix", Prob: 0.9, Threshold: 0.5, Decision: chispa.DecisionConfident}
	}
	cases := []struct {
		name string
		gr   chispaGuestResponse
	}{
		{"unknown label", func() chispaGuestResponse { g := valid(); g.Label = "not-a-real-label"; return g }()},
		{"missing label", func() chispaGuestResponse { g := valid(); g.Label = ""; return g }()},
		{"NaN prob", func() chispaGuestResponse { g := valid(); g.Prob = math.NaN(); return g }()},
		{"+Inf prob", func() chispaGuestResponse { g := valid(); g.Prob = math.Inf(1); return g }()},
		{"negative prob", func() chispaGuestResponse { g := valid(); g.Prob = -0.1; return g }()},
		{"prob over 1", func() chispaGuestResponse { g := valid(); g.Prob = 1.1; return g }()},
		{"negative threshold", func() chispaGuestResponse { g := valid(); g.Threshold = -0.1; return g }()},
		{"threshold over NeverConfident", func() chispaGuestResponse { g := valid(); g.Threshold = chispa.NeverConfident + 0.1; return g }()},
		{"Inf threshold", func() chispaGuestResponse { g := valid(); g.Threshold = math.Inf(1); return g }()},
		{"unknown decision", func() chispaGuestResponse { g := valid(); g.Decision = "maybe"; return g }()},
		{"candidate with unknown label", func() chispaGuestResponse {
			g := valid()
			g.Candidates = []chispa.ClassProb{{Label: "fix", Prob: 0.9}, {Label: "not-a-real-label", Prob: 0.1}}
			return g
		}()},
		{"candidate with NaN prob", func() chispaGuestResponse {
			g := valid()
			g.Candidates = []chispa.ClassProb{{Label: "fix", Prob: math.NaN()}}
			return g
		}()},
		{"duplicate candidate label", func() chispaGuestResponse {
			g := valid()
			g.Candidates = []chispa.ClassProb{{Label: "fix", Prob: 0.6}, {Label: "fix", Prob: 0.4}}
			return g
		}()},
		{"more candidates than labels", func() chispaGuestResponse {
			g := valid()
			// Un modelo de 3 etiquetas no puede tener 4 candidatos distintos:
			// esto simula un invitado que ignora el modelo real (o una réplica
			// hostil que intenta hinchar la respuesta con etiquetas huérfanas
			// dentro del tope de tamaño, ver maxChispaGuestAnswerBytes).
			g.Candidates = []chispa.ClassProb{{Label: "fix", Prob: 0.4}, {Label: "feat", Prob: 0.3}, {Label: "docs", Prob: 0.2}, {Label: "chore", Prob: 0.1}}
			return g
		}()},
		{"huge evidence list", func() chispaGuestResponse {
			g := valid()
			ev := make([]chispa.Evidence, maxGuestEvidence+1)
			for i := range ev {
				ev[i] = chispa.Evidence{Feature: "f", Weight: 1}
			}
			g.Evidence = ev
			return g
		}()},
		{"evidence with Inf weight", func() chispaGuestResponse {
			g := valid()
			g.Evidence = []chispa.Evidence{{Feature: "f", Weight: math.Inf(-1)}}
			return g
		}()},
		{"evidence with empty feature", func() chispaGuestResponse {
			g := valid()
			g.Evidence = []chispa.Evidence{{Feature: "", Weight: 1}}
			return g
		}()},
		{"label-set mismatch after redeploy", func() chispaGuestResponse {
			// Un modelo viejo tenía "fix"/"feat"/"docs"/"chore"; tras
			// `kling chispa deploy -replace` con un modelo de solo 3 etiquetas, una
			// réplica que todavía sirviera la imagen anterior (o mintiera)
			// contestaría con una etiqueta que el registro nuevo ya no conoce.
			g := valid()
			g.Label = "chore"
			return g
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := validateGuestReply(labels, c.gr); err == nil {
				t.Fatalf("validateGuestReply(%+v) = nil error, want a rejection", c.gr)
			}
		})
	}
}

// TestValidateGuestReplyAccepts comprueba que una respuesta sana —incluida una
// con la distribución entera, como manda una réplica que duda— pasa la
// validación y conserva sus datos.
func TestValidateGuestReplyAccepts(t *testing.T) {
	labels := []string{"fix", "feat", "docs"}
	gr := chispaGuestResponse{
		Label: "fix", Prob: 0.4, Threshold: 0.5, Decision: chispa.DecisionEscalate,
		Candidates: []chispa.ClassProb{{Label: "fix", Prob: 0.4}, {Label: "feat", Prob: 0.35}, {Label: "docs", Prob: 0.25}},
		Evidence:   []chispa.Evidence{{Feature: "word:fix", Weight: 1.2}},
	}
	p, err := validateGuestReply(labels, gr)
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if p.Label != "fix" || p.Prob != 0.4 || p.Threshold != 0.5 || len(p.Probs) != 3 || len(p.Evidence) != 1 {
		t.Fatalf("got %+v", p)
	}
	// Un umbral de NeverConfident (la clase nunca contesta confiada) es válido,
	// no [0,1] como una probabilidad: ver el comentario de validateGuestReply.
	never := chispaGuestResponse{Label: "fix", Prob: 1, Threshold: chispa.NeverConfident, Decision: chispa.DecisionEscalate}
	if _, err := validateGuestReply(labels, never); err != nil {
		t.Fatalf("NeverConfident threshold should be valid: %v", err)
	}
}

// TestClassifyMicroVMInvalidReply comprueba que Classify convierte una
// respuesta de réplica que no pasa validateGuestReply en un 502 con un mensaje
// claro, sin dejar pasar la etiqueta inventada como si Chispa hubiera contestado.
func TestClassifyMicroVMInvalidReply(t *testing.T) {
	fj := newFakeChispaGuest(t)
	fj.resp = chispaGuestResponse{Label: "not-a-real-label", Prob: 0.9, Threshold: 0.5, Confident: true, Decision: chispa.DecisionConfident}
	reps := &fakeReplicas{addr: strings.TrimPrefix(fj.srv.URL, "http://")}
	cfg := microVMConfig(t, &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}, &TaskConfig{Chispa: "commits"})
	g, err := New(Options{Config: cfg, Replicas: reps})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
	_, err = g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "fix the bug"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %v", err)
	}
}

// TestNeedsDaemonMicroVM comprueba que un registro con una tarea chispa en
// microvm sí necesita daemon, a diferencia de uno solo con chispa en proceso.
func TestNeedsDaemonMicroVM(t *testing.T) {
	cfg := &Config{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "s"}}}
	if !cfg.NeedsDaemon() {
		t.Fatal("a microvm-backed chispa model should need the daemon")
	}
	cfg2 := &Config{Models: map[string]*ModelConfig{"m": {Kind: KindChispa, Path: "m.chispa"}}}
	if cfg2.NeedsDaemon() {
		t.Fatal("an in-process-only registry should not need the daemon")
	}
}

// multiReplicas reparte una réplica distinta según el nombre del dorado: hace
// falta cuando una prueba tiene a la vez un Chispa en microvm y un VON (dos
// invitados de mentira distintos).
type multiReplicas struct{ byAddr map[string]string }

func (m *multiReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	addr, ok := m.byAddr[snap]
	if !ok {
		return nil, errors.New("no fake replica for " + snap)
	}
	return &Replica{Addr: addr, Release: func() {}, Drop: func() {}}, nil
}
