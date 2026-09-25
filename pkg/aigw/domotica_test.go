package aigw

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/codificador"
	"github.com/juan52878911/kindling/pkg/domotica"
	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/train"
)

// Una tarea de domótica diminuta: JEV sabe «enciende/apaga …» y todo lo demás
// es fuera de ámbito; el codificador falso pone «oscuro» cerca de turn_on.

func domoJEV(t *testing.T) string {
	t.Helper()
	var exs []jev.Example
	things := []string{"la lámpara", "el foco", "la bombilla", "el flexo", "la tira", "el aplique"}
	for i := 0; i < 240; i++ {
		th := things[i%len(things)]
		switch i % 3 {
		case 0:
			exs = append(exs, jev.Example{Text: "enciende " + th, Label: "turn_on"})
		case 1:
			exs = append(exs, jev.Example{Text: "apaga " + th, Label: "turn_off"})
		default:
			exs = append(exs, jev.Example{Text: []string{"qué hora es", "cuéntame un chiste", "pon una alarma", "llama a mamá"}[i%4] + " " + th, Label: domotica.OutOfScope})
		}
	}
	cfg := train.Config{Spec: jev.DefaultSpec(), Seed: 1}
	cfg.Spec.Buckets = 1 << 12
	res, err := train.Train(exs, exs[:60], cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "intent.jev")
	if err := res.Model.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// vecFor es el «codificador»: tres direcciones, una por clase.
func vecFor(text string) []float32 {
	switch {
	case strings.Contains(text, "oscur"):
		return []float32{0, 0, 1, 0.1}
	case strings.Contains(text, "apag"):
		return []float32{0, 1, 0, 0.1}
	}
	return []float32{1, 0, 0, 0.1}
}

func domoHead(t *testing.T) string {
	t.Helper()
	var ds codificador.Dataset
	for i := 0; i < 90; i++ {
		v := vecFor([]string{"x", "apaga", "oscuro"}[i%3])
		v[3] = float32(i%7) / 10 // algo de ruido
		ds.X, ds.Y = append(ds.X, v), append(ds.Y, i%3)
	}
	res, err := codificador.Train([]string{domotica.OutOfScope, "turn_off", "turn_on"}, ds, ds,
		codificador.TrainConfig{Epochs: 30, TargetPrecision: 0.9, MinSupport: 3, Meta: codificador.Meta{Encoder: "fake:q8_0", Prefix: "query: "}})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "head.jenc")
	if err := res.Head.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func newDomoGateway(t *testing.T, force bool) (*Gateway, *fakeReplicas, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var b struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		var data []any
		for i, s := range b.Input {
			if !strings.HasPrefix(s, "query: ") {
				t.Errorf("the head's prefix is missing: %q", s)
			}
			data = append(data, map[string]any{"index": i, "embedding": vecFor(s)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	reps := &fakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")}
	cfg := &Config{
		Models: map[string]*ModelConfig{
			"intent": {Kind: KindJEV, Path: domoJEV(t)},
			"enc":    {Kind: KindEmbed, Snapshot: "enc-e5"},
		},
		Tasks: map[string]*TaskConfig{
			"home": {Domotica: &DomoticaConfig{Intent: "intent", Encoder: "enc", Head: domoHead(t), EncoderForce: force}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return g, reps, calls
}

func TestDomoticaConfig(t *testing.T) {
	jv := &ModelConfig{Kind: KindJEV, Path: "x.jev"}
	emb := &ModelConfig{Kind: KindEmbed, Snapshot: "enc-e5"}
	for name, task := range map[string]*TaskConfig{
		"jev too":        {JEV: "intent", Domotica: &DomoticaConfig{Intent: "intent"}},
		"not jev":        {Domotica: &DomoticaConfig{Intent: "enc"}},
		"head alone":     {Domotica: &DomoticaConfig{Intent: "intent", Head: "h.jenc"}},
		"encoder is jev": {Domotica: &DomoticaConfig{Intent: "intent", Encoder: "intent", Head: "h.jenc"}},
		"force alone":    {Domotica: &DomoticaConfig{Intent: "intent", EncoderForce: true}},
	} {
		c := &Config{Models: map[string]*ModelConfig{"intent": jv, "enc": emb}, Tasks: map[string]*TaskConfig{"t": task}}
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (&Config{Models: map[string]*ModelConfig{"e": {Kind: KindEmbed}}}).Validate(); err == nil {
		t.Error("embed model without snapshot accepted")
	}
}

func TestDomoticaDecideAndGate(t *testing.T) {
	g, reps, calls := newDomoGateway(t, false)
	h := g.Handler("")
	decide := func(text string) DecideResponse {
		rec := do(t, h, http.MethodPost, "/v1/decide", "", map[string]any{"task": "home", "text": text, "lang": "es"})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
		}
		return decode[DecideResponse](t, rec)
	}

	// Sin evaluación la capa 3 está rechazada: no se llama al codificador.
	d := decide("está muy oscuro aquí")
	if d.Encoder == nil || d.Encoder.Status != "refused" || calls.Load() != 0 || d.Escalate != domotica.EscalateTo {
		t.Fatalf("without an eval: %+v (%d calls)", d, calls.Load())
	}
	if rec := do(t, h, http.MethodPost, "/v1/classify", "", map[string]any{"task": "home", "text": "x"}); rec.Code != 400 {
		t.Fatalf("classify on a domotica task: %d", rec.Code)
	}

	// La evaluación: la capa 3 contesta bien lo oscuro, que JEV no sabe.
	rows := []domotica.Row{}
	for i := 0; i < 12; i++ {
		rows = append(rows,
			domotica.Row{Text: "está muy oscuro en la sala " + string(rune('a'+i)), Lang: "es", Intent: "turn_on", Slots: domotica.Slots{Device: domotica.DevLight}},
			domotica.Row{Text: "enciende la lámpara", Lang: "es", Intent: "turn_on", Slots: domotica.Slots{Device: domotica.DevLight}})
	}
	rec := do(t, h, http.MethodPost, "/v1/admin/eval", "", EvalRequest{Task: "home", Data: "t.jsonl", Rows: rows})
	if rec.Code != 200 {
		t.Fatalf("eval: %d %s", rec.Code, rec.Body)
	}
	out := decode[struct {
		Record  DomoticaEvalRecord `json:"record"`
		Cascade CascadeState       `json:"cascade"`
	}](t, rec)
	r := out.Record.Results
	if !out.Record.BeatsFast || out.Cascade.Status != "on" || r.CascadeOnlyRight != 12 || r.FastOnlyRight != 0 || r.ToEncoder != 12 {
		t.Fatalf("eval: %+v %+v", out.Record, out.Cascade)
	}

	// Encendida: lo oscuro lo contesta el codificador.
	d = decide("está muy oscuro aquí")
	if !d.Confident || d.Layer != domotica.LayerEncoder || d.Label != "turn_on" || d.Encoder.Status != "on" || reps.acquired.Load() == 0 {
		t.Fatalf("with the encoder: %+v", d)
	}
	// Cambiar la cabeza invalida la evaluación (la puerta mira su hash).
	if err := os.WriteFile(g.config().Tasks["home"].Domotica.Head, []byte("otra cabeza"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.regate()
	if st := g.cascade("home"); st.Status != "refused" || !strings.Contains(st.Reason, "changed") {
		t.Fatalf("a changed head must refuse: %+v", st)
	}

	// Réplica caída con la capa forzada: escala a VON diciendo por qué.
	g2, reps2, _ := newDomoGateway(t, true)
	reps2.fail = errors.New("no capacity")
	rec = do(t, g2.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "home", "text": "está muy oscuro"})
	d = decode[DecideResponse](t, rec)
	if d.Confident || d.Escalate != domotica.EscalateVON || d.EncoderError == "" || d.Encoder.Status != "forced" {
		t.Fatalf("encoder down: %+v", d)
	}
}
