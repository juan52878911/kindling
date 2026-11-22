package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// El fondo lee lo que quiere cada plantilla de la anotación del snapshot, que es
// donde la dejó quien la construyó: así no hay una segunda lista de plantillas
// que mantener en sincronía.
func TestDeseoDe(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "ts", Imagen: "toolchain", Pool: 3},
	})

	casos := []struct {
		nombre string
		snap   *api.Snapshot
		quiere int
		cual   string
	}{
		{"con receta y pool", &api.Snapshot{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 3, "ts"},
		{"snapshot ajeno", &api.Snapshot{Name: "notas",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 0, ""},
		{"sin anotación", &api.Snapshot{Name: "sbx-otra"}, 0, ""},
		{"anotación ilegible", &api.Snapshot{Name: "sbx-rota",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: json.RawMessage("{")}}, 0, ""},
	}
	for _, c := range casos {
		cual, quiere := DeseoDe(c.snap)
		if quiere != c.quiere || (c.quiere > 0 && cual != c.cual) {
			t.Errorf("%s: (%q, %d), quería (%q, %d)", c.nombre, cual, quiere, c.cual, c.quiere)
		}
	}
}

// Una plantilla sin pool no fabrica nada: el fondo es opt-in, porque cuesta RAM
// en reposo.
func TestSinPoolNoSeFabricaNada(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "sin", Imagen: "toolchain"},
	})
	if _, quiere := DeseoDe(&api.Snapshot{Name: "sbx-sin",
		Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}); quiere != 0 {
		t.Fatalf("quiere %d, want 0", quiere)
	}
}

// daemonPool es un daemon mínimo para el fondo: un snapshot de plantilla con
// pool, los sandboxes que se le den, y cuenta lo que se crea y se borra. Lo
// demás (grafos) contesta 404.
type daemonPool struct {
	mu       sync.Mutex
	vivas    []*api.Machine
	creadas  int
	borradas []string
	// snaps, si no es nil, sustituye al snapshot sbx-ts por defecto; falla
	// hace fallar la creación desde esos snapshots; pedidas guarda cada
	// creación aceptada.
	snaps   []*api.Snapshot
	falla   map[string]bool
	pedidas []api.SandboxRequest
	// retardo hace tardar cada creación; enCurso y maxEnCurso miden cuántas
	// hay a la vez; sinMemoria hace fallar con 507 las que lleguen del N-ésimo
	// en adelante (0: ninguna).
	retardo    time.Duration
	enCurso    int
	maxEnCurso int
	intentos   int
	sinMemoria int
	// conReclamo anuncia sandbox-claim en GET /info; renovadas guarda los
	// POST /sandboxes/{ref}/renew.
	conReclamo bool
	renovadas  []string
	// inicios guarda el instante en que empezó cada creación; listados cuenta
	// los GET /sandboxes.
	inicios  []time.Time
	listados int
}

