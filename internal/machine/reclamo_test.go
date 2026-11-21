package machine

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// managerReclamo es un Manager mínimo, sin VMM, con n máquinas running
// reclamables (etiqueta pool=ok) sembradas en byID.
func managerReclamo(t *testing.T, n int) *Manager {
	t.Helper()
	m := &Manager{byID: map[string]*api.Machine{}, socket: map[string]string{}, quit: make(chan struct{}), bus: events.New()}
	t.Cleanup(func() { close(m.quit) })
	for i := 0; i < n; i++ {
		sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	}
	return m
}

func sembrarReclamable(m *Manager, st api.State, labels map[string]string) *api.Machine {
	id := newID()
	mc := &api.Machine{ID: id, Name: id, State: st, AllowExec: true, MemMiB: 256, Labels: labels, CreatedAt: time.Now()}
	m.mu.Lock()
	m.byID[id] = mc
	m.mu.Unlock()
	return mc
}

// Hipótesis del plan (1.6): el daemon no tiene compare-and-swap, así que el
// gateway necesita dos fases y una espera de 50 ms. Con una sola sección
// crítica, 100 reclamos sobre 10 máquinas dan exactamente 10 ganadores.
func TestReclamarConcurrente(t *testing.T) {
	m := managerReclamo(t, 10)
	var wg sync.WaitGroup
	var ganadas, vacias atomic.Int32
	var mu sync.Mutex
	vistas := map[string]bool{}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mc, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"},
				Labels: map[string]string{"kling.sandbox.claim": "x"}, TTLSeconds: 60})
			switch {
			case err == nil:
				ganadas.Add(1)
				mu.Lock()
				if vistas[mc.ID] {
					t.Errorf("máquina %s entregada dos veces", mc.ID)
				}
				vistas[mc.ID] = true
				mu.Unlock()
			case errors.Is(err, ErrNoReclamable):
				vacias.Add(1)
			default:
				t.Errorf("error inesperado: %v", err)
			}
		}()
	}
	wg.Wait()
	if ganadas.Load() != 10 || vacias.Load() != 90 {
		t.Fatalf("ganadas=%d vacías=%d, quería 10 y 90", ganadas.Load(), vacias.Load())
	}
}

func TestReclamarSoloRunningSinTransicion(t *testing.T) {
	m := managerReclamo(t, 0)
	for _, st := range []api.State{api.StatePaused, api.StateWarm, api.StateFailed, api.StateStopped} {
		sembrarReclamable(m, st, map[string]string{"pool": "ok"})
	}
	enTransicion := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	m.marcarTransicion(enTransicion.ID, api.TransitionFreezing)
	sinExec := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	sinExec.AllowExec = false
	if _, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: marcaReclamo}); !errors.Is(err, ErrNoReclamable) {
		t.Fatalf("err = %v, quería ErrNoReclamable", err)
	}
}

func TestReclamarRespetaAbsentYMatch(t *testing.T) {
	m := managerReclamo(t, 0)
	ajena := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok", "otro": "1"})
	otra := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "no"})
	propia := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	mc, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: marcaReclamo, Absent: []string{"otro"}})
	if err != nil || mc.ID != propia.ID {
		t.Fatalf("reclamó %v, %v; quería %s", mc, err, propia.ID)
	}
	if ajena.Labels["otro"] != "1" || otra.Labels["pool"] != "no" {
		t.Fatal("tocó una máquina que no debía")
	}
}

func TestReclamarNoRobaDeInquilinoNiYaReclamada(t *testing.T) {
	m := managerReclamo(t, 0)
	sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok", api.LabelOwner: "a"})
	sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok", api.LabelClaim: "n"})
	sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok", api.LabelPool: "base"})
	if _, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: marcaReclamo}); !errors.Is(err, ErrNoReclamable) {
		t.Fatalf("err = %v, quería ErrNoReclamable", err)
	}
}

func TestReclamarMatchVacioYEtiquetasMalas(t *testing.T) {
	m := managerReclamo(t, 1)
	for _, req := range []api.ClaimRequest{
		{},
		{Match: map[string]string{"pool": "ok"}, Labels: map[string]string{"kling.graph": "x"}},
		{Match: map[string]string{"pool": "ok"}, OnTTL: "explota"},
		{Match: map[string]string{"pool": "ok"}, TTLSeconds: -1},
	} {
		if _, err := m.Reclamar(req); !errors.Is(err, ErrRunRequest) {
			t.Errorf("%+v: err = %v, quería ErrRunRequest", req, err)
		}
	}
}

