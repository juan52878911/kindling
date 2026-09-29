package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteListo es un agente de invitado con /ready y /hooks programables.
type agenteListo struct {
	mu       sync.Mutex
	listoEn  int // /ready contesta listo a partir de esta llamada (1 = la primera)
	llamadas int
	falla    bool // los ganchos fallan
	ganchos  bool // la imagen declara ganchos
	lanzados []string
	sinRuta  bool // agente viejo: 404 a todo
}

func (a *agenteListo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sinRuta {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case api.GuestReadyPath:
		a.llamadas++
		st := api.GuestReady{Probe: true, HasHooks: a.ganchos}
		switch {
		case a.falla:
			st.Hooks, st.Detail = api.HooksFailed, "post-restore hook 10-id: exit status 1"
		case a.llamadas >= a.listoEn:
			st.Ready = true
			if a.ganchos {
				st.Hooks = api.HooksDone
			}
		default:
			st.Detail = "sys.boot_completed is not 1"
		}
		if !st.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(st)
	case api.GuestHooksPath:
		a.lanzados = append(a.lanzados, r.URL.Query().Get("restore"))
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(api.GuestReady{Probe: true, HasHooks: true, Hooks: api.HooksRunning})
	default:
		http.NotFound(w, r)
	}
}

func conAgenteListo(t *testing.T, a *agenteListo) (*Manager, string) {
	t.Helper()
	m, id := managerConInvitado(t, a)
	m.byID[id].State = api.StateRunning
	return m, id
}

func TestInterpretarListo(t *testing.T) {
	casos := []struct {
		code  int
		body  string
		ready bool
		err   error
	}{
		{200, `{"ready":true,"probe":true}`, true, nil},
		{503, `{"ready":false,"probe":true,"detail":"x"}`, false, nil},
		{200, `{"ready":false}`, false, nil},
		{503, `{"ready":true}`, false, nil}, // contradictorio: gana lo prudente
		{404, `404 page not found`, false, errListoViejo},
		{400, `missing Mcp-Session-Id`, false, errListoViejo},
		// Un servidor que contesta 200 a cualquier ruta no tiene sonda: no se
		// reintenta hasta agotar el plazo (el agente falso de los tests de
		// commit hacía esperar 120 s a cada Commit en Linux).
		{200, ``, false, errListoViejo},
		{200, `OK`, false, errListoViejo},
	}
	for _, c := range casos {
		st, err := interpretarListo(c.code, []byte(c.body))
		if !errors.Is(err, c.err) || (c.err == nil && err != nil) || st.Ready != c.ready {
			t.Errorf("%d %s → %+v %v", c.code, c.body, st, err)
		}
	}
	if _, err := interpretarListo(500, []byte("boom")); err == nil {
		t.Error("un 500 es un error")
	}
	if _, err := interpretarListo(503, []byte("busy")); err == nil || errors.Is(err, errListoViejo) {
		t.Errorf("un 503 que no es JSON es un error pasajero, no un agente sin sonda: %v", err)
	}
}

func TestEstadoListo(t *testing.T) {
	casos := map[string]api.GuestReady{
		api.ReadyUnknown: {Ready: true},
		api.ReadyYes:     {Ready: true, Probe: true},
		api.ReadyWaiting: {Probe: true},
		api.ReadyFailed:  {HasHooks: true, Hooks: api.HooksFailed},
	}
	for quiero, st := range casos {
		if got := estadoListo(st); got != quiero {
			t.Errorf("estadoListo(%+v) = %q, quiero %q", st, got, quiero)
		}
	}
}

func TestWaitReadyEsperaALaSonda(t *testing.T) {
	a := &agenteListo{listoEn: 3}
	m, id := conAgenteListo(t, a)
	t0 := time.Now()
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 10 * time.Second})
	if err != nil || res.Ready != api.ReadyYes || !res.OK() {
		t.Fatalf("WaitReady = %+v, %v", res, err)
	}
	if a.llamadas != 3 || time.Since(t0) < 3*pasoListoPrimero {
		t.Errorf("llamadas = %d en %s: tenía que insistir hasta la tercera", a.llamadas, time.Since(t0))
	}
	if got, _ := m.Get(id); got.Ready != api.ReadyYes {
		t.Errorf("Machine.Ready = %q", got.Ready)
	}
}

