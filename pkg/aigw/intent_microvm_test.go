package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/domotica"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// La capa 2 de una tarea de domótica servida serverless: el modelo de
// intención es backend "microvm" y un kling-chispa falso hace de réplica.

// wakeReplicas es fakeReplicas con el despertar que el planificador habría
// entregado a la siguiente petición (uno por Acquire, como TakeWake).
type wakeReplicas struct {
	addr string
	fail error
	mu   sync.Mutex
	next *scheduler.WakeTrace
}

func (r *wakeReplicas) Acquire(context.Context, string) (*Replica, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	r.mu.Lock()
	w := r.next
	r.next = nil
	r.mu.Unlock()
	return &Replica{Addr: r.addr, Release: func() {}, Drop: func() {}, Wake: w}, nil
}

func (r *wakeReplicas) wake(how string, total time.Duration) {
	r.mu.Lock()
	r.next = &scheduler.WakeTrace{How: how, Total: total}
	r.mu.Unlock()
}

// domoGuest es un kling-chispa falso: contesta lo que diga answer(text).
type domoGuest struct {
	mu     sync.Mutex
	answer func(text string) map[string]any
	last   chispaGuestRequest
}

func newDomoMicroVM(t *testing.T, rec ChispaDeployRecord, localSlots string) (*Gateway, *domoGuest, *wakeReplicas) {
	t.Helper()
	fg := &domoGuest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/classify" {
			http.NotFound(w, r)
			return
		}
		var req chispaGuestRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fg.mu.Lock()
		fg.last = req
		ans := fg.answer(req.Text)
		fg.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ans)
	}))
	t.Cleanup(srv.Close)
	reps := &wakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")}
	cfg := &Config{
		Models: map[string]*ModelConfig{"intent": {Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-room"}},
		Tasks:  map[string]*TaskConfig{"home": {Domotica: &DomoticaConfig{Intent: "intent", Slots: localSlots}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-room": rec}
	return g, fg, reps
}

var roomRecord = ChispaDeployRecord{
	Labels: []string{"turn_on", "turn_off", domotica.OutOfScope},
	Slots:  []string{domotica.SlotDevice, domotica.SlotArea},
}

// span marca sub dentro de text como el hueco slot (con un Text mentiroso:
// el gateway debe rehacerlo del texto de la petición).
func span(text, slot, sub string) map[string]any {
	i := strings.Index(text, sub)
	return map[string]any{"slot": slot, "start": i, "end": i + len(sub), "text": "lo que diga el invitado"}
}

func decideHome(t *testing.T, g *Gateway, text string) DecideResponse {
	t.Helper()
	rec := do(t, g.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "home", "text": text, "lang": "es"})
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
	}
	return decode[DecideResponse](t, rec)
}

const kitchenText = "oye quiero la luz de la cocina ya"

func TestDomoticaMicroVMIntentAndSlots(t *testing.T) {
	g, fg, reps := newDomoMicroVM(t, roomRecord, "")
	fg.answer = func(text string) map[string]any {
		return map[string]any{"label": "turn_on", "prob": 0.93, "threshold": 0.5, "confident": true, "decision": "confident",
			"slots": []any{span(text, domotica.SlotDevice, "luz"), span(text, domotica.SlotArea, "cocina")}}
	}

	// Congelada: la primera orden la descongela y la traza lo dice.
	reps.wake("thaw", 27*time.Millisecond)
	d := decideHome(t, g, kitchenText)
	if d.Layer != domotica.LayerChispa || !d.Confident || d.Label != "turn_on" {
		t.Fatalf("got %+v", d)
	}
	if d.Slots.Device != domotica.DevLight || d.Slots.Area != "kitchen" {
		t.Fatalf("the replica's slots were not used: %+v", d.Slots)
	}
	for _, sp := range d.Spans {
		if sp.Text != kitchenText[sp.Start:sp.End] {
			t.Fatalf("span text must come from the request, not the guest: %+v", sp)
		}
	}
	r := d.ChispaReplica
	if r == nil || r.State != domotica.ReplicaFrozen || r.WakeMS != 27 || r.Model != "intent" {
		t.Fatalf("replica trace: %+v", r)
	}
	if fg.last.Fields["lang"] != "es" || fg.last.Explain {
		t.Fatalf("the replica got %+v", fg.last)
	}
	steps := domotica.FastSteps(d.Decision, false)
	if steps[1].Layer != domotica.LayerChispa || steps[1].Replica == nil || steps[1].Replica.State != domotica.ReplicaFrozen {
		t.Fatalf("the chispa step must carry the replica: %+v", steps)
	}

	// Ya despierta: sin despertar.
	d = decideHome(t, g, kitchenText)
	if d.ChispaReplica == nil || d.ChispaReplica.State != domotica.ReplicaWarm || d.ChispaReplica.WakeMS != 0 {
		t.Fatalf("warm: %+v", d.ChispaReplica)
	}
	// Pausada: se reanuda.
	reps.wake("resume", 2*time.Millisecond)
	if d = decideHome(t, g, kitchenText); d.ChispaReplica.State != domotica.ReplicaPaused || d.ChispaReplica.WakeMS != 2 {
		t.Fatalf("paused: %+v", d.ChispaReplica)
	}
	// Sin réplica: del dorado.
	reps.wake("restore", 90*time.Millisecond)
	if d = decideHome(t, g, kitchenText); d.ChispaReplica.State != domotica.ReplicaNew {
		t.Fatalf("restore: %+v", d.ChispaReplica)
	}

	// /v1/tasks dice dónde vive Chispa (la demo lo enseña).
	rec := do(t, g.Handler(""), http.MethodGet, "/v1/tasks", "", nil)
	if !strings.Contains(rec.Body.String(), `"chispa_backend":"microvm"`) {
		t.Fatalf("tasks: %s", rec.Body)
	}
}

// La confianza la decide el gateway: un "confident" del invitado por debajo
// del umbral no vale.
func TestDomoticaMicroVMConfidenceIsGatewaySide(t *testing.T) {
	g, fg, _ := newDomoMicroVM(t, roomRecord, "")
	fg.answer = func(text string) map[string]any {
		return map[string]any{"label": "turn_on", "prob": 0.3, "threshold": 0.5, "confident": true, "decision": "confident",
			"slots": []any{span(text, domotica.SlotDevice, "luz")}}
	}
	d := decideHome(t, g, kitchenText)
	if d.Confident || d.Reason != domotica.ReasonLowProb || d.Escalate != domotica.EscalateTo {
		t.Fatalf("got %+v", d)
	}
}

// Una réplica que miente (etiqueta que no existe, hueco fuera del texto o de
// un nombre desconocido) no decide nada: la orden escala con el motivo.
func TestDomoticaMicroVMInvalidReplies(t *testing.T) {
	for name, ans := range map[string]func(string) map[string]any{
		"unknown label": func(string) map[string]any {
			return map[string]any{"label": "make_coffee", "prob": 0.99, "threshold": 0.5}
		},
		"prob out of range": func(string) map[string]any {
			return map[string]any{"label": "turn_on", "prob": 7, "threshold": 0.5}
		},
		"span outside the text": func(text string) map[string]any {
			return map[string]any{"label": "turn_on", "prob": 0.9, "threshold": 0.5,
				"slots": []any{map[string]any{"slot": domotica.SlotDevice, "start": 3, "end": len(text) + 40}}}
		},
		"unknown slot": func(text string) map[string]any {
			return map[string]any{"label": "turn_on", "prob": 0.9, "threshold": 0.5,
				"slots": []any{span(text, "password", "luz")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g, fg, _ := newDomoMicroVM(t, roomRecord, "")
			fg.answer = ans
			d := decideHome(t, g, kitchenText)
			if d.Confident || d.Reason != domotica.ReasonChispaError || d.ChispaError == "" || d.Escalate != domotica.EscalateTo {
				t.Fatalf("got %+v", d)
			}
			if st := domotica.FastSteps(d.Decision, false); st[1].Status != domotica.StepError {
				t.Fatalf("steps: %+v", st)
			}
		})
	}
}

// Sin réplica disponible la orden escala; la traza no inventa un estado.
func TestDomoticaMicroVMWakeError(t *testing.T) {
	g, _, reps := newDomoMicroVM(t, roomRecord, "")
	reps.fail = errors.New("no capacity")
	d := decideHome(t, g, kitchenText)
	if d.Confident || d.Reason != domotica.ReasonChispaError || d.ChispaReplica != nil {
		t.Fatalf("got %+v", d)
	}
	// Las plantillas siguen contestando sin tocar la microVM.
	if d = decideHome(t, g, "enciende la luz"); d.Layer != domotica.LayerTemplate || !d.Confident {
		t.Fatalf("template: %+v", d)
	}
}

// Un dorado sin huecos en su registro (sin -slots, o de antes): lo que mande
// la réplica se ignora; con "slots" en la tarea los marca el proceso.
func TestDomoticaMicroVMSlotsWithoutRecord(t *testing.T) {
	norec := ChispaDeployRecord{Labels: roomRecord.Labels}
	answer := func(text string) map[string]any {
		return map[string]any{"label": "turn_on", "prob": 0.9, "threshold": 0.5,
			"slots": []any{span(text, "whatever", "luz")}}
	}
	g, fg, _ := newDomoMicroVM(t, norec, "")
	fg.answer = answer
	d := decideHome(t, g, kitchenText)
	if d.Layer != domotica.LayerChispa || len(d.Spans) != 0 || d.ChispaError != "" {
		t.Fatalf("slots from a replica without slots in its record must be ignored: %+v", d)
	}

	// Con un .chispas local, los huecos salen de él (aquí no carga: 503,
	// igual que con Chispa en proceso), nunca de la réplica.
	g2, fg2, _ := newDomoMicroVM(t, norec, filepath.Join(t.TempDir(), "missing.chispas"))
	fg2.answer = answer
	rec := do(t, g2.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "home", "text": kitchenText})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestValidateGuestSpans(t *testing.T) {
	text := "pon la luz"
	ok := []string{"device"}
	for name, sp := range map[string][]slots.Span{
		"out of order":   {{Slot: "device", Start: 7, End: 10}, {Slot: "device", Start: 4, End: 6}},
		"empty":          {{Slot: "device", Start: 7, End: 7}},
		"negative start": {{Slot: "device", Start: -1, End: 3}},
		"too many":       make([]slots.Span, maxGuestSpans+1),
	} {
		if _, err := validateGuestSpans(ok, text, sp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// "salón": la ó ocupa los bytes 10 y 11; cortar en el 11 parte el carácter.
	if _, err := validateGuestSpans(ok, "pon el salón", []slots.Span{{Slot: "device", Start: 7, End: 11}}); err == nil {
		t.Error("a span that splits a UTF-8 character was accepted")
	}
	got, err := validateGuestSpans(ok, text, []slots.Span{{Slot: "device", Start: 7, End: 10, Text: "mentira"}})
	if err != nil || got[0].Text != "luz" {
		t.Fatalf("got %+v %v", got, err)
	}
}
