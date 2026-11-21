//go:build !darwin

package net

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// procesos sustituye todo lo que lanza procesos y apunta cada uno.
type procesos struct {
	mu  sync.Mutex
	log []string
}

func (p *procesos) add(s string) { p.mu.Lock(); p.log = append(p.log, s); p.mu.Unlock() }
func (p *procesos) n() int       { p.mu.Lock(); defer p.mu.Unlock(); return len(p.log) }
func (p *procesos) vaciar()      { p.mu.Lock(); p.log = nil; p.mu.Unlock() }

func sustituirProcesos(t *testing.T) *procesos {
	t.Helper()
	oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA := ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost
	t.Cleanup(func() {
		ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost = oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA
	})
	p := &procesos{}
	ordenEnNetns = func(ns string, a ...string) error { p.add("en-netns " + strings.Join(a, " ")); return nil }
	ejecutar = func(a ...string) error { p.add(strings.Join(a, " ")); return nil }
	ipBatch = func(ns string, _ ...string) error { p.add("ip-batch " + ns); return nil }
	ejecutarRestore = func(ns, _ string) ([]byte, error) { p.add("iptables-restore " + ns); return nil, nil }
	hayRestore = func() bool { return true }
	hayIp6tables = func() bool { return true }
	existeRed = func(*Net) bool { return false }
	asegurarHost = func() error { return nil }
	return p
}

var contadorRanura atomic.Int64

// ranuraFalsa monta una ranura de verdad (con montarBase y los procesos
// sustituidos) con un id y un índice únicos.
func ranuraFalsa() (*Ranura, error) {
	i := int(contadorRanura.Add(1))
	id := fmt.Sprintf("%08x", i)
	n := Plan(i, id)
	if err := n.MontarRanura(1000); err != nil {
		return nil, err
	}
	return &Ranura{ID: id, Net: n}, nil
}

func esperarListas(t *testing.T, f *Fondo, n int) {
	t.Helper()
	for i := 0; i < 2000 && f.Len() < n; i++ {
		time.Sleep(time.Millisecond)
	}
	if f.Len() < n {
		t.Fatalf("el fondo tiene %d ranuras, quería %d", f.Len(), n)
	}
}

func TestTomarRanuraNoLanzaProcesosDeMontarBase(t *testing.T) {
	p := sustituirProcesos(t)
	r, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	f := NuevoFondo(0, ranuraFalsa, func(*Ranura) {}) // tam 0: no repone
	f.listas = []*Ranura{r}
	p.vaciar()
	if got := f.Tomar(); got != r {
		t.Fatal("no entregó la ranura lista")
	}
	if got := f.Entregar(r.ID); got != r.Net {
		t.Fatal("Entregar no devuelve la red de la ranura")
	}
	if p.n() != 0 {
		t.Fatalf("tomar una ranura lanzó procesos: %q", p.log)
	}
}

func TestAplicarPropioLanzaComoMuchoDos(t *testing.T) {
	for _, vecino := range []bool{false, true} {
		p := sustituirProcesos(t)
		r, err := ranuraFalsa()
		if err != nil {
			t.Fatal(err)
		}
		p.vaciar()
		r.Net.VecinoInvitado = vecino
		if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
			t.Fatal(err)
		}
		if p.n() > 2 {
			t.Errorf("vecino=%v: aplicarPropio lanzó %d procesos: %q", vecino, p.n(), p.log)
		}
		if vecino != strings.Contains(strings.Join(p.log, "|"), "nud stale") {
			t.Errorf("vecino=%v: %q", vecino, p.log)
		}
		for _, l := range p.log {
			if strings.Contains(l, "netns add") || strings.HasPrefix(l, "ip-batch") {
				t.Errorf("aplicarPropio repitió el montaje de la base: %q", l)
			}
		}
	}
}

func TestRanuraNoPasaDosVecesPorAplicarPropio(t *testing.T) {
	p := sustituirProcesos(t)
	r, _ := ranuraFalsa()
	if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
		t.Fatal(err)
	}
	p.vaciar()
	// Un segundo Setup del mismo *Net ya no es una ranura: monta todo de nuevo.
	if err := r.Net.Setup(EgressInternet, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.log, "|"), "netns add") {
		t.Fatalf("el segundo Setup no remontó la base: %q", p.log)
	}
}

func TestRanuraDescartadaSeDestruyeYNoVuelve(t *testing.T) {
	sustituirProcesos(t)
	var destruidas sync.Map
	f := NuevoFondo(2, ranuraFalsa, func(r *Ranura) { destruidas.Store(r.ID, true) })
	f.Reponer()
	esperarListas(t, f, 2)
	r := f.Tomar()
	f.Descartar(r.ID)
	if _, ok := destruidas.Load(r.ID); !ok {
		t.Fatal("la ranura descartada no se destruyó")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("una ranura descartada se pudo entregar")
	}
	esperarListas(t, f, 2)
	for i := 0; i < 2; i++ {
		if o := f.Tomar(); o == nil || o.ID == r.ID {
			t.Fatalf("la ranura descartada reapareció en el fondo: %v", o)
		}
	}
	f.Cerrar()
}

