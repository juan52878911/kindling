package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// El token con nombre resuelve a su tenant y lo cuelga del contexto; el token
// único sigue siendo el tenant "default". Un token que no coincide, 401.
func TestAuthResuelveTenant(t *testing.T) {
	g := &Scheduler{}
	g.SetTenants([]TenantLimit{
		{Name: "equipo-a", Token: "tok-a", MaxInflight: 2},
		{Name: "equipo-b", Token: "tok-b", MaxInstances: 1},
	})

	// El handler protegido devuelve el nombre del tenant resuelto.
	quien := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tenantFrom(r.Context()).name))
	})
	h := g.authHandler(quien, "tok-default")

	casos := []struct {
		header     string
		wantCode   int
		wantTenant string
	}{
		{"Bearer tok-default", http.StatusOK, defaultTenant},
		{"Bearer tok-a", http.StatusOK, "equipo-a"},
		{"Bearer tok-b", http.StatusOK, "equipo-b"},
		{"Bearer tok-desconocido", http.StatusUnauthorized, ""},
		{"", http.StatusUnauthorized, ""},
	}
	for _, c := range casos {
		r := httptest.NewRequest(http.MethodPost, "/mcp/svc", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.wantCode {
			t.Errorf("%q: código %d, want %d", c.header, w.Code, c.wantCode)
		}
		if c.wantCode == http.StatusOK && w.Body.String() != c.wantTenant {
			t.Errorf("%q: tenant %q, want %q", c.header, w.Body.String(), c.wantTenant)
		}
	}
}

// Sin tenant en el contexto (auth desactivada), tenantFrom cae a "default" sin
// límites: el comportamiento de siempre.
func TestTenantFromCaeADefault(t *testing.T) {
	tnt := tenantFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	if tnt.name != defaultTenant {
		t.Errorf("tenant %q, want %q", tnt.name, defaultTenant)
	}
	if tnt.maxInflight != 0 || tnt.maxInstances != 0 {
		t.Error("el default no debería tener cuotas")
	}
}

// La cuota de inflight rechaza a partir del tope y vuelve a admitir al liberar.
func TestCuotaInflight(t *testing.T) {
	g := &Scheduler{}
	tnt := &tenant{name: "t", maxInflight: 2}

	if !g.tenantBegin(tnt) || !g.tenantBegin(tnt) {
		t.Fatal("las dos primeras deberían caber")
	}
	if g.tenantBegin(tnt) {
		t.Fatal("la tercera excede la cuota y debería rechazarse")
	}
	// Al liberar una, cabe otra.
	g.tenantEnd(tnt)
	if !g.tenantBegin(tnt) {
		t.Fatal("tras liberar una debería volver a caber")
	}
	// end de más no baja el contador por debajo de cero (no regala cuota).
	g.tenantEnd(tnt)
	g.tenantEnd(tnt)
	g.tenantEnd(tnt)
	g.tenantEnd(tnt)
	if n := g.TenantInflight()["t"]; n != 0 {
		t.Errorf("inflight = %d tras vaciar, want 0", n)
	}
}

// maxInflight 0 (el default) no rechaza nunca, aunque cuente.
func TestCuotaInflightSinLimite(t *testing.T) {
	g := &Scheduler{}
	tnt := &tenant{name: "libre"}
	for i := 0; i < 100; i++ {
		if !g.tenantBegin(tnt) {
			t.Fatalf("sin límite no debería rechazar (i=%d)", i)
		}
	}
}

// FAIRNESS: quien pide sitio sacrifica lo SUYO antes que lo de otro tenant,
// aunque el de otro sea más antiguo.
func TestEvictPrefiereMismoTenant(t *testing.T) {
	var congelada string
	g := &Scheduler{services: map[string]*entry{}, routes: map[string]*sessionRoute{}}
	g.freezeFn = func(id string) error { congelada = id; return nil }

	ahora := time.Now()
	// La de "B" es MÁS antigua, pero pertenece a otro tenant: no debe caer ella.
	g.services["svc-b"] = &entry{machineID: "m-b", tenant: "B", lastUse: ahora.Add(-2 * time.Hour)}
	// La de "A" es más reciente, pero es del tenant que pide: cae primero.
	g.services["svc-a"] = &entry{machineID: "m-a", tenant: "A", lastUse: ahora.Add(-time.Minute)}

	got := g.evictLRU(t.Context(), "quiere", "A")
	if got != "svc-a" {
		t.Fatalf("sacrificó %q, quería la propia del tenant A (svc-a)", got)
	}
	if congelada != "m-a" {
		t.Errorf("congeló %q, quería m-a", congelada)
	}
	g.mu.Lock()
	_, sigueB := g.services["svc-b"]
	g.mu.Unlock()
	if !sigueB {
		t.Error("desalojó la instancia de otro tenant teniendo una propia que ceder")
	}
}

