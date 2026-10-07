package machine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cuotasDePrueba: a tiene topes; b, ninguno.
func cuotasDePrueba(c Cuota) func(string) (Cuota, bool) {
	return func(owner string) (Cuota, bool) {
		if owner == "a" {
			return c, true
		}
		return Cuota{}, false
	}
}

func maquinaDe(owner string, st api.State, mem int) *api.Machine {
	id := newID()
	return &api.Machine{ID: id, Name: id, State: st, MemMiB: mem, CreatedAt: time.Now(),
		Labels: map[string]string{api.LabelOwner: owner}}
}

// Qué cuenta y qué no: las paradas y fallidas no son máquinas ni memoria,
// pero su disco sigue ahí; una congelada cuenta entera; la memoria es la del
// techo si lo tiene, y el disco el lógico (512 si no se dijo).
func TestCuotaInquilinoQueCuenta(t *testing.T) {
	casos := []struct {
		mc   api.Machine
		want UsoCuota
	}{
		{api.Machine{State: api.StateRunning, MemMiB: 256}, UsoCuota{1, 256, defaultOverlayMiB}},
		{api.Machine{State: api.StateWarm, MemMiB: 256, DiskMiB: 2048}, UsoCuota{1, 256, 2048}},
		{api.Machine{State: api.StatePaused, MemMiB: 256, MemMaxMiB: 1024}, UsoCuota{1, 1024, defaultOverlayMiB}},
		{api.Machine{State: api.StateCreated, MemMiB: 128}, UsoCuota{1, 128, defaultOverlayMiB}},
		{api.Machine{State: api.StateStopped, MemMiB: 256, DiskMiB: 100}, UsoCuota{0, 0, 100}},
		{api.Machine{State: api.StateFailed, MemMiB: 256}, UsoCuota{0, 0, defaultOverlayMiB}},
	}
	for _, c := range casos {
		if got := usoDe(&c.mc); got != c.want {
			t.Errorf("%s: %+v, quería %+v", c.mc.State, got, c.want)
		}
	}
}

func TestCuotaInquilinoExcede(t *testing.T) {
	c := Cuota{Maquinas: 2, MemMiB: 1024, DiscoMiB: SinTope}
	casos := []struct {
		nombre      string
		lleva, pide UsoCuota
		contiene    string // "" = cabe
	}{
		{"cabe justo", UsoCuota{1, 512, 0}, UsoCuota{1, 512, 0}, ""},
		{"una máquina de más", UsoCuota{2, 0, 0}, UsoCuota{1, 0, 0}, "max_machines is 2 and it has 2 machine(s)"},
		{"memoria de más", UsoCuota{1, 768, 0}, UsoCuota{1, 512, 0}, "max_mem_mib is 1024 and its machines that aren't stopped or failed take 768 MiB; this needs 512 more"},
		{"disco sin tope", UsoCuota{0, 0, 1 << 30}, UsoCuota{0, 0, 1 << 20}, ""},
		// Por encima (se bajó el tope) pero sin pedir más de eso: pasa.
		{"no sube", UsoCuota{5, 4096, 0}, UsoCuota{0, 0, 512}, ""},
	}
	for _, x := range casos {
		err := excedeCuota("a", c, x.lleva, x.pide)
		switch {
		case x.contiene == "" && err != nil:
			t.Errorf("%s: %v", x.nombre, err)
		case x.contiene != "" && (err == nil || !strings.Contains(err.Error(), x.contiene)):
			t.Errorf("%s: %v, quería %q", x.nombre, err, x.contiene)
		case x.contiene != "" && !api.IsTenantQuota(err):
			t.Errorf("%s: %v no es un 429 de cuota", x.nombre, err)
		}
	}
	// 0 es "nada", no "sin tope".
	if err := excedeCuota("a", Cuota{Maquinas: 0, MemMiB: SinTope, DiscoMiB: SinTope}, UsoCuota{}, UsoCuota{Maquinas: 1}); !api.IsTenantQuota(err) {
		t.Errorf("max_machines 0: %v", err)
	}
}

// La carrera que importa: muchos runs del mismo inquilino a la vez. Contar y
// publicar son la misma sección crítica, así que pasan exactamente los que
// caben, ni uno más; y los de otro inquilino no se enteran.
func TestCuotaInquilinoPublicarConcurrente(t *testing.T) {
	m := newTestManager(t)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 3, MemMiB: SinTope, DiscoMiB: SinTope}))
	// Una parada de a: no cuenta como máquina.
	parada := maquinaDe("a", api.StateStopped, 256)
	if err := m.publicar(parada); err != nil {
		t.Fatal(err)
	}

	const n = 32
	var bien, cuota, otro atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := "a"
			if i%4 == 0 {
				owner = "b"
			}
			err := m.publicar(maquinaDe(owner, api.StateCreated, 256))
			switch {
			case err == nil && owner == "a":
				bien.Add(1)
			case err == nil:
				otro.Add(1)
			case api.IsTenantQuota(err):
				cuota.Add(1)
			default:
				t.Errorf("publicar: %v", err)
			}
		}()
	}
	wg.Wait()
	if bien.Load() != 3 || otro.Load() != n/4 || cuota.Load() != n-n/4-3 {
		t.Fatalf("a: %d publicadas, %d rechazadas; b: %d (quería 3, %d, %d)", bien.Load(), cuota.Load(), otro.Load(), n-n/4-3, n/4)
	}
	// Lo rechazado no quedó en byID.
	if got := len(m.List()); got != 1+3+n/4 {
		t.Fatalf("%d máquinas en byID, quería %d", got, 1+3+n/4)
	}
}

