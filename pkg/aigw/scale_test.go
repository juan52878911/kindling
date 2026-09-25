package aigw

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/von"
)

// fakeDaemon es lo justo del API del daemon para que pkg/scheduler despierte,
// congele y cree réplicas: máquinas en memoria cuyo puerto 8000 reenvía al
// llama falso (como en macOS, con Forwards).
type fakeDaemon struct {
	mu       sync.Mutex
	llama    string
	machines map[string]*api.Machine
	order    []string
	runs     []api.RunRequest
	thaws    int
	freezes  int
}

func (d *fakeDaemon) serve(t *testing.T) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "aigw") // corto: un socket Unix de macOS no pasa de 104 bytes
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
	send := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		out := []*api.Machine{}
		for _, id := range d.order {
			c := *d.machines[id]
			out = append(out, &c)
		}
		send(w, out)
	})
	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.runs = append(d.runs, req)
		id := fmt.Sprintf("m%02d", len(d.runs))
		m := &api.Machine{ID: id, Name: req.Name, State: api.StateRunning, From: req.From, MemMiB: 768,
			TTLSeconds: req.TTLSeconds, IP: "10.0.0.9",
			Labels:   api.MergeLabels(map[string]string{api.LabelService: req.From, von.LabelModel: "smol:q8_0"}, req.Labels),
			Forwards: map[string]string{"8000": d.llama}}
		d.machines[id] = m
		d.order = append(d.order, id)
		send(w, m)
	})
	state := func(to api.State, count *int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			defer d.mu.Unlock()
			m := d.machines[r.PathValue("ref")]
			if m == nil {
				http.Error(w, `{"error":"no such machine"}`, 404)
				return
			}
			m.State = to
			*count++
			c := *m
			send(w, &c)
		}
	}
	mux.HandleFunc("POST /machines/{ref}/thaw", state(api.StateRunning, &d.thaws))
	mux.HandleFunc("POST /machines/{ref}/freeze", state(api.StateWarm, &d.freezes))
	mux.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		send(w, []*api.Snapshot{{Name: "von-smol", MemMiB: 768, Labels: map[string]string{api.LabelService: "von-smol"}}})
	})
	mux.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		send(w, api.GuestResponse{Status: 200})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return api.NewClient(sock)
}

func (d *fakeDaemon) counts() (runs, thaws, freezes, running int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.machines {
		if m.State == api.StateRunning {
			running++
		}
	}
	return len(d.runs), d.thaws, d.freezes, running
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Escala a cero de verdad, con pkg/scheduler: la primera escalada restaura una
// réplica del dorado, la ociosa se congela, la siguiente la descongela (no crea
// otra), la concurrencia añade réplicas hasta el tope, una ráfaga posterior
// reutiliza las congeladas, una máquina ajena nunca se toca, y al cerrar no
// queda nada corriendo.
func TestEscalaACero(t *testing.T) {
	ll := newFakeLlama(t)
	d := &fakeDaemon{llama: strings.TrimPrefix(ll.srv.URL, "http://"), machines: map[string]*api.Machine{}}
	client := d.serve(t)

	// Una réplica de un humano (`kling run -from von-smol`), sin la etiqueta del gateway.
	d.machines["user1"] = &api.Machine{ID: "user1", Name: "smol-1", State: api.StateRunning, From: "von-smol",
		Labels: map[string]string{api.LabelService: "von-smol"}, Forwards: map[string]string{"8000": d.llama}}
	d.order = append(d.order, "user1")

	cfg := &Config{
		Models: map[string]*ModelConfig{
			"commits": {Kind: KindChispa, Path: trainedModel(t)},
			"smol":    {Kind: KindVON, Snapshot: "von-smol"},
		},
		Tasks: map[string]*TaskConfig{"kind": {Chispa: "commits", EscalateTo: "smol", EscalateForce: true,
			Thresholds: map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}}},
	}
	g, err := New(Options{Client: client, Config: cfg, Idle: 300 * time.Millisecond, MaxReplicas: 2, MaxInflight: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.Start(ctx)
	h := g.Handler("")
	ask := func() int {
		return do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash"}).Code
	}

	if c := ask(); c != 200 {
		t.Fatalf("first escalation: %d", c)
	}
	runs, thaws, _, _ := d.counts()
	if runs != 1 || thaws != 0 {
		t.Fatalf("first request: runs=%d thaws=%d, want a restore", runs, thaws)
	}
	d.mu.Lock()
	r0 := d.runs[0]
	d.mu.Unlock()
	if r0.Labels[LabelGateway] != "default" || !strings.HasPrefix(r0.Name, "gw-von-smol-") || r0.TTLSeconds != 0 || r0.From != "von-smol" {
		t.Fatalf("run request = %+v", r0)
	}
	ask()
	if runs, thaws, _, _ := d.counts(); runs != 1 || thaws != 0 {
		t.Fatalf("warm request woke something: runs=%d thaws=%d", runs, thaws)
	}

	// Ociosa -> congelada.
	waitFor(t, "idle freeze", func() bool { _, _, f, _ := d.counts(); return f == 1 })
	if c := ask(); c != 200 {
		t.Fatal(c)
	}
	if runs, thaws, _, _ := d.counts(); runs != 1 || thaws != 1 {
		t.Fatalf("after idle: runs=%d thaws=%d, want the same machine thawed", runs, thaws)
	}

	// Concurrencia: tres peticiones retenidas -> dos réplicas (el tope), no tres.
	ll.entered, ll.block = make(chan struct{}, 8), make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ask() }()
		<-ll.entered
	}
	if runs, _, _, _ := d.counts(); runs != 2 {
		t.Fatalf("scale-out: %d machines created, want 2 (max_replicas)", runs)
	}
	close(ll.block)
	wg.Wait()
	ll.block = nil

	// Las dos se congelan; una ráfaga nueva las descongela en vez de crear más.
	waitFor(t, "both frozen", func() bool {
		_, _, _, running := d.counts()
		return running == 1 // solo la del humano
	})
	ll.entered, ll.block = make(chan struct{}, 8), make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ask() }()
		<-ll.entered
	}
	close(ll.block)
	wg.Wait()
	ll.block = nil
	runs, thaws, _, _ = d.counts()
	if runs != 2 || thaws != 3 {
		t.Fatalf("second burst: runs=%d thaws=%d, want 2 and 3 (frozen replicas reused)", runs, thaws)
	}

	m := do(t, h, "GET", "/metrics", "", nil).Body.String()
	for _, want := range []string{
		`kling_ai_von_wake_seconds_count{model="smol",how="restore"} 2`,
		`kling_ai_von_wake_seconds_count{model="smol",how="thaw"} 3`,
		`kling_ai_von_replicas{model="smol",state="running"} 2`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	cancel()
	g.Close(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, mc := range d.machines {
		if id == "user1" {
			if mc.State != api.StateRunning {
				t.Fatal("the gateway touched a machine it doesn't own")
			}
			continue
		}
		if mc.State != api.StateWarm {
			t.Fatalf("%s still %s after Close", id, mc.State)
		}
	}
}
