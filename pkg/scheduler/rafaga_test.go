package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net"
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

// Las sesiones que el puente rechaza por tope (el reintento del gateway) se
// apuntan a la réplica que ya nace en vez de arrancar cada una la suya: con
// 1024 MiB el gateway estima 13 sesiones y el puente, que ve algo menos de
// memoria, admite 12, así que en una ráfaga sobra una por instancia.
func TestRechazadasPorElPuenteEsperanALaQueNace(t *testing.T) {
	g := &Scheduler{
		services:    map[string]*entry{},
		extra:       map[string][]*entry{},
		routes:      map[string]*sessionRoute{},
		MaxReplicas: 16,
	}
	g.services["svc"] = &entry{machineID: "m-0", maxSessions: 8, checkedAt: time.Now(), lastUse: time.Now()}
	var creadas atomic.Int64
	g.buildFn = func(context.Context, string, *tenant, bool) (*entry, error) {
		n := creadas.Add(1)
		time.Sleep(50 * time.Millisecond)
		return &entry{machineID: fmt.Sprintf("m-%d", n), maxSessions: 8, lastUse: time.Now()}, nil
	}
	const rechazadas = 6
	var wg sync.WaitGroup
	errs := make(chan error, rechazadas)
	for i := 0; i < rechazadas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.joinOrScaleOut(context.Background(), "svc", tenantFrom(context.Background())); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("joinOrScaleOut: %v", err)
	}
	if n := creadas.Load(); n != 1 {
		t.Fatalf("%d réplicas para %d sesiones rechazadas que caben en una; quería 1", n, rechazadas)
	}
}

// nacienteDePrueba deja un scheduler con la primaria llena (k sesiones) para
// que cada joinOrScaleOut se apunte a una naciente o cree la suya.
func nacienteDePrueba(k int) *Scheduler {
	g := &Scheduler{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	g.services["svc"] = &entry{machineID: "m-0", maxSessions: k, checkedAt: time.Now(), lastUse: time.Now()}
	for i := 0; i < k; i++ {
		g.routes[fmt.Sprintf("s%d", i)] = &sessionRoute{service: "svc", machineID: "m-0"}
	}
	return g
}

// esperarPendientes espera a que la primera naciente del servicio tenga p
// sesiones apuntadas.
func esperarPendientes(t *testing.T, g *Scheduler, p int) {
	t.Helper()
	plazo := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		n := 0
		if l := g.nacientes["svc"]; len(l) > 0 {
			n = l[0].pendientes
		}
		g.mu.Unlock()
		if n == p {
			return
		}
		if time.Now().After(plazo) {
			t.Fatalf("la naciente no llegó a %d pendientes (tiene %d)", p, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Un fallo real de la réplica (el invitado no levanta y waitReadyAddr vence con
// un timeout de red) lo heredan las que esperaban: un timeout de red también
// cumple errors.Is(err, context.DeadlineExceeded), y tomarlo por «ajeno» hacía
// reintentar a todas, con una réplica fallida nueva por ronda.
func TestNacienteConTimeoutDeRedNoReintentaEnCadena(t *testing.T) {
	vencido, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, errRed := (&net.Dialer{}).DialContext(vencido, "tcp", "127.0.0.1:1")
	var opErr *net.OpError
	if !errors.As(errRed, &opErr) || !errors.Is(errRed, context.DeadlineExceeded) {
		t.Fatalf("precondición: quería un *net.OpError que cumpla DeadlineExceeded; err = %v", errRed)
	}

	const esperando = 8
	g := nacienteDePrueba(esperando + 1)
	var builds atomic.Int64
	soltar := make(chan struct{})
	g.buildFn = func(context.Context, string, *tenant, bool) (*entry, error) {
		builds.Add(1)
		<-soltar
		return nil, fmt.Errorf("replica not ready: %w", errRed)
	}
	errs := make(chan error, esperando+1)
	pedir := func() {
		_, err := g.joinOrScaleOut(context.Background(), "svc", tenantFrom(context.Background()))
		errs <- err
	}
	go pedir()
	esperarPendientes(t, g, 1)
	for i := 0; i < esperando; i++ {
		go pedir()
	}
	esperarPendientes(t, g, esperando+1)
	close(soltar)
	for i := 0; i < esperando+1; i++ {
		if err := <-errs; !errors.Is(err, errRed) {
			t.Errorf("err = %v; quería el fallo de la réplica", err)
		}
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d réplicas creadas; quería 1 (las que esperaban heredan el fallo)", n)
	}
}

// Un pánico en build no deja la naciente registrada: las que esperaban
// reciben un error en vez de colgarse y deja de contar para el tope.
func TestNacienteConPanicoSeCierra(t *testing.T) {
	g := nacienteDePrueba(2)
	soltar := make(chan struct{})
	g.buildFn = func(context.Context, string, *tenant, bool) (*entry, error) {
		<-soltar
		panic("build roto")
	}
	panico := make(chan any, 1)
	go func() {
		defer func() { panico <- recover() }()
		_, _ = g.joinOrScaleOut(context.Background(), "svc", tenantFrom(context.Background()))
	}()
	esperarPendientes(t, g, 1)
	espera := make(chan error, 1)
	go func() {
		_, err := g.joinOrScaleOut(context.Background(), "svc", tenantFrom(context.Background()))
		espera <- err
	}()
	esperarPendientes(t, g, 2)
	close(soltar)
	if r := <-panico; r == nil {
		t.Fatal("el pánico de build no llegó a quien la creó")
	}
	select {
	case err := <-espera:
		if !errors.Is(err, errNacienteRota) {
			t.Fatalf("err = %v; quería errNacienteRota", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la que esperaba se quedó colgada en una naciente que nunca se cierra")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := len(g.nacientes["svc"]); n != 0 {
		t.Fatalf("quedan %d nacientes registradas", n)
	}
}

// La cuota de instancias del inquilino que creó la réplica no es la de los
// demás: una sesión de otro inquilino vuelve a empezar (y acaba servida), y una
// del mismo inquilino hereda el rechazo.
func TestNacienteCuotaDeOtroInquilino(t *testing.T) {
	g := nacienteDePrueba(3)
	a, b := &tenant{name: "a"}, &tenant{name: "b"}
	var builds atomic.Int64
	soltar := make(chan struct{})
	g.buildFn = func(_ context.Context, _ string, tnt *tenant, _ bool) (*entry, error) {
		n := builds.Add(1)
		<-soltar
		if tnt == a {
			return nil, fmt.Errorf("%w: %q", errTenantInstances, tnt.name)
		}
		return &entry{machineID: fmt.Sprintf("m-%d", n), maxSessions: 3, lastUse: time.Now()}, nil
	}
	type res struct {
		quien string
		err   error
	}
	out := make(chan res, 3)
	pedir := func(quien string, tnt *tenant) {
		_, err := g.joinOrScaleOut(context.Background(), "svc", tnt)
		out <- res{quien, err}
	}
	go pedir("a1", a)
	esperarPendientes(t, g, 1)
	go pedir("a2", a)
	go pedir("b", b)
	esperarPendientes(t, g, 3)
	close(soltar)
	for i := 0; i < 3; i++ {
		r := <-out
		switch r.quien {
		case "a1", "a2":
			if !errors.Is(r.err, errTenantInstances) {
				t.Errorf("%s: err = %v; quería la cuota de su inquilino", r.quien, r.err)
			}
		case "b":
			if r.err != nil {
				t.Errorf("b heredó la cuota de otro inquilino: %v", r.err)
			}
		}
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("%d réplicas creadas; quería 2 (la fallida de a y la de b)", n)
	}
}
