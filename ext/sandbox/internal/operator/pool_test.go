package operator

// SandboxPool: el operador concilia spec.size contra PUT /v1/templates/{name}/pool
// del frontal y escribe status.available. Un API de Kubernetes y un frontal
// falsos, sin clúster ni KVM.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/pkg/api"
)

type k8sPools struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	objs  map[string]*Pool
	rv    int
	event chan Pool // MODIFIED hacia el watch abierto
}

func nuevoK8sPools(t *testing.T) *k8sPools {
	t.Helper()
	f := &k8sPools{t: t, objs: map[string]*Pool{}, event: make(chan Pool, 8)}
	base := "/apis/" + Group + "/" + Version + "/"
	mux := http.NewServeMux()
	mux.HandleFunc(base+PoolResource, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "1" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			enc := json.NewEncoder(w)
			for {
				select {
				case <-r.Context().Done():
					return
				case p := <-f.event:
					enc.Encode(map[string]any{"type": "MODIFIED", "object": p})
					w.(http.Flusher).Flush()
				}
			}
		}
		f.mu.Lock()
		items := []Pool{}
		for _, o := range f.objs {
			items = append(items, *o)
		}
		rv := strconv.Itoa(f.rv)
		f.mu.Unlock()
		writeTestJSON(w, 200, map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "items": items})
	})
	mux.HandleFunc(base+"namespaces/", func(w http.ResponseWriter, r *http.Request) {
		// /namespaces/{ns}/sandboxpools/{name}/status
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"namespaces/"), "/")
		if len(parts) != 4 || parts[1] != PoolResource || parts[3] != "status" || r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		o, ok := f.objs[parts[0]+"/"+parts[2]]
		if !ok {
			writeTestJSON(w, 404, status{Kind: "Status", Code: 404, Message: "no pool"})
			return
		}
		patch := struct {
			Status PoolStatus `json:"status"`
		}{o.Status}
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		o.Status = patch.Status
		f.rv++
		o.Metadata.ResourceVersion = strconv.Itoa(f.rv)
		writeTestJSON(w, 200, o)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *k8sPools) client() *KubeClient { return NewKubeClient(&KubeConfig{BaseURL: f.srv.URL}) }

func (f *k8sPools) seed(p Pool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	p.Metadata.ResourceVersion = strconv.Itoa(f.rv)
	f.objs[p.Metadata.Namespace+"/"+p.Metadata.Name] = &p
}

func (f *k8sPools) get(key string) Pool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.objs[key]
}

// cambiarSize simula `kubectl edit`: sube generation y avisa por el watch.
func (f *k8sPools) cambiarSize(key string, n int) {
	f.mu.Lock()
	o := f.objs[key]
	o.Spec.Size = n
	o.Metadata.Generation++
	f.rv++
	o.Metadata.ResourceVersion = strconv.Itoa(f.rv)
	cp := *o
	f.mu.Unlock()
	f.event <- cp
}

type frontalPools struct {
	srv *httptest.Server

	mu       sync.Mutex
	puts     []string // "plantilla=tamaño"
	tokens   []string
	respCode int // 0 = 200
	respMsg  string
	ready    int
}

func nuevoFrontalPools(t *testing.T) *frontalPools {
	t.Helper()
	f := &frontalPools{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/templates/{name}/pool", func(w http.ResponseWriter, r *http.Request) {
		var pet api.PoolRequest
		json.NewDecoder(r.Body).Decode(&pet)
		f.mu.Lock()
		f.puts = append(f.puts, r.PathValue("name")+"="+strconv.Itoa(pet.Size))
		f.tokens = append(f.tokens, r.Header.Get("Authorization"))
		code, msg, ready := f.respCode, f.respMsg, f.ready
		f.mu.Unlock()
		if code != 0 {
			writeTestJSON(w, code, api.Error{Message: msg})
			return
		}
		writeTestJSON(w, 200, frontal.PoolRespuesta{Template: r.PathValue("name"), Size: pet.Size, Available: ready})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *frontalPools) llamadas() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.puts...)
}