func nuevoDaemonPool(t *testing.T, pool int, vivas ...*api.Machine) (*daemonPool, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pool")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	d := &daemonPool{vivas: vivas}
	receta, _ := json.Marshal(map[string]any{"template": plantilla.Plantilla{Nombre: "ts", Imagen: "toolchain", Pool: pool}})
	m := http.NewServeMux()
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.snaps != nil {
			json.NewEncoder(w).Encode(d.snaps)
			return
		}
		json.NewEncoder(w).Encode([]*api.Snapshot{{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}})
	})
	m.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		info := api.Info{Version: "fake"}
		if d.conReclamo {
			info.Capabilities = []string{api.CapabilityClaim}
		}
		json.NewEncoder(w).Encode(info)
	})
	m.HandleFunc("POST /sandboxes/{ref}/renew", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.renovadas = append(d.renovadas, r.PathValue("ref"))
		json.NewEncoder(w).Encode(&api.Machine{ID: r.PathValue("ref"), State: api.StateRunning})
	})
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.listados++
		json.NewEncoder(w).Encode(d.vivas)
	})
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req api.SandboxRequest
		json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.intentos++
		if d.sinMemoria > 0 && d.intentos >= d.sinMemoria {
			d.mu.Unlock()
			w.WriteHeader(http.StatusInsufficientStorage)
			json.NewEncoder(w).Encode(api.Error{Message: "not enough memory", Code: api.CodeInsufficientMemory})
			return
		}
		d.inicios = append(d.inicios, time.Now())
		d.enCurso++
		d.maxEnCurso = max(d.maxEnCurso, d.enCurso)
		retardo := d.retardo
		d.mu.Unlock()
		time.Sleep(retardo)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.enCurso--
		if d.falla[req.From] {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(api.Error{Message: "boom"})
			return
		}
		d.creadas++
		d.pedidas = append(d.pedidas, req)
		// Como un daemon de verdad: la creada aparece en el listado siguiente,
		// así una segunda vuelta no la vuelve a pedir.
		nueva := &api.Machine{ID: fmt.Sprintf("nueva%010d", d.creadas), State: api.StateRunning, Labels: req.Labels}
		d.vivas = append(d.vivas, nueva)
		json.NewEncoder(w).Encode(nueva)
	})
	m.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.borradas = append(d.borradas, r.PathValue("ref"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d, sock
}

func precalentada(id string, st api.State) *api.Machine {
	return &api.Machine{ID: id, State: st, Labels: map[string]string{
		api.LabelKind: api.KindSandbox, LabelTemplate: "ts"}}
}

// Una precalentada rota (failed, stopped) no la reclama nadie: no cuenta para
// el fondo, que fabrica otra, y se borra para que no ocupe plaza para siempre.
// Las que el frontal sí reclama (running, frozen) cuentan y se dejan.
func TestVueltaCuentaSoloLasReclamables(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 2,
		precalentada("rota0123456789a", api.StateFailed),
		precalentada("parada123456789", api.StateStopped),
		precalentada("viva01234567890", api.StateRunning))
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 1 {
		t.Errorf("created %d, want 1", d.creadas)
	}
	if len(d.borradas) != 2 || d.borradas[0] != "rota0123456789a" || d.borradas[1] != "parada123456789" {
		t.Errorf("removed %v, want the failed and the stopped one", d.borradas)
	}

	d, sock = nuevoDaemonPool(t, 1, precalentada("dormida12345678", api.StateWarm))
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	if d.creadas != 0 || len(d.borradas) != 0 {
		t.Errorf("a frozen prewarmed one: created %d, removed %v", d.creadas, d.borradas)
	}
}

// snapPool es el snapshot de la plantilla nombre con pool y guardado en hecho.
func snapPool(nombre string, pool int, hecho time.Time) *api.Snapshot {
	receta, _ := json.Marshal(map[string]any{"template": plantilla.Plantilla{Nombre: nombre, Imagen: "toolchain", Pool: pool}})
	return &api.Snapshot{Name: plantilla.SnapshotDe(nombre), CreatedAt: hecho,
		Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}
}

// Tras un rebuild (mismo nombre, otro snapshot) las precalentadas sin dueño de
// antes se borran y se fabrican del snapshot nuevo, con su versión puesta; las
// de una plantilla que ya no existe se borran sin reponer.
func TestVueltaBorraPrecalentadasViejasYHuerfanas(t *testing.T) {
	ahora := time.Now().UTC()
	sn := snapPool("ts", 1, ahora)
	vieja := precalentada("vieja0123456789", api.StateRunning)
	vieja.Labels[plantilla.LabelVersion] = plantilla.VersionDe(&api.Snapshot{CreatedAt: ahora.Add(-time.Hour)})
	huerfana := precalentada("huerfana1234567", api.StateWarm)
	huerfana.Labels[LabelTemplate] = "borrada"
	deTenant := precalentada("tenant012345678", api.StateRunning)
	deTenant.Labels[LabelTenant] = "ana"
	d, sock := nuevoDaemonPool(t, 0, vieja, huerfana, deTenant)
	d.snaps = []*api.Snapshot{sn}
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.borradas) != 2 || d.borradas[0] != "vieja0123456789" || d.borradas[1] != "huerfana1234567" {
		t.Errorf("removed %v, want the stale and the orphaned one (not the tenant's)", d.borradas)
	}
	if d.creadas != 1 || d.pedidas[0].Labels[plantilla.LabelVersion] != plantilla.VersionDe(sn) {
		t.Errorf("created %d (%+v), want one labeled with version %s", d.creadas, d.pedidas, plantilla.VersionDe(sn))
	}
}