// Lo mismo con memoria y disco: la suma de lo publicado nunca pasa del tope.
func TestCuotaInquilinoPublicarConcurrenteMemoriaYDisco(t *testing.T) {
	m := newTestManager(t)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: SinTope, MemMiB: 1000, DiscoMiB: 5 * defaultOverlayMiB}))
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.publicar(maquinaDe("a", api.StateCreated, 300)); err != nil && !api.IsTenantQuota(err) {
				t.Errorf("publicar: %v", err)
			}
		}()
	}
	wg.Wait()
	// 1000/300 = 3 por memoria (el disco dejaría 5).
	if got := len(m.List()); got != 3 {
		t.Fatalf("%d publicadas, quería 3", got)
	}
}

// Start: una parada vuelve a contar al dejar de estarlo, y se decide en la
// misma sección crítica (reclamarParada). Varias a la vez: solo las que caben.
func TestCuotaInquilinoStartConcurrente(t *testing.T) {
	m := newTestManager(t)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 2, MemMiB: SinTope, DiscoMiB: SinTope}))
	viva := maquinaDe("a", api.StateRunning, 256)
	if err := m.publicar(viva); err != nil {
		t.Fatal(err)
	}
	var paradas []string
	for range 6 {
		mc := maquinaDe("a", api.StateStopped, 256)
		if err := m.publicar(mc); err != nil {
			t.Fatal(err)
		}
		paradas = append(paradas, mc.ID)
	}
	var bien atomic.Int32
	var wg sync.WaitGroup
	for _, id := range paradas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.reclamarParada(id)
			switch {
			case err == nil:
				bien.Add(1)
			case !api.IsTenantQuota(err):
				t.Errorf("reclamarParada: %v", err)
			}
		}()
	}
	wg.Wait()
	if bien.Load() != 1 {
		t.Fatalf("%d arrancadas, quería 1 (tope 2, una ya viva)", bien.Load())
	}
	// Las rechazadas siguen paradas.
	paradasAun := 0
	for _, mc := range m.List() {
		if mc.State == api.StateStopped {
			paradasAun++
		}
	}
	if paradasAun != 5 {
		t.Fatalf("%d siguen paradas, quería 5", paradasAun)
	}
	// Una que ya contaba (created sin VMM: un arranque a medias) no pide
	// nada nuevo: se puede reintentar aunque esté en el tope.
	var creada string
	for _, mc := range m.List() {
		if mc.State == api.StateCreated {
			creada = mc.ID
		}
	}
	if _, err := m.reclamarParada(creada); err != nil {
		t.Fatalf("reintentar una created: %v", err)
	}
}

// El filtro previo de un grafo: si sus nodos eager no caben, no se arranca
// ninguno.
func TestCuotaInquilinoGrafoAntesDeArrancar(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	e.m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 1, MemMiB: SinTope, DiscoMiB: SinTope}))
	g := grafoTienda(true)
	for n, nodo := range g.Nodes {
		nodo.Labels = map[string]string{api.LabelOwner: "a"}
		g.Nodes[n] = nodo
	}
	_, err := e.m.GraphUp(context.Background(), g, nil)
	if !api.IsTenantQuota(err) || !strings.Contains(err.Error(), "graph tienda") {
		t.Fatalf("up: %v", err)
	}
	if e.arrancadas.Load() != 0 {
		t.Fatalf("arrancó %d nodos antes de decir que no caben", e.arrancadas.Load())
	}
	// Sin cuota para su dueño, el mismo grafo arranca.
	e.m.SetCuotas(nil)
	if _, err := e.m.GraphUp(context.Background(), g, nil); err != nil {
		t.Fatal(err)
	}
}

// El filtro previo de Run: lo que ya no cabe se dice antes de reservar nada.
func TestCuotaInquilinoComprobar(t *testing.T) {
	m := newTestManager(t)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 1, MemMiB: SinTope, DiscoMiB: SinTope}))
	if err := m.comprobarCuota("a", UsoCuota{Maquinas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.publicar(maquinaDe("a", api.StateRunning, 256)); err != nil {
		t.Fatal(err)
	}
	if err := m.comprobarCuota("a", UsoCuota{Maquinas: 1}); !api.IsTenantQuota(err) {
		t.Fatalf("a en el tope: %v", err)
	}
	for _, owner := range []string{"b", ""} {
		if err := m.comprobarCuota(owner, UsoCuota{Maquinas: 100}); err != nil {
			t.Fatalf("%q sin cuota: %v", owner, err)
		}
	}
	err := m.comprobarCuota("a", UsoCuota{Maquinas: 1})
	if want := fmt.Sprintf("tenant %q quota exceeded", "a"); !strings.Contains(err.Error(), want) {
		t.Fatalf("mensaje: %v", err)
	}
}

// El filtro previo de Start no cuenta a la máquina que se arranca: una created
// a medias (ya cuenta) se puede reintentar en el tope, como en reclamarParada.
func TestCuotaInquilinoFiltroPrevioExcluye(t *testing.T) {
	m := newTestManager(t)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 1, MemMiB: SinTope, DiscoMiB: SinTope}))
	creada := maquinaDe("a", api.StateCreated, 256)
	if err := m.publicar(creada); err != nil {
		t.Fatal(err)
	}
	pide := UsoCuota{Maquinas: 1, MemMiB: 256}
	if err := m.comprobarCuota("a", pide); !api.IsTenantQuota(err) {
		t.Fatalf("contándola: %v", err)
	}
	if err := m.comprobarCuotaSin("a", pide, creada.ID); err != nil {
		t.Fatalf("sin contarla: %v", err)
	}
}
