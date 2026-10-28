package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// altaFalsa da de alta en byID una copia del fondo ya lista, sin VMM.
func altaFalsa(m *Manager, contador *atomic.Int32) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		n := contador.Add(1)
		id := fmt.Sprintf("%016x", n)
		m.mu.Lock()
		m.byID[id] = &api.Machine{ID: id, Name: "pool-" + id[10:], From: name, State: api.StateRunning,
			CreatedAt: time.Now(), Labels: map[string]string{api.LabelPool: name, api.LabelPoolReady: "1"}}
		m.mu.Unlock()
		return id, nil
	}
}

func esperar(t *testing.T, que string, ok func() bool) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(fin) {
			t.Fatalf("no llegó a cumplirse: %s", que)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fondoDePrueba(t *testing.T, size int) (*Manager, *atomic.Int32) {
	t.Helper()
	m := newTestManager(t)
	m.bus = events.New()
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	var n atomic.Int32
	m.altaFondo = altaFalsa(m, &n)
	// Antes de que TempDir se limpie, ninguna reposición puede seguir escribiendo.
	t.Cleanup(func() {
		m.pararFondos()
		m.fondoMu.Lock()
		var hechos []chan struct{}
		for _, f := range m.fondos {
			if f.hecho != nil {
				hechos = append(hechos, f.hecho)
			}
		}
		m.fondoMu.Unlock()
		for _, h := range hechos {
			<-h
		}
	})
	if _, err := m.SetFondo("dorado", size); err != nil {
		t.Fatal(err)
	}
	esperar(t, "fondo lleno", func() bool { return m.InfoFondo("dorado").Ready == size })
	return m, &n
}

func TestFondoRellenaYReclama(t *testing.T) {
	m, creadas := fondoDePrueba(t, 2)

	req := api.RunRequest{From: "dorado", Name: "mia", TTLSeconds: 60, Labels: map[string]string{"a": "b"}}
	mc, ok := m.reclamarFondo(context.Background(), req)
	if !ok {
		t.Fatal("el fondo tiene copias y no reclamó")
	}
	if mc.Name != "mia" || mc.TTLSeconds != 60 || mc.TTLAt == nil || mc.Labels["a"] != "b" {
		t.Fatalf("copia mal adjudicada: %+v", mc)
	}
	if _, ok := mc.Labels[api.LabelPool]; ok {
		t.Fatalf("la copia reclamada conserva la etiqueta del fondo: %v", mc.Labels)
	}
	if got, _ := m.Get("mia"); got == nil || got.ID != mc.ID {
		t.Fatal("la copia no se encuentra por su nombre nuevo")
	}
	// Se repone en segundo plano hasta el tamaño pedido.
	esperar(t, "fondo repuesto", func() bool { return m.InfoFondo("dorado").Ready == 2 })
	if n := creadas.Load(); n != 3 {
		t.Fatalf("copias creadas = %d, quería 3 (2 + la reposición)", n)
	}
	// Sin nombre pedido, la copia lo recibe como una nueva.
	mc2, ok := m.reclamarFondo(context.Background(), api.RunRequest{From: "dorado"})
	if !ok || mc2.Name == "" || mc2.Name == mc.Name {
		t.Fatalf("nombre por defecto: %+v %v", mc2, ok)
	}
}

func TestFondoCarreraDeReclamaciones(t *testing.T) {
	m, _ := fondoDePrueba(t, 1)
	// Sin reposición: la segunda reclamación no puede llevarse la misma copia.
	bloqueo := make(chan struct{})
	m.altaFondo = func(ctx context.Context, name string) (string, error) {
		select {
		case <-bloqueo:
		case <-ctx.Done():
		}
		return "", ctx.Err()
	}
	var wg sync.WaitGroup
	var ganadas atomic.Int32
	ids := sync.Map{}
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mc, ok := m.reclamarFondo(context.Background(), api.RunRequest{From: "dorado", Name: fmt.Sprintf("m%d", i)}); ok {
				ganadas.Add(1)
				if _, dup := ids.LoadOrStore(mc.ID, true); dup {
					t.Errorf("dos reclamaciones se llevaron %s", mc.ID)
				}
			}
		}()
	}
	wg.Wait()
	close(bloqueo)
	if ganadas.Load() != 1 {
		t.Fatalf("reclamaciones ganadas = %d, quería exactamente 1", ganadas.Load())
	}
}

func TestFondoVaciarYApagar(t *testing.T) {
	m, _ := fondoDePrueba(t, 3)
	if _, err := m.SetFondo("dorado", 0); err != nil {
		t.Fatal(err)
	}
	if got := m.InfoFondo("dorado"); got.Size != 0 || got.Ready != 0 {
		t.Fatalf("tras apagarlo: %+v", got)
	}
	// rm es asíncrono: las copias quedan como tumbas hasta que acaba la limpieza.
	if !m.EsperarBorrados(5 * time.Second) {
		t.Fatal("la limpieza de las copias no acabó")
	}
	for _, mc := range m.List() {
		if mc.Labels[api.LabelPool] != "" {
			t.Fatalf("queda una copia del fondo: %s", mc.Name)
		}
	}
	if _, ok := m.reclamarFondo(context.Background(), api.RunRequest{From: "dorado"}); ok {
		t.Fatal("un fondo apagado reclamó")
	}
}