// Que una plantilla no se pueda fabricar no deja sin fondo a las demás del host.
func TestVueltaSigueTrasUnFalloDeOtraPlantilla(t *testing.T) {
	ahora := time.Now().UTC()
	d, sock := nuevoDaemonPool(t, 0)
	d.snaps = []*api.Snapshot{snapPool("rota", 1, ahora), snapPool("buena", 1, ahora)}
	d.falla = map[string]bool{"sbx-rota": true}
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 1 || d.pedidas[0].From != "sbx-buena" {
		t.Errorf("created %d (%+v), want the prewarmed of sbx-buena", d.creadas, d.pedidas)
	}
}

// El relleno crea en paralelo pero nunca más de CreacionesALaVez a la vez, y
// con 12 por crear y 40 ms cada una tarda ~3 tandas, no 12.
func TestRellenoEnParaleloAcotado(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 12)
	d.retardo = 40 * time.Millisecond
	ini := time.Now()
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 12 {
		t.Fatalf("created %d, want 12", d.creadas)
	}
	if d.maxEnCurso > CreacionesALaVez || d.maxEnCurso < 2 {
		t.Fatalf("%d creations at once, want 2..%d", d.maxEnCurso, CreacionesALaVez)
	}
	if el := time.Since(ini); el > 12*40*time.Millisecond*3/4 {
		t.Fatalf("took %s: not parallel", el)
	}
}

// Al primer 507 la plantilla deja de crear: las que no habían empezado no se
// piden (las en vuelo, como mucho CreacionesALaVez-1, acaban).
func TestRellenoParaAlPrimer507(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 40)
	d.sinMemoria = 3
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.intentos > 3+CreacionesALaVez {
		t.Fatalf("%d attempts after the first 507 at the 3rd", d.intentos)
	}
}

// Pedir despierta a Vigilar sin esperar al tic, y varios Pedir seguidos no
// encadenan vueltas.
func TestPedirDisparaUnaVueltaSinEsperarAlTic(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 2)
	r := Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Vigilar(ctx)
	creadas := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.creadas }
	esperar := func(n int, plazo time.Duration) bool {
		fin := time.Now().Add(plazo)
		for time.Now().Before(fin) {
			if creadas() >= n {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}
	if !esperar(2, 2*time.Second) { // la vuelta inicial
		t.Fatal("no initial round")
	}
	// Se gasta una: ahora el daemon solo tiene 1 viva y falta otra.
	d.mu.Lock()
	d.vivas = nil // el fondo vacío no espera
	d.mu.Unlock()
	ini := time.Now()
	for i := 0; i < 50; i++ {
		r.Pedir("h1", "ts")
	}
	if !esperar(4, 2*time.Second) {
		t.Fatal("Pedir did not trigger a round")
	}
	if el := time.Since(ini); el > 50*time.Millisecond {
		t.Fatalf("the round took %s after Pedir", el)
	}
	time.Sleep(100 * time.Millisecond)
	if n := creadas(); n != 4 {
		t.Fatalf("50 Pedir made %d creations in total, want 4 (one round)", n)
	}
	// Un host o plantilla ajenos no crean nada.
	r.Pedir("h1", "otra")
	time.Sleep(100 * time.Millisecond)
	if n := creadas(); n != 4 {
		t.Fatalf("Pedir of another template created: %d", n)
	}
}

func nuevoRell(sock string) *Rellenador {
	return Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0))
}

// H1: con un host que anuncia sandbox-claim, una precalentada warm (su TTL
// venció y se congeló) no la reclama nadie: se trata como rota, se borra y se
// repone.
func TestVueltaReponeLasWarmSiElHostReclama(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 1, precalentada("dormida12345678", api.StateWarm))
	d.conReclamo = true
	nuevoRell(sock).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 1 || len(d.borradas) != 1 || d.borradas[0] != "dormida12345678" {
		t.Fatalf("created %d removed %v, want 1 and the warm one", d.creadas, d.borradas)
	}
}

// H1: a una running con menos de un cuarto de TTL se le renueva; a una recién
// hecha, no.
func TestVueltaRenuevaLasQueSeVanADormir(t *testing.T) {
	ya := time.Now()
	vieja := precalentada("vieja0123456789", api.StateRunning)
	vieja.TTLSeconds = 3600
	hace := ya.Add(-50 * time.Minute)
	vieja.TTLAt = &hace
	nueva := precalentada("nueva0123456789", api.StateRunning)
	nueva.TTLSeconds = 3600
	nueva.TTLAt = &ya
	d, sock := nuevoDaemonPool(t, 2, vieja, nueva)
	nuevoRell(sock).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.renovadas) != 1 || d.renovadas[0] != "vieja0123456789" {
		t.Fatalf("renewed %v, want only the old one", d.renovadas)
	}
	if d.creadas != 0 {
		t.Fatalf("created %d", d.creadas)
	}
}

