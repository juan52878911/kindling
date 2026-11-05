package machine

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// restaurarCopiasFork: como mucho aLaVez a la vez, las copias en el orden de
// req, y cada una pasa por lista.
func TestRestaurarCopiasForkEnParalelo(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}}
	var dentro, maximo atomic.Int32
	antes := restaurarFork
	t.Cleanup(func() { restaurarFork = antes })
	restaurarFork = func(ctx context.Context, _ *Manager, req api.RunRequest) (*api.Machine, error) {
		d := dentro.Add(1)
		for {
			v := maximo.Load()
			if d <= v || maximo.CompareAndSwap(v, d) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		dentro.Add(-1)
		return &api.Machine{ID: "id-" + req.Name, Name: req.Name}, nil
	}
	req := func(i int) api.RunRequest { return api.RunRequest{Name: "copia-" + strconv.Itoa(i)} }
	var listas atomic.Int32
	out, err := m.restaurarCopiasFork(context.Background(), "caja", 7, 3, req,
		func(context.Context, *api.Machine) error { listas.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 7 || listas.Load() != 7 {
		t.Fatalf("%d copias, %d por lista", len(out), listas.Load())
	}
	for i, mc := range out {
		if mc == nil || mc.Name != "copia-"+strconv.Itoa(i) {
			t.Fatalf("copia %d = %+v: fuera de orden", i, mc)
		}
	}
	if v := maximo.Load(); v > 3 || v < 2 {
		t.Fatalf("%d a la vez, quiero como mucho 3 (y más de una)", v)
	}
}

// El primer fallo cancela las que faltan, espera a todas y es el que se
// devuelve, con el número de su copia.
func TestRestaurarCopiasForkFalloCancelaElResto(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}}
	var lanzadas atomic.Int32
	var vivas sync.WaitGroup
	antes := restaurarFork
	t.Cleanup(func() { restaurarFork = antes })
	restaurarFork = func(ctx context.Context, _ *Manager, req api.RunRequest) (*api.Machine, error) {
		lanzadas.Add(1)
		vivas.Add(1)
		defer vivas.Done()
		if req.Name == "copia-1" {
			return nil, errors.New("restore failed: no KVM here")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return &api.Machine{Name: req.Name}, nil
		}
	}
	req := func(i int) api.RunRequest { return api.RunRequest{Name: "copia-" + strconv.Itoa(i)} }
	t0 := time.Now()
	out, err := m.restaurarCopiasFork(context.Background(), "caja", 10, 2, req, nil)
	if err == nil || out != nil || !strings.Contains(err.Error(), "fork 2 of 10 from caja") ||
		!strings.Contains(err.Error(), "no KVM") {
		t.Fatalf("= %d copias, %v", len(out), err)
	}
	if time.Since(t0) > time.Second {
		t.Fatal("el fallo no canceló a las demás")
	}
	if n := lanzadas.Load(); n >= 10 {
		t.Fatalf("se lanzaron %d tras el fallo", n)
	}
	vivas.Wait() // ninguna sigue corriendo: ya volvieron todas
}