func TestWaitReadyPlazoAgotado(t *testing.T) {
	a := &agenteListo{listoEn: 1 << 30}
	m, id := conAgenteListo(t, a)
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 600 * time.Millisecond})
	if !errors.Is(err, ErrNotReady) || res.Ready != api.ReadyWaiting || res.OK() {
		t.Fatalf("WaitReady = %+v, %v", res, err)
	}
	if !strings.Contains(err.Error(), "sys.boot_completed") {
		t.Errorf("el error tiene que traer el motivo de la sonda: %v", err)
	}
}

func TestWaitReadyGanchoFallidoNoEspera(t *testing.T) {
	a := &agenteListo{falla: true, ganchos: true}
	m, id := conAgenteListo(t, a)
	t0 := time.Now()
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: time.Minute})
	if !errors.Is(err, ErrNotReady) || res.Ready != api.ReadyFailed || time.Since(t0) > 5*time.Second {
		t.Fatalf("WaitReady = %+v, %v en %s", res, err, time.Since(t0))
	}
}

func TestWaitReadyAgenteViejoNoEspera(t *testing.T) {
	m, id := conAgenteListo(t, &agenteListo{sinRuta: true})
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: time.Minute})
	if err != nil || !res.OK() || res.Guest != nil {
		t.Fatalf("un agente anterior a /ready es 'nada que esperar': %+v, %v", res, err)
	}
}

func TestWaitReadySinAgenteVale(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	// Un puerto donde no escucha nadie: 127.0.0.1:1.
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning,
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	t0 := time.Now()
	if _, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: time.Minute, SinAgenteVale: true}); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 3*time.Second {
		t.Errorf("sin agente no hay que esperar al plazo: %s", time.Since(t0))
	}
	// Sin SinAgenteVale sí se espera (a que arranque), y acaba en ErrNotReady.
	if _, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 500 * time.Millisecond}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("sin agente y sin SinAgenteVale = %v", err)
	}
}

func TestListoParaCongelarExplica(t *testing.T) {
	m, id := conAgenteListo(t, &agenteListo{listoEn: 1 << 30})
	err := m.listoParaCongelar(context.Background(), id, 300*time.Millisecond)
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("listoParaCongelar = %v", err)
	}
	// Una máquina que no existe no es asunto de esta comprobación.
	if err := m.listoParaCongelar(context.Background(), "nadie", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCommitEsperaAListoYSkipReady(t *testing.T) {
	m, id := conAgenteListo(t, &agenteListo{listoEn: 1 << 30})
	_, err := m.CommitWith(context.Background(), id, "dorado", CommitOptions{ReadyWait: 300 * time.Millisecond})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("commit de un invitado no listo = %v", err)
	}
	// Con SkipReady pasa de largo y llega al commit de verdad (que aquí falla
	// por otra cosa: no hay VMM).
	_, err = m.CommitWith(context.Background(), id, "dorado", CommitOptions{SkipReady: true})
	if err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("commit con SkipReady = %v", err)
	}
}

