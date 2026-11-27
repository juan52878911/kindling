package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/chispa"
	"github.com/juan52878911/kindling/pkg/api"
)

// daemonModelos es un daemon de mentira con lo justo del API para hacer
// dorados y moverlos (run, guest, squeeze, commit, rm, ls, snapshots), sin
// microVMs. Lo usan los tests de chispa deploy, model deploy y model roll.
type daemonModelos struct {
	mu       sync.Mutex
	llamadas []string
	runs     []api.RunRequest
	commits  []api.CommitRequest
	borradas []string
	snaps    []*api.Snapshot
	maquinas []*api.Machine
	// estado es el código que contesta cada ruta del invitado (0 = 200).
	estado map[string]int
	// tras cambia el estado de una máquina cuando se la consulta n veces
	// (simula al gateway congelando una réplica ociosa).
	congelarTras map[string]int
	// warmCod es lo que contesta POST /snapshots/{name}/warm (0 = 200; 404
	// es un daemon anterior, 501 uno que no sabe precalentar);
	// calentados, los dorados que se pidió precalentar.
	warmCod    int
	calentados []string
}

func (d *daemonModelos) cliente(t *testing.T) *api.Client {
	t.Helper()
	m := http.NewServeMux()
	m.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		var rr api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&rr)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.runs = append(d.runs, rr)
		id := fmt.Sprintf("m%d", len(d.runs))
		mc := &api.Machine{ID: id, Name: rr.Name, State: api.StateRunning, BootMS: 5, Labels: rr.Labels, VCPUs: rr.VCPUs, MemMiB: rr.MemMiB}
		d.maquinas = append(d.maquinas, mc)
		_ = json.NewEncoder(w).Encode(mc)
	})
	m.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, mc := range d.maquinas {
			if n, ok := d.congelarTras[mc.ID]; ok {
				if n <= 0 {
					mc.State = api.StateWarm
				}
				d.congelarTras[mc.ID] = n - 1
			}
		}
		_ = json.NewEncoder(w).Encode(d.maquinas)
	})
	m.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		var g api.GuestRequest
		_ = json.NewDecoder(r.Body).Decode(&g)
		d.mu.Lock()
		defer d.mu.Unlock()
		if g.ProbeOnly {
			d.llamadas = append(d.llamadas, fmt.Sprintf("guest probe :%d", g.Port))
			_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: 200})
			return
		}
		d.llamadas = append(d.llamadas, fmt.Sprintf("guest %s %s :%d", g.Method, g.Path, g.Port))
		st := d.estado[g.Path]
		if st == 0 {
			st = 200
		}
		_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: st, Body: `{"label":"x","confidence":0.9}`})
	})
	m.HandleFunc("POST /machines/{ref}/squeeze", func(w http.ResponseWriter, r *http.Request) {
		d.apunta("squeeze")
		_ = json.NewEncoder(w).Encode(api.SqueezeResult{ReclaimedMiB: 1})
	})
	m.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		var cr api.CommitRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.llamadas = append(d.llamadas, "commit "+cr.Name)
		d.commits = append(d.commits, cr)
		var labels map[string]string
		for _, mc := range d.maquinas {
			if mc.ID == r.PathValue("ref") {
				labels = mc.Labels
			}
		}
		s := &api.Snapshot{Name: cr.Name, MemBytes: 1 << 20, Labels: labels}
		d.snaps = append(d.snaps, s)
		_ = json.NewEncoder(w).Encode(s)
	})
	m.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ref := r.PathValue("ref")
		d.llamadas = append(d.llamadas, "rm "+ref)
		d.borradas = append(d.borradas, ref)
		var quedan []*api.Machine
		for _, mc := range d.maquinas {
			if mc.ID != ref {
				quedan = append(quedan, mc)
			}
		}
		d.maquinas = quedan
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST /snapshots/{name}/warm", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.llamadas = append(d.llamadas, "warm "+r.PathValue("name"))
		if d.warmCod != 0 && d.warmCod != 200 {
			http.Error(w, `{"error":"no"}`, d.warmCod)
			return
		}
		d.calentados = append(d.calentados, r.PathValue("name"))
		_ = json.NewEncoder(w).Encode(api.WarmSnapshotResult{Bytes: 1 << 20, Started: true})
	})
	m.HandleFunc("DELETE /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		name := r.PathValue("name")
		d.llamadas = append(d.llamadas, "rm-snapshot "+name)
		var quedan []*api.Snapshot
		for _, s := range d.snaps {
			if s.Name != name {
				quedan = append(quedan, s)
			}
		}
		d.snaps = quedan
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(d.snaps)
	})
	return fakeDaemon(t, m)
}

func (d *daemonModelos) apunta(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.llamadas = append(d.llamadas, s)
}

// R8-10: el dorado de Chispa graba cpu_pct = 100 por vCPU, como el de VON. Sin
// él heredaba el 50 % del daemon y sus réplicas, la mitad de un núcleo.
func TestMakeFijaCPUPct(t *testing.T) {
	d := &daemonModelos{}
	c := d.cliente(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := makeChispaGolden(ctx, c, chispaGoldenOptions{
		Image: "tickets", Snapshot: "tickets", VCPUs: 2, MemMiB: 128, Wait: 2 * time.Second, WarmText: "hola",
	})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runs[0]
	if r.CPUPct != 200 || r.Egress != "none" || r.Labels[api.LabelService] != "tickets" ||
		r.Labels[chispaLabelTask] != "tickets" || r.Labels[api.LabelPorts] != fmt.Sprint(chispa.GuestPort) {
		t.Fatalf("run: %+v", r)
	}
	if g.Snapshot.Name != "tickets" || g.BootMS != 5 {
		t.Fatalf("resultado: %+v", g)
	}
	// El mismo flujo que antes: sonda, /healthz, una clasificación, apretar,
	// congelar y borrar la plantilla.
	p := chispa.GuestPort
	quiero := []string{
		fmt.Sprintf("guest probe :%d", p),
		fmt.Sprintf("guest GET /healthz :%d", p),
		fmt.Sprintf("guest POST /v1/classify :%d", p),
		"squeeze", "commit tickets", "rm m1",
	}
	if strings.Join(d.llamadas, "|") != strings.Join(quiero, "|") {
		t.Fatalf("llamadas:\n%s\nquería:\n%s", strings.Join(d.llamadas, "\n"), strings.Join(quiero, "\n"))
	}
}

// Un calentamiento que no contesta 200 no deja dorado, y la plantilla se borra.
func TestChispaGoldenWarmupFalla(t *testing.T) {
	d := &daemonModelos{estado: map[string]int{"/v1/classify": 500}}
	c := d.cliente(t)
	_, err := makeChispaGolden(context.Background(), c, chispaGoldenOptions{
		Image: "tickets", Snapshot: "tickets", VCPUs: 1, MemMiB: 128, Wait: 2 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "warm-up") {
		t.Fatalf("err = %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.commits) != 0 || len(d.borradas) != 1 {
		t.Fatalf("commits %v, borradas %v", d.commits, d.borradas)
	}
}
