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
	mu         sync.Mutex
	cargando   int // respuestas 503 de /health antes del 200
	run        api.RunRequest
	commit     api.CommitRequest
	borradas   []string
	peticiones []string
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
			var req ChatRequest
			_ = json.Unmarshal([]byte(g.Body), &req)
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
