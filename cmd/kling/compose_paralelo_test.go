package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestEnParaleloRespetaElTope(t *testing.T) {
	var a, pico atomic.Int32
	hechos := make([]bool, 20)
	enParalelo(len(hechos), 3, func(i int) {
		n := a.Add(1)
		for {
			p := pico.Load()
			if n <= p || pico.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		hechos[i] = true
		a.Add(-1)
	})
	for i, h := range hechos {
		if !h {
			t.Fatalf("no llamó a %d", i)
		}
	}
	if p := pico.Load(); p > 3 {
		t.Fatalf("%d a la vez, tope 3", p)
	}
}

// Los nodos se publican a la vez, pero lo que se dice sale en el orden de
// sus nombres y el error es el del primero que falla en ese orden.
func TestPublicarComposeEnOrden(t *testing.T) {
	var a, pico atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/publish", func(w http.ResponseWriter, r *http.Request) {
		n := a.Add(1)
		defer a.Add(-1)
		for {
			p := pico.Load()
			if n <= p || pico.CompareAndSwap(p, n) {
				break
			}
		}
		ref := r.PathValue("ref")
		// El primero en orden es el que más tarda: si saliera según acaban,
		// iría el último.
		if ref == "dev-a" {
			time.Sleep(40 * time.Millisecond)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		if ref == "dev-d" || ref == "dev-c" {
			http.Error(w, "port taken", http.StatusConflict)
			return
		}
		var req api.PublishRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(api.Publication{Machine: ref, Ports: req.Ports})
	})
	c := fakeDaemon(t, mux)
	tr := &traduccionCompose{Proyect: "dev", Publica: map[string][]api.PortPublish{
		"a": {{Host: 8001, Guest: 80}},
		"b": {{Host: 8002, Guest: 80}},
		"c": {{Host: 8003, Guest: 80}},
		"d": {{Host: 8004, Guest: 80}},
	}}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = pw
	err = publicarCompose(context.Background(), c, tr)
	os.Stderr = stderr
	pw.Close()
	salida, _ := io.ReadAll(pr)
	if err == nil || !strings.HasPrefix(err.Error(), "c: ") {
		t.Fatalf("err = %v, want el de c", err)
	}
	ia, ib := strings.Index(string(salida), "8001"), strings.Index(string(salida), "8002")
	if ia < 0 || ib < 0 || ia > ib || strings.Contains(string(salida), "8004") {
		t.Fatalf("salida fuera de orden:\n%s", salida)
	}
	if pico.Load() < 2 {
		t.Errorf("publicó de uno en uno (pico %d)", pico.Load())
	}
}

// verProyecto: un grafo que no existe no es error; uno que existe trae sus
// máquinas, y sinGrafo quita las del grafo.
func TestVerProyecto(t *testing.T) {
	var existe atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !existe.Load() {
			http.Error(w, "no such graph", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(api.Graph{ID: "g1", Nodes: map[string]api.GraphNode{"db": {MachineID: "m1"}}})
	})
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.Machine{
			{ID: "m1", Name: "dev-db", Labels: map[string]string{api.LabelGraph: "g1", labelComposeProject: "dev"}},
			{ID: "m2", Name: "dev-app-1a2b3c-prep", Labels: map[string]string{labelComposeProject: "dev"}},
		})
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Info{Version: "v-test"})
	})
	c := fakeDaemon(t, mux)
	v, err := verProyecto(context.Background(), c, "dev", true)
	if err != nil || v.g != nil || v.info == nil || v.info.Version != "v-test" {
		t.Fatalf("sin grafo: %+v, %v", v, err)
	}
	if err := v.dueno("dev", "/x/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	existe.Store(true)
	v, err = verProyecto(context.Background(), c, "dev", false)
	if err != nil || v.g == nil || len(v.ms) != 2 || v.info != nil {
		t.Fatalf("con grafo: %+v, %v", v, err)
	}
	if q := v.sinGrafo(v.g); len(q) != 1 || q[0].ID != "m2" {
		t.Fatalf("sinGrafo = %v", q)
	}
}
