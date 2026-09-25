package von

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonFalso es un daemon de mentira por un socket Unix: lo justo del API
// (run, guest, commit, rm) para probar el flujo del dorado sin microVMs.
type daemonFalso struct {
	mu          sync.Mutex
	cargando    int // respuestas 503 de /health antes del 200
	chats       []ChatRequest
	run         api.RunRequest
	commit      api.CommitRequest
	borradas    []string
	peticiones  []string
	warmFalla   bool // el /v1/chat/completions del calentamiento responde 500
	commitFalla bool // el /commit responde 500
}

func (d *daemonFalso) servir(t *testing.T) *api.Client {
	t.Helper()
	// Ruta corta: en macOS un socket Unix no admite más de 104 bytes, y el
	// TempDir de los tests se pasa.
	dir, err := os.MkdirTemp("", "von")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&d.run)
		_ = json.NewEncoder(w).Encode(api.Machine{ID: "m1", Name: d.run.Name, BootMS: 42, Labels: d.run.Labels})
	})
	mux.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		var g api.GuestRequest
		_ = json.NewDecoder(r.Body).Decode(&g)
		d.mu.Lock()
		defer d.mu.Unlock()
		if g.Port != Port {
			http.Error(w, `{"error":"wrong port"}`, http.StatusForbidden)
			return
		}
		d.peticiones = append(d.peticiones, g.Method+" "+g.Path)
		var out api.GuestResponse
		switch {
		case g.ProbeOnly:
			out.Status = 200
		case g.Path == "/health" && d.cargando > 0:
			d.cargando--
			out = api.GuestResponse{Status: 503, Body: `{"error":{"code":503,"message":"Loading model"}}`}
		case g.Path == "/health":
			out = api.GuestResponse{Status: 200, Body: `{"status":"ok"}`}
		case g.Path == "/v1/chat/completions":
			if d.warmFalla {
				out = api.GuestResponse{Status: 500, Body: `{"error":{"code":500,"message":"boom"}}`}
				break
			}
			var req ChatRequest
			_ = json.Unmarshal([]byte(g.Body), &req)
			d.chats = append(d.chats, req)
			if req.MaxTokens == 0 || len(req.Messages) == 0 {
				out = api.GuestResponse{Status: 400, Body: "bad"}
				break
			}
			out = api.GuestResponse{Status: 200, Body: `{"model":"m:q8_0","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
				`"timings":{"prompt_n":12,"prompt_per_second":400,"predicted_n":2,"predicted_per_second":50}}`}
		default:
			out.Status = 404
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&d.commit)
		if d.commitFalla {
			http.Error(w, `{"error":{"code":500,"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(api.Snapshot{Name: d.commit.Name, MemBytes: 500 << 20, Labels: d.run.Labels})
	})
	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.borradas = append(d.borradas, r.PathValue("ref"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return api.NewClient(sock)
}

func TestMakeGolden(t *testing.T) {
	d := &daemonFalso{cargando: 3}
	c := d.servir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	g, err := MakeGolden(ctx, c, GoldenOptions{
		Image: "von-smol", Snapshot: "von-smol", Ref: "smollm2-360m-instruct:q8_0",
		VCPUs: 2, MemMiB: 768, AllowExec: true, Wait: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Snapshot.Name != "von-smol" || g.Warm.Text() != "Hello" || g.BootMS != 42 {
		t.Fatalf("resultado: %+v", g)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// La plantilla arranca sin red, con el puerto declarado y la etiqueta del
	// modelo, que el dorado hereda; y se borra al acabar.
	if d.run.Egress != "none" || d.run.CPUPct != 200 || d.run.Labels[api.LabelPorts] != "8000" ||
		d.run.Labels[LabelModel] != "smollm2-360m-instruct:q8_0" || !strings.HasPrefix(d.run.Name, "von-smol-golden-") {
		t.Fatalf("run: %+v", d.run)
	}
	if d.cargando != 0 {
		t.Fatalf("no esperó a que cargara: quedan %d 503", d.cargando)
	}
	if d.commit.Name != "von-smol" || len(d.borradas) != 1 {
		t.Fatalf("commit %+v, borradas %v", d.commit, d.borradas)
	}
	// El calentamiento va DESPUÉS de /health 200 y ANTES del commit.
	ult := d.peticiones[len(d.peticiones)-1]
	if ult != "POST /v1/chat/completions" {
		t.Fatalf("última petición antes del commit: %q (%v)", ult, d.peticiones)
	}
}

func TestWaitReadyAgotaPlazo(t *testing.T) {
	d := &daemonFalso{cargando: 1 << 30}
	c := d.servir(t)
	err := WaitReady(context.Background(), c, "m1", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still loading") {
		t.Fatalf("debería rendirse diciendo que sigue cargando: %v", err)
	}
}

// Los tres siguientes prueban que, pase lo que pase en el camino, MakeGolden
// deja el error subir Y borra la plantilla igual: el defer de client.go que
// hace el Remove no depende de en qué paso se rompió.

func TestMakeGoldenErrorWaitReady(t *testing.T) {
	// cargando altísimo: /health nunca contesta 200 antes de que el Wait
	// (deliberadamente corto) se agote.
	d := &daemonFalso{cargando: 1 << 30}
	c := d.servir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	g, err := MakeGolden(ctx, c, GoldenOptions{
		Image: "von-smol", Snapshot: "von-smol", Ref: "smollm2-360m-instruct:q8_0",
		VCPUs: 1, MemMiB: 256, Wait: 200 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "still loading") {
		t.Fatalf("err = %v, quería que siguiera diciendo que carga", err)
	}
	if g != nil {
		t.Fatalf("resultado = %+v, quería nil", g)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.borradas) != 1 || d.borradas[0] != "m1" {
		t.Fatalf("borradas = %v: la plantilla debe borrarse aunque WaitReady falle", d.borradas)
	}
	if d.commit.Name != "" {
		t.Fatalf("no debería haber llegado a hacer commit: %+v", d.commit)
	}
}

func TestMakeGoldenErrorWarm(t *testing.T) {
	// El modelo carga bien (cargando: 0), pero el calentamiento revienta.
	d := &daemonFalso{warmFalla: true}
	c := d.servir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	g, err := MakeGolden(ctx, c, GoldenOptions{
		Image: "von-smol", Snapshot: "von-smol", Ref: "smollm2-360m-instruct:q8_0",
		VCPUs: 1, MemMiB: 256, Wait: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "warm-up") {
		t.Fatalf("err = %v, quería un error de warm-up", err)
	}
	if g != nil {
		t.Fatalf("resultado = %+v, quería nil", g)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.borradas) != 1 || d.borradas[0] != "m1" {
		t.Fatalf("borradas = %v: la plantilla debe borrarse aunque el calentamiento falle", d.borradas)
	}
	if d.commit.Name != "" {
		t.Fatalf("no debería haber llegado a hacer commit: %+v", d.commit)
	}
}

func TestMakeGoldenErrorCommit(t *testing.T) {
	// Carga y calentamiento bien, pero el commit (congelar el dorado) revienta.
	d := &daemonFalso{commitFalla: true}
	c := d.servir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	g, err := MakeGolden(ctx, c, GoldenOptions{
		Image: "von-smol", Snapshot: "von-smol", Ref: "smollm2-360m-instruct:q8_0",
		VCPUs: 1, MemMiB: 256, Wait: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %v, quería un error de commit", err)
	}
	if g != nil {
		t.Fatalf("resultado = %+v, quería nil", g)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.borradas) != 1 || d.borradas[0] != "m1" {
		t.Fatalf("borradas = %v: la plantilla debe borrarse aunque el commit falle", d.borradas)
	}
}

func TestChat(t *testing.T) {
	c := (&daemonFalso{}).servir(t)
	r, _, err := Chat(context.Background(), c, "m1", ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 4,
	})
	if err != nil || r.Text() != "Hello" || r.Timings == nil || r.Timings.PredictedPerSecond != 50 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, _, err := Chat(context.Background(), c, "m1", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("un 400 del servidor debería ser error")
	}
	if _, _, err := Chat(context.Background(), c, "m1", ChatRequest{Stream: true}); err == nil {
		t.Fatal("stream por el proxy debería rechazarse")
	}
}

func TestMakeGoldenPrefijos(t *testing.T) {
	d := &daemonFalso{}
	c := d.servir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pre := []Prefix{{System: "You control a smart home."}, {System: "You triage tickets.", User: "Ticket:"}}
	g, err := MakeGolden(ctx, c, GoldenOptions{
		Image: "q", Snapshot: "q", Ref: "q:q8_0", VCPUs: 2, MemMiB: 1024, Wait: 5 * time.Second,
		Prefixes: pre, Labels: map[string]string{LabelPrefixes: "viejo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// El calentamiento y después cada prefijo, en orden, con un token a
	// temperatura 0; el último es el que queda en la ranura.
	if len(d.chats) != 3 || len(g.PrefixTokens) != 2 {
		t.Fatalf("peticiones: %+v, tokens %v", d.chats, g.PrefixTokens)
	}
	for i, p := range pre {
		r := d.chats[i+1]
		if r.MaxTokens != 1 || r.Temperature == nil || *r.Temperature != 0 ||
			r.Messages[0].Content != p.System || r.Messages[len(r.Messages)-1].Role != "user" {
			t.Fatalf("prefijo %d: %+v", i, r)
		}
	}
	if d.chats[2].Messages[1].Content != "Ticket:" || d.chats[1].Messages[1].Content != "Hi" {
		t.Fatalf("turno del usuario: %+v", d.chats)
	}
	// La etiqueta dice qué prefijos lleva el dorado; la que venía se pisa.
	if d.run.Labels[LabelPrefixes] != PrefixesHash(pre) || PrefixesHash(pre) == "" {
		t.Fatalf("etiqueta: %v", d.run.Labels)
	}
}

func TestPrefixesHash(t *testing.T) {
	a := []Prefix{{System: "a"}, {System: "b", User: "u"}}
	b := []Prefix{{System: "b", User: "u"}, {System: "a"}}
	if PrefixesHash(a) != PrefixesHash(b) || len(PrefixesHash(a)) != 12 {
		t.Fatalf("el orden no cuenta: %s %s", PrefixesHash(a), PrefixesHash(b))
	}
	if PrefixesHash(nil) != "" || PrefixesHash(a) == PrefixesHash(a[:1]) {
		t.Fatal("conjuntos distintos, hashes distintos; ninguno, vacío")
	}
}