func TestFondoBorrarPlantillaLoVacia(t *testing.T) {
	m, _ := fondoDePrueba(t, 2)
	if err := m.RemoveSnapshot("dorado"); err != nil {
		t.Fatalf("borrar la plantilla con fondo: %v", err)
	}
	if !m.EsperarBorrados(5 * time.Second) {
		t.Fatal("la limpieza de las copias no acabó")
	}
	if len(m.List()) != 0 {
		t.Fatalf("quedan máquinas: %d", len(m.List()))
	}
	if m.InfoFondo("dorado").Size != 0 {
		t.Fatal("el tamaño del fondo sobrevive a la plantilla")
	}
}

func TestFondoNoReclamaLoQueSeDecideAlRestaurar(t *testing.T) {
	m, _ := fondoDePrueba(t, 1)
	for nombre, req := range map[string]api.RunRequest{
		"cpupct": {From: "dorado", CPUPct: 50},
		"vol":    {From: "dorado", Volume: "v"},
		"egress": {From: "dorado", Egress: "internet"},
		"owner":  {From: "dorado", Labels: map[string]string{api.LabelOwner: "x"}},
		"exec":   {From: "dorado", AllowExec: true}, // el dorado no la tiene
	} {
		if _, ok := m.reclamarFondo(context.Background(), req); ok {
			t.Errorf("%s: reclamó del fondo una petición que no cabe en la copia", nombre)
		}
	}
	if m.InfoFondo("dorado").Ready != 1 {
		t.Fatal("una petición no elegible gastó una copia")
	}
}

// El CLI manda siempre VCPUs y MemMiB, y con -from no significan nada: no
// deben bastar para saltarse el fondo.
func TestFondoReclamaConLosDefaultsDelCLI(t *testing.T) {
	m, _ := fondoDePrueba(t, 1)
	req := api.RunRequest{From: "dorado", Name: "x", VCPUs: 1, MemMiB: 256, Egress: "none", TTLSeconds: 0}
	if _, ok := m.reclamarFondo(context.Background(), req); !ok {
		t.Fatal("la petición típica de kling run -from no reclamó")
	}
}

func TestFondoTamanoFueraDeRango(t *testing.T) {
	m := newTestManager(t)
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	for _, n := range []int{-1, api.PoolMax + 1} {
		if _, err := m.SetFondo("dorado", n); err == nil {
			t.Errorf("tamaño %d aceptado", n)
		}
	}
	if _, err := m.SetFondo("nada", 1); err == nil {
		t.Error("fondo de una plantilla que no existe")
	}
}

// Un dorado de otro VMM (vz → hv) deja el fondo vacío: InfoFondo y
// ProblemasFondos lo dicen con la orden para rehacerlo, en vez de solo el log.
// Un fallo de otra clase sale como Error; reponer bien lo limpia.
func TestFondoDoradoObsoletoSeInforma(t *testing.T) {
	m, _ := fondoDePrueba(t, 1)
	escribirSnapshot(t, m, "viejo", api.Snapshot{VMM: "kling-vz 0.18.0"})
	m.FijarOrigen("0.19.0", "kling-hv 0.19.0")
	m.altaFondo = func(ctx context.Context, name string) (string, error) {
		if name == "viejo" {
			return "", errObsoleto(name, causaObsoleto("kling-vz 0.18.0", "kling-hv 0.19.0"))
		}
		return "", errors.New("no space left")
	}
	if _, err := m.SetFondo("viejo", 2); err != nil {
		t.Fatal(err)
	}
	esperar(t, "fondo obsoleto", func() bool { return m.InfoFondo("viejo").Stale != "" })
	p := m.InfoFondo("viejo")
	if !strings.Contains(p.Stale, "kling-vz 0.18.0") || !strings.Contains(p.Fix, "kling save -replace <machine> viejo") || p.Error != "" {
		t.Fatalf("pool obsoleto = %+v", p)
	}
	if _, err := m.SetFondo("dorado", 2); err != nil {
		t.Fatal(err)
	}
	esperar(t, "fallo del otro", func() bool { return m.InfoFondo("dorado").Error != "" })
	ps := m.ProblemasFondos()
	if len(ps) != 2 || ps[0].Template != "dorado" || ps[0].Error != "no space left" || ps[0].Stale != "" || ps[1].Template != "viejo" {
		t.Fatalf("ProblemasFondos = %+v", ps)
	}
	// Apagado (tamaño 0) no es un problema.
	if _, err := m.SetFondo("viejo", 0); err != nil {
		t.Fatal(err)
	}
	if ps := m.ProblemasFondos(); len(ps) != 1 || ps[0].Template != "dorado" {
		t.Fatalf("con viejo apagado: %+v", ps)
	}
}
