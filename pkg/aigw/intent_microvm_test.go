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
	"github.com/juan52878911/kindling/pkg/intent"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// La capa 2 de una tarea de intención servida serverless: el modelo de
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

// intentGuest es un kling-chispa falso: contesta lo que diga answer(text).
type intentGuest struct {
	mu     sync.Mutex
	answer func(text string) map[string]any
	last   chispaGuestRequest
}

func newIntentMicroVM(t *testing.T, rec ChispaDeployRecord, localSlots string) (*Gateway, *intentGuest, *wakeReplicas) {
	t.Helper()
	fg := &intentGuest{}
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
		Models: map[string]*ModelConfig{"intent": {Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-tickets"}},
		Tasks:  map[string]*TaskConfig{"tickets": {Intent: &IntentConfig{Model: "intent", Schema: ticketSchema(t), Slots: localSlots}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-tickets": rec}
	return g, fg, reps
}

var ticketRecord = ChispaDeployRecord{
	Labels: []string{"open_ticket", "close_ticket", testOOS},
	Slots:  []string{"queue", "priority"},
}

// span marca sub dentro de text como el hueco slot (con un Text mentiroso:
// el gateway debe rehacerlo del texto de la petición).
func span(text, slot, sub string) map[string]any {
	i := strings.Index(text, sub)
	return map[string]any{"slot": slot, "start": i, "end": i + len(sub), "text": "lo que diga el invitado"}
}

func decideTicket(t *testing.T, g *Gateway, text string) DecideResponse {
	t.Helper()
	rec := do(t, g.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "tickets", "text": text, "lang": "en"})
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
	}
	return decode[DecideResponse](t, rec)
}

const helpText = "hey I need one for the help desk now"

func TestIntentMicroVMIntentAndSlots(t *testing.T) {
	g, fg, reps := newIntentMicroVM(t, ticketRecord, "")
	fg.answer = func(text string) map[string]any {
		return map[string]any{"label": "open_ticket", "prob": 0.93, "threshold": 0.5, "confident": true, "decision": "confident",
			"slots": []any{span(text, "queue", "help desk")}}
	}

	// Congelada: la primera orden la descongela y la traza lo dice.
	reps.wake("thaw", 27*time.Millisecond)
	d := decideTicket(t, g, helpText)
	if d.Layer != intent.LayerChispa || !d.Confident || d.Label != "open_ticket" {
		t.Fatalf("got %+v", d)
	}
	if sl, _ := d.Slots.(map[string]any); sl["queue"] != "support" {
		t.Fatalf("the replica's slots were not used: %+v", d.Slots)
	}
	for _, sp := range d.Spans {
		if sp.Text != helpText[sp.Start:sp.End] {
			t.Fatalf("span text must come from the request, not the guest: %+v", sp)
		}
	}
	r := d.ChispaReplica
	if r == nil || r.State != intent.ReplicaFrozen || r.WakeMS != 27 || r.Model != "intent" {
		t.Fatalf("replica trace: %+v", r)
	}
	if fg.last.Fields["lang"] != "en" || fg.last.Explain {
		t.Fatalf("the replica got %+v", fg.last)
	}

	// Ya despierta: sin despertar.
	d = decideTicket(t, g, helpText)
	if d.ChispaReplica == nil || d.ChispaReplica.State != intent.ReplicaWarm || d.ChispaReplica.WakeMS != 0 {
		t.Fatalf("warm: %+v", d.ChispaReplica)
	}
	// Pausada: se reanuda.
	reps.wake("resume", 2*time.Millisecond)
	if d = decideTicket(t, g, helpText); d.ChispaReplica.State != intent.ReplicaPaused || d.ChispaReplica.WakeMS != 2 {
		t.Fatalf("paused: %+v", d.ChispaReplica)
	}
	// Sin réplica: del dorado.
	reps.wake("restore", 90*time.Millisecond)
	if d = decideTicket(t, g, helpText); d.ChispaReplica.State != intent.ReplicaNew {
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
func TestIntentMicroVMConfidenceIsGatewaySide(t *testing.T) {
	g, fg, _ := newIntentMicroVM(t, ticketRecord, "")
	fg.answer = func(text string) map[string]any {
		return map[string]any{"label": "open_ticket", "prob": 0.3, "threshold": 0.5, "confident": true, "decision": "confident",
			"slots": []any{span(text, "queue", "help desk")}}
	}
	d := decideTicket(t, g, helpText)
	if d.Confident || d.Reason != intent.ReasonLowProb || d.Escalate != intent.EscalateTo {
		t.Fatalf("got %+v", d)
	}
}

// Una réplica que miente (etiqueta que no existe, hueco fuera del texto o de
// un nombre desconocido) no decide nada: la orden escala con el motivo.
func TestIntentMicroVMInvalidReplies(t *testing.T) {
	for name, ans := range map[string]func(string) map[string]any{
		"unknown label": func(string) map[string]any {
			return map[string]any{"label": "make_coffee", "prob": 0.99, "threshold": 0.5}
		},
		"prob out of range": func(string) map[string]any {
			return map[string]any{"label": "open_ticket", "prob": 7, "threshold": 0.5}
		},
		"span outside the text": func(text string) map[string]any {
			return map[string]any{"label": "open_ticket", "prob": 0.9, "threshold": 0.5,
				"slots": []any{map[string]any{"slot": "queue", "start": 3, "end": len(text) + 40}}}
		},
		"unknown slot": func(text string) map[string]any {
			return map[string]any{"label": "open_ticket", "prob": 0.9, "threshold": 0.5,
				"slots": []any{span(text, "password", "help desk")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g, fg, _ := newIntentMicroVM(t, ticketRecord, "")
			fg.answer = ans
			d := decideTicket(t, g, helpText)
			if d.Confident || d.Reason != intent.ReasonChispaError || d.ChispaError == "" || d.Escalate != intent.EscalateTo {
				t.Fatalf("got %+v", d)
			}
		})
	}
}

// Sin réplica disponible la orden escala; la traza no inventa un estado.
func TestIntentMicroVMWakeError(t *testing.T) {
	g, _, reps := newIntentMicroVM(t, ticketRecord, "")
	reps.fail = errors.New("no capacity")
	d := decideTicket(t, g, helpText)
	if d.Confident || d.Reason != intent.ReasonChispaError || d.ChispaReplica != nil {
		t.Fatalf("got %+v", d)
	}
	// Las plantillas siguen contestando sin tocar la microVM.
	if d = decideTicket(t, g, "open a ticket"); d.Layer != intent.LayerTemplate || !d.Confident {
		t.Fatalf("template: %+v", d)
	}
}

// Un dorado sin huecos en su registro (sin -slots, o de antes): lo que mande
// la réplica se ignora; con "slots" en la tarea los marca el proceso.
func TestIntentMicroVMSlotsWithoutRecord(t *testing.T) {
	norec := ChispaDeployRecord{Labels: ticketRecord.Labels}
	answer := func(text string) map[string]any {
		return map[string]any{"label": "open_ticket", "prob": 0.9, "threshold": 0.5,
			"slots": []any{span(text, "whatever", "help desk")}}
	}
	g, fg, _ := newIntentMicroVM(t, norec, "")
	fg.answer = answer
	d := decideTicket(t, g, helpText)
	if d.Layer != intent.LayerChispa || len(d.Spans) != 0 || d.ChispaError != "" {
		t.Fatalf("slots from a replica without slots in its record must be ignored: %+v", d)
	}

	// Con un .chispas local, los huecos salen de él (aquí no carga: 503,
	// igual que con Chispa en proceso), nunca de la réplica.
	g2, fg2, _ := newIntentMicroVM(t, norec, filepath.Join(t.TempDir(), "missing.chispas"))
	fg2.answer = answer
	rec := do(t, g2.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "tickets", "text": helpText})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestValidateGuestSpans(t *testing.T) {
	text := "fix my car"
	ok := []string{"item"}
	for name, sp := range map[string][]slots.Span{
		"out of order":   {{Slot: "item", Start: 7, End: 10}, {Slot: "item", Start: 4, End: 6}},
		"empty":          {{Slot: "item", Start: 7, End: 7}},
		"negative start": {{Slot: "item", Start: -1, End: 3}},
		"too many":       make([]slots.Span, maxGuestSpans+1),
	} {
		if _, err := validateGuestSpans(ok, text, sp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// "menú": la ú ocupa los bytes 10 y 11; cortar en el 11 parte el carácter.
	if _, err := validateGuestSpans(ok, "ver el menú", []slots.Span{{Slot: "item", Start: 7, End: 11}}); err == nil {
		t.Error("a span that splits a UTF-8 character was accepted")
	}
	got, err := validateGuestSpans(ok, text, []slots.Span{{Slot: "item", Start: 7, End: 10, Text: "mentira"}})
	if err != nil || got[0].Text != "car" {
		t.Fatalf("got %+v %v", got, err)
	}
}
