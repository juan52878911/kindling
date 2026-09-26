package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
	"github.com/juan52878911/kindling/pkg/codificador"
	"github.com/juan52878911/kindling/pkg/intent"
)

// El gateway de kindling con el dominio de la habitación: una tarea de
// intención diminuta en la que Chispa sabe «enciende/apaga …», todo lo demás
// es fuera de ámbito y el codificador falso pone «oscuro» cerca de turn_on.

func roomChispa(t *testing.T) string {
	t.Helper()
	var exs []chispa.Example
	things := []string{"la lámpara", "el foco", "la bombilla", "el flexo", "la tira", "el aplique"}
	for i := 0; i < 240; i++ {
		th := things[i%len(things)]
		switch i % 3 {
		case 0:
			exs = append(exs, chispa.Example{Text: "enciende " + th, Label: "turn_on"})
		case 1:
			exs = append(exs, chispa.Example{Text: "apaga " + th, Label: "turn_off"})
		default:
			exs = append(exs, chispa.Example{Text: []string{"qué hora es", "cuéntame un chiste", "pon una alarma", "llama a mamá"}[i%4] + " " + th, Label: domotica.OutOfScope})
		}
	}
	cfg := train.Config{Spec: chispa.DefaultSpec(), Seed: 1}
	cfg.Spec.Buckets = 1 << 12
	res, err := train.Train(exs, exs[:60], cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "intent.chispa")
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

func roomHead(t *testing.T) string {
	t.Helper()
	var ds codificador.Dataset
	for i := 0; i < 90; i++ {
		v := vecFor([]string{"x", "apaga", "oscuro"}[i%3])
		v[3] = float32(i%7) / 10
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

// fakeReplicas apunta todas las réplicas al codificador falso.
type fakeReplicas struct {
	addr     string
	fail     error
	acquired atomic.Int64
}

func (f *fakeReplicas) Acquire(context.Context, string) (*aigw.Replica, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.acquired.Add(1)
	return &aigw.Replica{Addr: f.addr, Release: func() {}, Drop: func() {}}, nil
}

func newRoomGateway(t *testing.T, force bool) (http.Handler, *aigw.Gateway, *fakeReplicas, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var b struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		var data []any
		for i, s := range b.Input {
			data = append(data, map[string]any{"index": i, "embedding": vecFor(s)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	reps := &fakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")}
	cfg := &aigw.Config{
		Models: map[string]*aigw.ModelConfig{
			"intent": {Kind: aigw.KindChispa, Path: roomChispa(t)},
			"enc":    {Kind: aigw.KindEmbed, Snapshot: "enc-e5"},
		},
		Tasks: map[string]*aigw.TaskConfig{
			"room": {Intent: &aigw.IntentConfig{Model: "intent", Domain: DomainName, Encoder: "enc", Head: roomHead(t), EncoderForce: force}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	doms, err := domains()
	if err != nil {
		t.Fatal(err)
	}
	g, err := aigw.New(aigw.Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir(), Domains: doms})
	if err != nil {
		t.Fatal(err)
	}
	return g.Handler(""), g, reps, calls
}

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// La decisión que da el gateway es la de la demo (la misma cascada y el mismo
// JSON), y la puerta de la capa 3 se enciende con su evaluación.
func TestGatewayDomainDecideAndGate(t *testing.T) {
	h, _, reps, calls := newRoomGateway(t, false)
	decide := func(text string) domotica.Decision {
		t.Helper()
		rec := post(t, h, "/v1/decide", map[string]any{"task": "room", "text": text, "lang": "es"})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
		}
		var d domotica.Decision
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Plantilla de la demo: huecos de la habitación, completos.
	if d := decide("pon el termostato a 22 grados"); d.Layer != domotica.LayerTemplate || d.Intent != "set_temperature" ||
		!d.Slots.HasValue || d.Slots.Value != 22 || d.Slots.Device != domotica.DevThermostat {
		t.Fatalf("template: %+v", d)
	}

	// Sin evaluación la capa 3 está rechazada: no se llama al codificador.
	if d := decide("está muy oscuro aquí"); d.Confident || calls.Load() != 0 || d.Escalate != domotica.EscalateTo {
		t.Fatalf("without an eval: %+v (%d calls)", d, calls.Load())
	}
	var rows []intent.Row
	for i := 0; i < 12; i++ {
		rows = append(rows,
			intent.Row{Text: "está muy oscuro en la sala " + string(rune('a'+i)), Lang: "es", Intent: "turn_on", Slots: json.RawMessage(`{"device":"light"}`)},
			intent.Row{Text: "enciende la lámpara", Lang: "es", Intent: "turn_on", Slots: json.RawMessage(`{"device":"light"}`)})
	}
	rec := post(t, h, "/v1/admin/eval", aigw.EvalRequest{Task: "room", Data: "t.jsonl", Rows: rows})
	if rec.Code != 200 {
		t.Fatalf("eval: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Record  aigw.IntentEvalRecord `json:"record"`
		Cascade aigw.CascadeState     `json:"cascade"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	r := out.Record.Results
	if !out.Record.BeatsFast || out.Cascade.Status != "on" || r.CascadeOnlyRight != 12 || r.FastOnlyRight != 0 || out.Record.Domain != "domain:"+DomainName {
		t.Fatalf("eval: %+v %+v", out.Record, out.Cascade)
	}
	// Encendida: lo oscuro lo contesta el codificador, con la luz implícita.
	d := decide("está muy oscuro aquí")
	if !d.Confident || d.Layer != domotica.LayerEncoder || d.Intent != "turn_on" || d.Slots.Device != domotica.DevLight || reps.acquired.Load() == 0 {
		t.Fatalf("with the encoder: %+v", d)
	}
	// La traza de la demo sabe leer la decisión del gateway.
	if st := domotica.FastSteps(d, true); len(st) < 3 || st[2].Layer != domotica.LayerEncoder {
		t.Fatalf("steps: %+v", st)
	}

	// Réplica caída con la capa forzada: escala a VON diciendo por qué.
	h2, _, reps2, _ := newRoomGateway(t, true)
	reps2.fail = errors.New("no capacity")
	rec = post(t, h2, "/v1/decide", map[string]any{"task": "room", "text": "está muy oscuro"})
	var d2 domotica.Decision
	_ = json.Unmarshal(rec.Body.Bytes(), &d2)
	if d2.Confident || d2.Escalate != domotica.EscalateVON || d2.EncoderError == "" {
		t.Fatalf("encoder down: %+v", d2)
	}
}
