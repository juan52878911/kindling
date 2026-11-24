package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// C9-09: una réplica de un servicio que nunca congela (IdleRemove, IdlePause)
// se pide con OnTTL remove: el daemon la carga sin seguimiento de páginas
// sucias y su TTL de seguridad la borra en vez de congelarla. Con la política
// de hoy no se pide nada.
func TestIdleRemovePideOnTTLRemove(t *testing.T) {
	for _, c := range []struct {
		p    IdlePolicy
		want string
	}{{IdleRemove, api.OnTTLRemove}, {IdlePause, api.OnTTLRemove}, {IdleDefault, ""}, {IdleFreeze, ""}} {
		d := nuevoDaemonServicio()
		g := gwServicio(t, d, invitadoOK(t))
		p := c.p
		g.IdleFor = func(string) IdlePolicy { return p }
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := g.ensure(ctx, "svc"); err != nil {
			cancel()
			t.Fatalf("%q: ensure: %v", c.p, err)
		}
		cancel()
		ps := d.pedidasCopia()
		if len(ps) != 1 || ps[0].OnTTL != c.want {
			t.Errorf("idle %q: on_ttl pedido = %+v; quería %q", c.p, ps, c.want)
		}
	}
}

// AfterRestore corre con las instancias RESTAURADAS, después de que el puerto
// acepte y antes de la sonda; no al descongelar ni al adoptar.
func TestAfterRestoreSoloTrasRestaurar(t *testing.T) {
	var ganchos atomic.Int32
	var sondaAntes atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && ganchos.Load() == 0 {
			sondaAntes.Store(true)
		}
	}))
	t.Cleanup(srv.Close)
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, srv)
	g.ReadyFor = func(string) *Probe { return &Probe{Path: "/healthz", Timeout: 2 * time.Second} }
	var vistos []string
	var mu sync.Mutex
	g.AfterRestore = func(_ context.Context, service string, mc *api.Machine) error {
		mu.Lock()
		vistos = append(vistos, service+"/"+mc.ID)
		mu.Unlock()
		ganchos.Add(1)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.ensure(ctx, "svc"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if sondaAntes.Load() {
		t.Error("la sonda llegó antes que AfterRestore")
	}
	mu.Lock()
	if len(vistos) != 1 || vistos[0] != "svc/r1" {
		t.Errorf("AfterRestore vio %v; quería [svc/r1]", vistos)
	}
	mu.Unlock()

	// Descongelar: el mismo planificador, una máquina congelada de antes.
	d2 := nuevoDaemonServicio()
	d2.poner(&api.Machine{ID: "w1", Name: "w1", State: api.StateWarm, IP: "127.0.0.1",
		Labels: map[string]string{api.LabelService: "svc"}, MemMiB: 128})
	d2.poner(&api.Machine{ID: "v1", Name: "v1", State: api.StateRunning, IP: "127.0.0.1",
		Labels: map[string]string{api.LabelService: "otro"}, MemMiB: 128})
	g2 := gwServicio(t, d2, invitadoOK(t))
	g2.AfterRestore = g.AfterRestore
	antes := ganchos.Load()
	if e, err := g2.ensure(ctx, "svc"); err != nil || e.machineID != "w1" {
		t.Fatalf("ensure por thaw: %v (%v)", e, err)
	}
	if e, err := g2.ensure(ctx, "otro"); err != nil || e.machineID != "v1" {
		t.Fatalf("ensure por adopción: %v (%v)", e, err)
	}
	if n := ganchos.Load() - antes; n != 0 {
		t.Errorf("AfterRestore corrió %d veces al descongelar o adoptar", n)
	}
}

// Un AfterRestore que falla no deja la réplica en el mapa: se borra como una
// que no llegó a escuchar y la petición recibe el error.
func TestAfterRestoreFalloSueltaReplica(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.AfterRestore = func(context.Context, string, *api.Machine) error {
		return errors.New("mmds rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.ensure(ctx, "svc"); err == nil {
		t.Fatal("ensure no falló con un AfterRestore que falla")
	}
	if d.existe("r1") {
		t.Error("la restaurada cuyo AfterRestore falló sigue viva")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entriesLocked("svc")) != 0 || g.adquiriendo["r1"] {
		t.Error("la restaurada fallida quedó en el mapa o marcada")
	}
}

// Una instancia con secretos en claro (HasSecrets tras AfterRestore) no se
// congela nunca: congelar volcaría su RAM a disco. Ociosa, se borra con
// cualquier política; el desalojo por memoria también la borra.
func TestMarcadaNoSeCongela(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.IdleFor = func(string) IdlePolicy { return IdleFreeze }
	g.AfterRestore = func(_ context.Context, _ string, mc *api.Machine) error {
		mc.HasSecrets = true
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.ensure(ctx, "svc"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	envejecer(t, g, "svc")
	g.reapOnce(ctx)
	if n := d.contar("POST /machines/r1/freeze"); n != 0 {
		t.Errorf("la marcada se congeló (%d)", n)
	}
	if d.existe("r1") {
		t.Error("la marcada ociosa no se borró")
	}

	// Desalojo y pausadas que se enfrían: tampoco congelan una marcada.
	g2, _, congeladas, borradas := gwPausasPolitica(0, IdleFreeze)
	g2.services["svc"] = &entry{machineID: "m-marcada", lastUse: time.Now().Add(-time.Hour), marcada: true}
	if got := g2.evictLRU(context.Background(), "otro", ""); got != "svc" || !borradas["m-marcada"] {
		t.Errorf("evictLRU = %q, borradas=%v; debía borrar la marcada", got, borradas)
	}
	g2.pausadas = map[string]pausada{"m-p": {service: "svc", memMiB: 64, at: time.Now().Add(-time.Hour), marcada: true}}
	g2.enfriarPausadas(context.Background())
	if !borradas["m-p"] || len(congeladas) != 0 {
		t.Errorf("pausada marcada que se enfría: borradas=%v congeladas=%v", borradas, congeladas)
	}
}

// Una máquina con secretos que este planificador no tiene como instancia (la
// dejó un proceso anterior: no se sabe qué generación de secretos lleva) no se
// adopta: se restaura otra y esa se borra aparte.
func TestMarcadaAjenaNoSeAdopta(t *testing.T) {
	d := nuevoDaemonServicio()
	d.poner(&api.Machine{ID: "vieja", Name: "vieja", State: api.StateRunning, IP: "127.0.0.1",
		Labels: map[string]string{api.LabelService: "svc"}, MemMiB: 128, HasSecrets: true})
	g := gwServicio(t, d, invitadoOK(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if e.machineID == "vieja" {
		t.Fatal("se adoptó una máquina con secretos de generación desconocida")
	}
	esperarQue(t, 3*time.Second, "la marcada ajena borrada", func() bool { return !d.existe("vieja") })
}

// Retire (rotación de un secreto en claro): las instancias del servicio dejan
// de recibir peticiones nuevas; la ociosa se borra ya, la que tiene trabajo en
// vuelo al terminarlo, y la siguiente petición restaura otra.
func TestRetireDrena(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.IdleFor = func(string) IdlePolicy { return IdleRemove }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ocupada, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	g.begin(ocupada)
	ociosa, err := g.scaleOut(ctx, "svc", tenantFrom(ctx))
	if err != nil {
		t.Fatal(err)
	}
	g.Retire(ctx, "svc")
	esperarQue(t, 3*time.Second, "la ociosa borrada", func() bool { return !d.existe(ociosa.machineID) })
	if !d.existe(ocupada.machineID) {
		t.Fatal("se borró la réplica con una petición en vuelo")
	}
	nueva, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if nueva.machineID == ocupada.machineID || nueva.machineID == ociosa.machineID {
		t.Fatalf("la petición nueva fue a una réplica retirada (%s)", nueva.machineID)
	}
	g.end(ocupada)
	esperarQue(t, 3*time.Second, "la retirada borrada al acabar su trabajo", func() bool { return !d.existe(ocupada.machineID) })
}