func nuevoPool(ns, name, plantilla string, size int) Pool {
	return Pool{Metadata: ObjectMeta{Name: name, Namespace: ns, Generation: 1}, Spec: PoolSpec{Template: plantilla, Size: size}}
}

func correrPools(t *testing.T, k *k8sPools, fr *frontalPools, resync time.Duration) {
	t.Helper()
	pc := NewPoolController(k.client(), NewFrontalClient(fr.srv.URL, "admin-tok"), nil)
	pc.BackoffBase = 10 * time.Millisecond
	pc.Resync = resync
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pc.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("PoolController.Run no volvió")
		}
	})
}

func TestPool_ConciliaTamanoYDaAvailable(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	fr.ready = 2
	k.seed(nuevoPool("default", "py", "python", 3))
	correrPools(t, k, fr, time.Hour)

	waitFor(t, "status.available", func() bool {
		a := k.get("default/py").Status.Available
		return a != nil && *a == 2
	})
	if got := fr.llamadas(); len(got) == 0 || got[0] != "python=3" {
		t.Fatalf("llamadas al frontal: %v", got)
	}
	if fr.tokens[0] != "Bearer admin-tok" {
		t.Errorf("token %q", fr.tokens[0])
	}
	if m := k.get("default/py").Status.Message; m != nil && *m != "" {
		t.Errorf("message %q", *m)
	}
}

func TestPool_UnCambioDeSizeSeAplicaPorWatch(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	k.seed(nuevoPool("default", "py", "python", 1))
	correrPools(t, k, fr, time.Hour)
	waitFor(t, "primer PUT", func() bool { return len(fr.llamadas()) >= 1 })
	// Esperar a que el watch esté abierto antes de emitir.
	time.Sleep(100 * time.Millisecond)
	k.cambiarSize("default/py", 4)
	waitFor(t, "PUT con size 4", func() bool {
		l := fr.llamadas()
		return len(l) >= 2 && l[len(l)-1] == "python=4"
	})
}

func TestPool_SizeSinCambioNoVuelveALlamarEntreResyncs(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	k.seed(nuevoPool("default", "py", "python", 2))
	correrPools(t, k, fr, time.Hour)
	waitFor(t, "status", func() bool { return k.get("default/py").Status.Available != nil })
	time.Sleep(150 * time.Millisecond)
	if n := len(fr.llamadas()); n != 1 {
		t.Fatalf("%d llamadas, quiero 1", n)
	}
}

func TestPool_ErrorDelFrontalVaAMessage(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	fr.respCode, fr.respMsg = 404, "no host has template python"
	k.seed(nuevoPool("default", "py", "python", 2))
	correrPools(t, k, fr, time.Hour)
	waitFor(t, "status.message", func() bool {
		m := k.get("default/py").Status.Message
		return m != nil && strings.Contains(*m, "no host has template")
	})
}

func TestPool_SizeFueraDeRangoNoSeEnvia(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	k.seed(nuevoPool("default", "py", "python", api.PoolMax+1))
	correrPools(t, k, fr, time.Hour)
	waitFor(t, "status.message", func() bool {
		m := k.get("default/py").Status.Message
		return m != nil && strings.Contains(*m, "size")
	})
	if n := len(fr.llamadas()); n != 0 {
		t.Fatalf("se mandó un tamaño inválido (%d llamadas)", n)
	}
}

func TestPool_PlantillaConBarraNoSeEnvia(t *testing.T) {
	k := nuevoK8sPools(t)
	fr := nuevoFrontalPools(t)
	k.seed(nuevoPool("default", "py", "../hosts", 1))
	correrPools(t, k, fr, time.Hour)
	waitFor(t, "status.message", func() bool {
		m := k.get("default/py").Status.Message
		return m != nil && strings.Contains(*m, "template")
	})
	if n := len(fr.llamadas()); n != 0 {
		t.Fatalf("%d llamadas", n)
	}
}