func TestTomasConcurrentesSinRepetidos(t *testing.T) {
	sustituirProcesos(t)
	f := NuevoFondo(16, ranuraFalsa, func(r *Ranura) {})
	f.Reponer()
	esperarListas(t, f, 16)
	var mu sync.Mutex
	ids, idx := map[string]bool{}, map[int]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.Tomar()
			if r == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ids[r.ID] || idx[r.Net.Index] {
				t.Errorf("ranura repetida: id %s índice %d", r.ID, r.Net.Index)
			}
			ids[r.ID], idx[r.Net.Index] = true, true
		}()
	}
	wg.Wait()
	f.Cerrar()
	if len(ids) < 16 {
		t.Errorf("solo %d tomas con ranura", len(ids))
	}
}

func TestRanuraTomadaYNoMontadaSeRecoge(t *testing.T) {
	sustituirProcesos(t)
	ahora := time.Now()
	var destruidas atomic.Int64
	f := NuevoFondo(1, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.ahora = func() time.Time { return ahora }
	f.Reponer()
	esperarListas(t, f, 1)
	r := f.Tomar()
	if f.Recoger() != 0 {
		t.Fatal("recogió una ranura reciente")
	}
	ahora = ahora.Add(PlazoRanuraTomada + time.Second)
	if f.Recoger() != 1 || destruidas.Load() != 1 {
		t.Fatal("no recogió la ranura tomada y no montada en 60 s")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("entregó una ranura ya recogida")
	}
	f.Cerrar()
}

func TestPropiaCubreEnEsperaYTomadas(t *testing.T) {
	sustituirProcesos(t)
	f := NuevoFondo(2, ranuraFalsa, func(*Ranura) {})
	f.Reponer()
	esperarListas(t, f, 2)
	r := f.Tomar()
	if !f.Propia(r.Net.NS) || !f.PrefijoEnUso(Corto(r.ID)) {
		t.Fatal("una ranura tomada debe seguir siendo del fondo")
	}
	if f.Propia("kl-ffffffff") {
		t.Fatal("un ns ajeno no es del fondo")
	}
	f.Cerrar()
}

func TestSetupSinRanuraConCuatroMontajesSimultaneos(t *testing.T) {
	sustituirProcesos(t)
	var enCurso, maximo atomic.Int64
	ipBatch = func(ns string, _ ...string) error {
		if ns == "" {
			v := enCurso.Add(1)
			for {
				m := maximo.Load()
				if v <= m || maximo.CompareAndSwap(m, v) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			enCurso.Add(-1)
		}
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := Plan(1000+i, fmt.Sprintf("%08x", 5000+i))
			if err := n.Setup(EgressInternet, nil, 1000); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if maximo.Load() > 4 {
		t.Fatalf("%d montajes completos a la vez; el tope es 4", maximo.Load())
	}
}

func TestRecogerConCerrojoOcupadoNoDestruye(t *testing.T) {
	sustituirProcesos(t)
	ahora := time.Now()
	var destruidas atomic.Int64
	f := NuevoFondo(1, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.ahora = func() time.Time { return ahora }
	var ocupado atomic.Bool
	ocupado.Store(true)
	var soltadas atomic.Int64
	f.Bloquear = func(string) (func(), bool) {
		if ocupado.Load() {
			return nil, false
		}
		return func() { soltadas.Add(1) }, true
	}
	f.Reponer()
	esperarListas(t, f, 1)
	r := f.Tomar()
	ahora = ahora.Add(PlazoRanuraTomada + time.Second)
	if f.Recoger() != 0 || destruidas.Load() != 0 {
		t.Fatal("destruyó una ranura cuyo id tiene el cerrojo ocupado")
	}
	ocupado.Store(false)
	if f.Recoger() != 1 || destruidas.Load() != 1 || soltadas.Load() != 1 {
		t.Fatal("a la vuelta siguiente debía destruirla y soltar el cerrojo")
	}
	if f.Entregar(r.ID) != nil {
		t.Fatal("entregó una ranura recogida")
	}
	f.Cerrar()
}

func TestCerrarDestruyeTodasYParaLaRecogida(t *testing.T) {
	sustituirProcesos(t)
	var destruidas atomic.Int64
	f := NuevoFondo(3, ranuraFalsa, func(*Ranura) { destruidas.Add(1) })
	f.IniciarRecogida(time.Millisecond)
	f.Reponer()
	esperarListas(t, f, 3)
	f.Cerrar() // espera a la goroutine de recogida: si no acaba, el test se cuelga
	if destruidas.Load() != 3 {
		t.Fatalf("destruidas = %d, quería 3", destruidas.Load())
	}
	f.Cerrar() // idempotente
}

// Una ranura entregada con none ya tiene las reglas de none montadas (S5):
// aplicarPropio no lanza ningún iptables-restore.
func TestRanuraConNoneNoLanzaRestoreEnAplicarPropio(t *testing.T) {
	p := sustituirProcesos(t)
	r, err := ranuraFalsa()
	if err != nil {
		t.Fatal(err)
	}
	viejo := iniciarResolverNone
	t.Cleanup(func() { iniciarResolverNone = viejo })
	iniciarResolverNone = func(*Net) error { return nil }
	p.vaciar()
	if err := r.Net.Setup(EgressNone, nil, 1000); err != nil {
		t.Fatal(err)
	}
	for _, l := range p.log {
		if strings.Contains(l, "iptables") {
			t.Errorf("Setup en none con ranura lanzó %q", l)
		}
	}
}