// Tras un fallo de creación el host/plantilla descansa unos segundos: la vuelta
// siguiente no vuelve a golpear.
func TestRellenoRetrocedeTrasUnFallo(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 1)
	d.falla = map[string]bool{"sbx-ts": true}
	r := nuevoRell(sock)
	r.Vuelta(context.Background())
	r.Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.intentos != 1 {
		t.Fatalf("%d attempts across two rounds, want 1", d.intentos)
	}
}

// vivasDe devuelve n precalentadas sanas de la plantilla ts.
func vivasDe(n int) []*api.Machine {
	var v []*api.Machine
	for i := 0; i < n; i++ {
		v = append(v, precalentada(fmt.Sprintf("viva%011d", i), api.StateRunning))
	}
	return v
}

// I2-E: tras una ráfaga de reclamos el relleno espera 300 ms desde el último
// (para no competir con las primeras peticiones del que reclama) y crea de 2 en
// 2, no de 4 en 4.
func TestPedirEsperaYRellenaDeDosEnDos(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 10, vivasDe(10)...)
	d.retardo = 30 * time.Millisecond
	r := nuevoRell(sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Vigilar(ctx)
	time.Sleep(100 * time.Millisecond) // vuelta inicial: no hace nada
	d.mu.Lock()
	d.vivas = vivasDe(2) // la ráfaga gastó 8
	d.mu.Unlock()
	var ultimo time.Time
	for i := 0; i < 10; i++ {
		r.Pedir("h1", "ts")
		ultimo = time.Now()
		time.Sleep(5 * time.Millisecond)
	}
	fin := time.Now().Add(3 * time.Second)
	for time.Now().Before(fin) {
		d.mu.Lock()
		n := d.creadas
		d.mu.Unlock()
		if n >= 8 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 8 {
		t.Fatalf("created %d, want 8", d.creadas)
	}
	t.Logf("starts %v creadas %d", d.inicios, d.creadas)
	if espera := d.inicios[0].Sub(ultimo); espera < 280*time.Millisecond {
		t.Errorf("first creation %s after the last claim, want >= 300ms", espera)
	}
	if d.maxEnCurso > 2 {
		t.Errorf("%d creations at once, want at most 2", d.maxEnCurso)
	}
}

// Con el fondo de la plantilla vacío no hay espera: el siguiente reclamo ya
// cae al restore.
func TestPedirConElFondoVacioNoEspera(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 2, vivasDe(2)...)
	r := nuevoRell(sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Vigilar(ctx)
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	d.vivas = nil
	d.mu.Unlock()
	ini := time.Now()
	r.Pedir("h1", "ts")
	for time.Since(ini) < 2*time.Second {
		d.mu.Lock()
		n := len(d.inicios)
		d.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.inicios) == 0 || d.inicios[0].Sub(ini) > 150*time.Millisecond {
		t.Fatalf("empty pool waited: starts %v", d.inicios)
	}
}

// Un pedido que llega con una vuelta en curso no se pierde: se atiende al acabar
// ésta, sin esperar al tic.
func TestPedidoConVueltaEnCursoSeAtiendeAlAcabar(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 3, vivasDe(3)...)
	r := nuevoRell(sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Vigilar(ctx)
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	d.vivas = vivasDe(1)
	d.retardo = 600 * time.Millisecond
	d.mu.Unlock()
	hecha := make(chan struct{})
	go func() { r.Vuelta(ctx); close(hecha) }()
	time.Sleep(50 * time.Millisecond)
	r.Pedir("h1", "ts")
	time.Sleep(450 * time.Millisecond) // pasa la espera con la vuelta aún en curso
	d.mu.Lock()
	antes := d.listados
	d.mu.Unlock()
	select {
	case <-hecha:
	case <-time.After(5 * time.Second):
		t.Fatal("the round never ended")
	}
	time.Sleep(300 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listados <= antes {
		t.Errorf("listings %d -> %d: the request was dropped", antes, d.listados)
	}
}