func TestTrasRestaurarLanzaGanchos(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 2}
	m, id := conAgenteListo(t, a)
	m.quit = make(chan struct{})
	defer close(m.quit)
	m.trasRestaurar(context.Background(), id, api.ResyncInstance, &api.GuestReady{Ready: true, HasHooks: true})
	a.mu.Lock()
	lanzados := append([]string(nil), a.lanzados...)
	a.mu.Unlock()
	if len(lanzados) != 1 || lanzados[0] != api.ResyncInstance {
		t.Fatalf("ganchos lanzados = %v", lanzados)
	}
	// La vigía acaba viendo listo.
	plazo := time.Now().Add(5 * time.Second)
	for time.Now().Before(plazo) {
		if mc, _ := m.Get(id); mc.Ready == api.ReadyYes {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mc, _ := m.Get(id)
	t.Fatalf("la vigía no llegó a listo: %q", mc.Ready)
}

func TestTrasRestaurarSinGanchosNoPide(t *testing.T) {
	var n atomic.Int32
	m, id := conAgenteListo(t, &agenteListo{})
	m.pruebasGanchos = func(ctx context.Context, id, kind string) (api.GuestReady, error) {
		n.Add(1)
		return api.GuestReady{}, nil
	}
	m.trasRestaurar(context.Background(), id, api.ResyncThaw, &api.GuestReady{Ready: true})
	m.trasRestaurar(context.Background(), id, api.ResyncThaw, nil)
	if n.Load() != 0 {
		t.Fatal("sin ganchos declarados no se pide POST /hooks")
	}
}

func TestRunHooksEspera(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 2}
	m, id := conAgenteListo(t, a)
	res, err := m.RunHooks(context.Background(), id, 10*time.Second)
	if err != nil || res.Ready != api.ReadyYes {
		t.Fatalf("RunHooks = %+v, %v", res, err)
	}
	if len(a.lanzados) != 1 || a.lanzados[0] != "manual" {
		t.Fatalf("lanzados = %v", a.lanzados)
	}
	viejo, vid := conAgenteListo(t, &agenteListo{sinRuta: true})
	if _, err := viejo.RunHooks(context.Background(), vid, 0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("RunHooks con agente viejo = %v", err)
	}
}

func TestTechoCPUDeLaReceta(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "images"), 0o755)
	m := &Manager{root: root}
	escribir := func(img string, rec api.ImageRecipe) {
		b, _ := json.Marshal(rec)
		os.WriteFile(m.recipePath(img), b, 0o644)
	}
	escribir("android", api.ImageRecipe{Name: "android", CPUPctPerVCPU: 100, CPUPct: 150})
	escribir("von", api.ImageRecipe{Name: "von", CPUPct: 150})
	escribir("mcp", api.ImageRecipe{Name: "mcp"})
	os.WriteFile(m.recipePath("rota"), []byte("{"), 0o644)
	casos := []struct {
		img        string
		vcpus, def int
		quiero     int
	}{
		{"android", 2, 0, 200}, // por vCPU manda
		{"android", 4, 70, 400},
		{"von", 2, 70, 150},
		{"mcp", 1, 70, 70}, // la receta no dice nada: el de quien pide
		{"mcp", 1, 0, 0},   // ni eso: el del daemon (0 aquí)
		{"rota", 1, 30, 30},
		{"no-existe", 1, 0, 0},
		{"../x", 1, 0, 0},
	}
	for _, c := range casos {
		if got := m.techoCPUPorDefecto(c.img, c.vcpus, c.def); got != c.quiero {
			t.Errorf("techoCPUPorDefecto(%s, %d, %d) = %d, quiero %d", c.img, c.vcpus, c.def, got, c.quiero)
		}
	}
}

func TestIPv6DeLaReceta(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "images"), 0o755)
	m := &Manager{root: root}
	b, _ := json.Marshal(api.ImageRecipe{Name: "android", GuestIPv6Stack: true})
	os.WriteFile(m.recipePath("android"), b, 0o644)
	b, _ = json.Marshal(api.ImageRecipe{Name: "mcp"})
	os.WriteFile(m.recipePath("mcp"), b, 0o644)
	os.WriteFile(m.recipePath("rota"), []byte("{"), 0o644)
	for img, quiero := range map[string]bool{"android": true, "mcp": false, "rota": false, "no-existe": false, "../x": false} {
		if got := m.ipv6DeReceta(img); got != quiero {
			t.Errorf("ipv6DeReceta(%s) = %v, quiero %v", img, got, quiero)
		}
	}
	if !strings.Contains(bootArgs(nil, false, "", true), "ipv6.disable_ipv6=1") {
		t.Error("bootArgs con la pila IPv6 no lleva ipv6.disable_ipv6=1")
	}
}