func TestReclamarPoneEtiquetasNombreYTTL(t *testing.T) {
	m := managerReclamo(t, 1)
	mc, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Name: "sbx1",
		Labels: map[string]string{"t": "1", api.LabelClaim: "n"}, TTLSeconds: 90, OnTTL: api.OnTTLRemove})
	if err != nil {
		t.Fatal(err)
	}
	if mc.Name != "sbx1" || mc.Labels["t"] != "1" || mc.Labels["pool"] != "ok" || mc.TTLSeconds != 90 ||
		mc.OnTTL != api.OnTTLRemove || mc.TTLAt == nil {
		t.Fatalf("copia = %+v", mc)
	}
	// El nombre ocupado se rechaza y no consume la máquina.
	sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	if _, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Name: "sbx1", Labels: marcaReclamo}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, quería ErrNameTaken", err)
	}
}

func TestReclamarCuota(t *testing.T) {
	m := managerReclamo(t, 1)
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 1, MemMiB: SinTope, DiscoMiB: SinTope}))
	sembrarReclamable(m, api.StateRunning, map[string]string{api.LabelOwner: "a"})
	_, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"},
		Labels: map[string]string{api.LabelOwner: "a", "x": "1"}})
	if !api.IsTenantQuota(err) {
		t.Fatalf("err = %v, quería cuota", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mc := range m.byID {
		if mc.Labels["x"] != "" || (mc.Labels["pool"] == "ok" && mc.Labels[api.LabelOwner] != "") {
			t.Fatalf("la cuota rechazada tocó etiquetas: %+v", mc.Labels)
		}
	}
}

// H4: Reclamar respeta el cerrojo de ciclo de vida. Una candidata con el
// cerrojo tomado (un freeze o un remove por TTL decidiendo) se salta, sin
// esperar; si no hay otra, no_claimable.
func TestReclamarSaltaCandidataConCerrojo(t *testing.T) {
	m := managerReclamo(t, 0)
	a := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	b := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	suelta, ok := m.tryLock(a.ID)
	if !ok {
		t.Fatal("no tomó el cerrojo de a")
	}
	for i := 0; i < 20; i++ { // el orden del mapa es aleatorio
		m.mu.Lock()
		b.Labels = map[string]string{"pool": "ok"}
		b.TTLAt = nil
		m.mu.Unlock()
		mc, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: marcaReclamo})
		if err != nil || mc.ID != b.ID {
			t.Fatalf("reclamó %v, %v; quería %s", mc, err, b.ID)
		}
	}
	suelta()

	m = managerReclamo(t, 0)
	solo := sembrarReclamable(m, api.StateRunning, map[string]string{"pool": "ok"})
	suelta, _ = m.tryLock(solo.ID)
	defer suelta()
	done := make(chan error, 1)
	go func() {
		_, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: marcaReclamo})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoReclamable) {
			t.Fatalf("err = %v, quería ErrNoReclamable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reclamar esperó al cerrojo")
	}
}

func TestReclamarValidaNombreYTTL(t *testing.T) {
	m := managerReclamo(t, 1)
	for _, req := range []api.ClaimRequest{
		{Match: map[string]string{"pool": "ok"}, TTLSeconds: api.SandboxMaxTTL + 1},
		{Match: map[string]string{"pool": "ok"}, Name: "../malo"},
		{Match: map[string]string{"pool": "ok"}, Name: "con espacio"},
	} {
		if _, err := m.Reclamar(req); !errors.Is(err, ErrRunRequest) {
			t.Errorf("%+v: err = %v, quería ErrRunRequest", req, err)
		}
	}
}

// Un reclamo que no deja marca (ni claim ni owner) permitiría reclamar dos
// veces la misma máquina: 400, y la máquina queda intacta.
func TestReclamarExigeMarca(t *testing.T) {
	m := managerReclamo(t, 1)
	for _, labels := range []map[string]string{nil, {}, {"otra": "cosa"}} {
		_, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: labels})
		if !errors.Is(err, ErrRunRequest) {
			t.Errorf("labels %v: err = %v, quería ErrRunRequest", labels, err)
		}
	}
	for _, k := range []string{api.LabelClaim, api.LabelOwner} {
		m := managerReclamo(t, 1)
		if _, err := m.Reclamar(api.ClaimRequest{Match: map[string]string{"pool": "ok"}, Labels: map[string]string{k: "x"}}); err != nil {
			t.Errorf("con %s: %v", k, err)
		}
	}
}

var marcaReclamo = map[string]string{api.LabelClaim: "n"}