// G-01: la cuota de instancias del tenant se comprueba Y RESERVA con el mismo
// candado. Sin la reserva, dos buildEntry concurrentes de servicios DISTINTOS
// del mismo tenant leen tenantInstances() (que solo mira g.services/g.extra)
// como si no hubiera nada en marcha, aunque el otro ya se haya reservado el
// hueco. La reserva tiene que verse ANTES de que la instancia exista.
func TestReservarTenantCuentaLoQueEstaEnVuelo(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
	tnt := &tenant{name: "t", maxInstances: 1}

	liberar1, err := g.reservarTenant(tnt)
	if err != nil {
		t.Fatalf("la primera reserva debía caber: %v", err)
	}
	// tenantInstances() seguiría diciendo 0 (nada registrado todavía), pero la
	// reserva en vuelo debe bastar para que la segunda choque con el tope.
	if _, err := g.reservarTenant(tnt); !errors.Is(err, errTenantInstances) {
		t.Fatalf("segunda reserva = %v; quería errTenantInstances (la primera sigue en vuelo)", err)
	}
	liberar1()
	// Liberada la primera, vuelve a caber.
	liberar2, err := g.reservarTenant(tnt)
	if err != nil {
		t.Fatalf("tras liberar debía volver a caber: %v", err)
	}
	liberar2()
}

// maxInstances 0 (sin límite) nunca reserva ni rechaza.
func TestReservarTenantSinLimiteNuncaRechaza(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
	tnt := &tenant{name: "libre"}
	for i := 0; i < 50; i++ {
		liberar, err := g.reservarTenant(tnt)
		if err != nil {
			t.Fatalf("sin límite no debería rechazar (i=%d): %v", i, err)
		}
		liberar()
	}
}

// Bajo carrera de verdad (-race), nunca puede haber más reservas vivas a la
// vez que el tope del tenant.
func TestReservarTenantEsSeguroBajoCarrera(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
	tnt := &tenant{name: "t", maxInstances: 3}

	var mu sync.Mutex
	var enVuelo, maxVisto int
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			liberar, err := g.reservarTenant(tnt)
			if err != nil {
				return
			}
			mu.Lock()
			enVuelo++
			if enVuelo > maxVisto {
				maxVisto = enVuelo
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			enVuelo--
			mu.Unlock()
			liberar()
		}()
	}
	wg.Wait()
	if maxVisto > tnt.maxInstances {
		t.Errorf("a la vez hubo %d reservas vivas; el tope era %d", maxVisto, tnt.maxInstances)
	}
}

// buildEntry rechaza por cuota SIN tocar el daemon (client a nil a propósito:
// si tocara el cliente antes de mirar la cuota, entraría en pánico).
func TestBuildEntryRechazaPorCuotaSinTocarElCliente(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
	tnt := &tenant{name: "t", maxInstances: 1}
	g.services["ya-despierto"] = &entry{tenant: "t"}

	_, err := g.buildEntry(context.Background(), "otro-servicio", tnt, false)
	if !errors.Is(err, errTenantInstances) {
		t.Fatalf("err = %v; quería errTenantInstances", err)
	}
}

// Cuando el tenant que pide no tiene nada propio ocioso, sí recurre a lo ajeno:
// hacer sitio importa más que la pureza del reparto.
func TestEvictCaeEnAjenoSiNoHayPropio(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, routes: map[string]*sessionRoute{}}
	g.freezeFn = func(id string) error { return nil }

	ahora := time.Now()
	g.services["svc-b"] = &entry{machineID: "m-b", tenant: "B", lastUse: ahora.Add(-time.Hour)}

	got := g.evictLRU(t.Context(), "quiere", "A")
	if got != "svc-b" {
		t.Fatalf("sacrificó %q; sin nada propio debería ceder la ajena (svc-b)", got)
	}
}