func TestEvaluarSwap(t *testing.T) {
	const min = 16 << 10
	casos := []struct {
		nombre                     string
		usado, total, libre, maxPc int64
		rechaza                    bool
	}{
		// Hoy en el Mac: 3,6 de 4 GiB de swap (89 %) pero 66 GiB de disco:
		// puede crecer, no hay presión de verdad.
		{"swap alto con disco", 3665, 4096, 66 << 10, 85, false},
		// Los fallos de sigill.md: 8,5 de 9,2 GB y el disco en el suelo.
		{"swap lleno sin disco", 8700, 9400, 12 << 10, 85, true},
		{"poco swap sin disco", 1000, 9400, 12 << 10, 85, false},
		{"apagado", 9000, 9400, 0, 0, false},
		{"sin swap", 0, 0, 0, 85, false},
	}
	for _, c := range casos {
		err := evaluarSwap(c.usado, c.total, c.libre, min, c.maxPc)
		if (err != nil) != c.rechaza {
			t.Errorf("%s: %v", c.nombre, err)
		}
		if err != nil && !api.IsInsufficientMemory(err) {
			t.Errorf("%s: tiene que ser 507: %v", c.nombre, err)
		}
	}
}

func TestParseSwapUsage(t *testing.T) {
	b := make([]byte, 31) // 32 menos el cero final que quita syscall.Sysctl
	put := func(off int, v uint64) {
		for i := 0; i < 8; i++ {
			b[off+i] = byte(v >> (8 * i))
		}
	}
	put(0, 4096<<20)
	put(8, 430<<20)
	put(16, 3665<<20)
	usado, total, ok := parseSwapUsage(b)
	if !ok || usado != 3665 || total != 4096 {
		t.Fatalf("parseSwapUsage = %d %d %v", usado, total, ok)
	}
	if _, _, ok := parseSwapUsage(b[:10]); ok {
		t.Error("corto no vale")
	}
}

func TestMinFreeDiskMiBPlataforma(t *testing.T) {
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "")
	if minFreeDiskMiB() != minDiscoLibrePlataforma {
		t.Fatal("sin variable, el de la plataforma")
	}
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "500")
	if minFreeDiskMiB() != 500 {
		t.Fatal("la variable manda")
	}
	t.Setenv("KLING_MAX_SWAP_PCT", "0")
	if maxSwapPct() != 0 {
		t.Fatal("0 apaga")
	}
	t.Setenv("KLING_MAX_SWAP_PCT", "basura")
	if maxSwapPct() != defaultMaxSwapPct {
		t.Fatal("basura = el de siempre")
	}
}

func TestObjetivoConMeminfo(t *testing.T) {
	// Android de 1 GiB con 250 MiB disponibles: colchón de 256, no se aprieta.
	android := &api.Machine{MemMiB: 1024}
	if got := objetivoConMeminfo(android, 0, api.GuestMemInfo{TotalMiB: 992, AvailableMiB: 250}); got != 0 {
		t.Errorf("Android sin holgura: objetivo %d, quiero 0 (no inflar)", got)
	}
	// Con 700 disponibles: 700-256 = 444, por debajo del suelo de la mitad (512).
	if got := objetivoConMeminfo(android, 0, api.GuestMemInfo{TotalMiB: 992, AvailableMiB: 700}); got != 444 {
		t.Errorf("objetivo = %d, quiero 444", got)
	}
	// Un MCP de 256 casi vacío: el suelo de siempre (128) manda.
	mcp := &api.Machine{MemMiB: 256}
	if got := objetivoConMeminfo(mcp, 0, api.GuestMemInfo{TotalMiB: 240, AvailableMiB: 230}); got != 102 {
		t.Errorf("objetivo MCP = %d, quiero 102 (230-128)", got)
	}
	if got := objetivoConMeminfo(mcp, 0, api.GuestMemInfo{TotalMiB: 240, AvailableMiB: 1000}); got != objetivoSinEstadisticas(mcp) {
		t.Errorf("nunca por debajo del suelo: %d", got)
	}
}

func TestSqueezeRechazaCopiaCompartida(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}, socket: map[string]string{}}
	id := "abcdef0123456789"
	m.byID[id] = &api.Machine{ID: id, Name: "phone-1", From: "phone-gold", State: api.StateRunning, MemShared: true}
	_, err := m.Squeeze(context.Background(), id)
	if !errors.Is(err, ErrSqueezeShared) || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("squeeze de una copia compartida = %v", err)
	}
	// Con force pasa la comprobación (y falla después: no hay VMM).
	if _, err := m.SqueezeWith(context.Background(), id, true); errors.Is(err, ErrSqueezeShared) {
		t.Fatalf("force no la saltó: %v", err)
	}
}
