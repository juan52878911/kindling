package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Una ráfaga de M sesiones simultáneas contra un servicio con K sesiones por
// instancia y un arranque lento: nacen exactamente ceil(M/K) instancias (con
// tope) y ninguna sesión se rechaza mientras M <= tope*K. Antes cada sesión
// que llegaba con las réplicas aún arrancando creaba la suya: 16 réplicas para
// 50 sesiones de 13 por instancia, y 503 para las que ya no cabían (#318).
func TestRafagaEsperaALaReplicaQueNace(t *testing.T) {
	casos := []struct{ m, k, tope int }{
		{50, 13, 16}, // la celda norep N=1 M=50 de la matriz de thaw a escala
		{64, 4, 16},  // justo en el tope: 16 instancias llenas
		{7, 1, 8},    // una sesión por instancia
		{30, 32, 0},  // cabe todo en la primaria; sin tope
		{100, 3, 0},  // sin tope: ceil(100/3)
	}
	for _, c := range casos {
		t.Run(fmt.Sprintf("M%d_K%d_tope%d", c.m, c.k, c.tope), func(t *testing.T) {
			var creadas atomic.Int64
			g := &Scheduler{
				services:    map[string]*entry{},
				extra:       map[string][]*entry{},
				routes:      map[string]*sessionRoute{},
				MaxReplicas: c.tope,
			}
			g.services["svc"] = &entry{machineID: "m-0", maxSessions: c.k, checkedAt: time.Now(), lastUse: time.Now()}
			g.buildFn = func(context.Context, string, *tenant, bool) (*entry, error) {
				n := creadas.Add(1)
				time.Sleep(50 * time.Millisecond) // arranque lento: la ventana de la ráfaga
				return &entry{machineID: fmt.Sprintf("m-%d", n), maxSessions: c.k, lastUse: time.Now()}, nil
			}

			var wg sync.WaitGroup
			errs := make(chan error, c.m)
			for i := 0; i < c.m; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					e, err := g.pickInstance(context.Background(), "svc", tenantFrom(context.Background()))
					if err != nil {
						errs <- err
						return
					}
					// Como el gateway: la sesión se fija a la instancia y el
					// initialize que acaba suelta la reserva.
					g.mu.Lock()
					g.routes[fmt.Sprintf("s%d", i)] = &sessionRoute{service: "svc", machineID: e.machineID}
					g.mu.Unlock()
					g.end(e)
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("pickInstance: %v (M <= tope*K: every session fits)", err)
			}

			want := (c.m + c.k - 1) / c.k
			g.mu.Lock()
			defer g.mu.Unlock()
			entries := g.entriesLocked("svc")
			if len(entries) != want || int(creadas.Load()) != want-1 {
				t.Fatalf("%d instancias y %d réplicas creadas; quería %d y %d", len(entries), creadas.Load(), want, want-1)
			}
			for _, e := range entries {
				if n := g.sessionCountLocked(e.machineID); n > c.k {
					t.Errorf("%s tiene %d sesiones; su capacidad es %d", e.machineID, n, c.k)
				}
			}
		})
	}
}

// Si la petición que creó la réplica se cancela, las sesiones que esperaban a
// esa réplica no heredan su error: vuelven a empezar y acaban servidas.
func TestNacienteCanceladaNoArrastraALasQueEsperan(t *testing.T) {
	g := &Scheduler{
		services:    map[string]*entry{},
		extra:       map[string][]*entry{},
		routes:      map[string]*sessionRoute{},
		MaxReplicas: 4,
	}
	g.services["svc"] = &entry{machineID: "m-0", maxSessions: 4, checkedAt: time.Now(), lastUse: time.Now()}
	for i := 0; i < 4; i++ { // la primaria, llena
		g.routes[fmt.Sprintf("s%d", i)] = &sessionRoute{service: "svc", machineID: "m-0"}
	}
	var creadas atomic.Int64
	g.buildFn = func(ctx context.Context, _ string, _ *tenant, _ bool) (*entry, error) {
		n := creadas.Add(1)
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &entry{machineID: fmt.Sprintf("m-%d", n), maxSessions: 4, lastUse: time.Now()}, nil
	}
	pendientes := func() int {
		g.mu.Lock()
		defer g.mu.Unlock()
		if l := g.nacientes["svc"]; len(l) > 0 {
			return l[0].pendientes
		}
		return 0
	}
	esperarA := func(p int) {
		plazo := time.Now().Add(5 * time.Second)
		for pendientes() != p {
			if time.Now().After(plazo) {
				t.Fatalf("la naciente no llegó a %d pendientes", p)
			}
			time.Sleep(time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	creadora := make(chan error, 1)
	go func() {
		_, err := g.pickInstance(ctx, "svc", tenantFrom(context.Background()))
		creadora <- err
	}()
	esperarA(1)
	espera := make(chan error, 1)
	go func() {
		_, err := g.pickInstance(context.Background(), "svc", tenantFrom(context.Background()))
		espera <- err
	}()
	esperarA(2)
	cancel()
	if err := <-creadora; !errors.Is(err, context.Canceled) {
		t.Fatalf("la creadora: err = %v; quería context.Canceled", err)
	}
	if err := <-espera; err != nil {
		t.Fatalf("la que esperaba heredó el fallo: %v", err)
	}
}
