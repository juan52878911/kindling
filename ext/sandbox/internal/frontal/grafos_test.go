package frontal

import (
	"context"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/ext/sandbox/internal/pool"
	"github.com/juan52878911/kindling/pkg/api"
)

// plantillaAgentePG es el caso del issue: el sandbox del agente con su
// Postgres, unidos por una arista link.
func plantillaAgentePG(pool int) plantilla.PlantillaGrafo {
	return plantilla.PlantillaGrafo{
		Kind: plantilla.KindGrafo, Nombre: "agente-pg", Pool: pool,
		Nodes: map[string]api.GraphNode{
			"agent": {From: "sbx-agent", AllowExec: true},
			"db":    {From: "pg16-golden", Ports: []int{5432}},
		},
		Edges: []api.GraphEdge{{From: "agent", To: "db", Kind: api.GraphEdgeLink, Port: 5432}},
	}
}

func guardarGrafo(t *testing.T, f *falso, p plantilla.PlantillaGrafo) {
	t.Helper()
	if err := plantilla.GuardarGrafo(context.Background(), api.NewClient(f.endpoint), p); err != nil {
		t.Fatal(err)
	}
}

// rellenar da una vuelta del fondo sobre los daemons dados.
func rellenar(t *testing.T, daemons map[string]*falso) {
	t.Helper()
	eps := map[string]string{}
	for n, f := range daemons {
		eps[n] = f.endpoint
	}
	pool.Nuevo(hosts.Nuevo(eps), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
}

// grafosLibres cuenta, en el daemon falso, las instancias libres de tpl.
func grafosLibres(t *testing.T, f *falso, tpl string) (libres []plantilla.Instancia) {
	t.Helper()
	c := api.NewClient(f.endpoint)
	gs, err := c.Graphs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ms, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range plantilla.Instancias(gs, ms) {
		if in.Plantilla == tpl && in.Libre() {
			libres = append(libres, in)
		}
	}
	return libres
}

func TestPoolFabricaGrafosCongelados(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(2))
	daemons := map[string]*falso{"a": f}

	rellenar(t, daemons)
	libres := grafosLibres(t, f, "agente-pg")
	if len(libres) != 2 {
		t.Fatalf("%d free graphs, want 2", len(libres))
	}
	for _, in := range libres {
		if in.Grafo.State != api.GraphStateFrozen {
			t.Fatalf("pooled graph %s is %s, want frozen", in.Grafo.Name, in.Grafo.State)
		}
		for nombre, mc := range in.Maquinas {
			if mc.Labels[api.LabelKind] != plantilla.KindNodoGrafo || mc.Labels[LabelTenant] != "" {
				t.Fatalf("node %s labels: %v", nombre, mc.Labels)
			}
		}
	}
	// Una segunda vuelta no fabrica más: ya están las que pide.
	rellenar(t, daemons)
	if n := f.grafosVivos(); n != 2 {
		t.Fatalf("%d graphs after a second round, want 2", n)
	}
	// Los nodos no son sandboxes: el fondo de máquinas y /v1/sandboxes no los ven.
	srv, _ := montar(t, daemons)
	if sbs := decodifica[[]Sandbox](t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes", nil)); len(sbs) != 0 {
		t.Fatalf("graph nodes leaked into /v1/sandboxes: %+v", sbs)
	}
}

func TestReclamarGrafoDelFondo(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(1))
	daemons := map[string]*falso{"a": f}
	rellenar(t, daemons)
	srv, _ := montar(t, daemons)

	resp := pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"})
	esperaCodigo(t, resp, 201)
	g := decodifica[Grafo](t, resp)
	if f.grafosUp != 1 {
		t.Fatalf("%d graph ups, want 1 (the pool's): the claim should not build a new one", f.grafosUp)
	}
	if g.Template != "agente-pg" || g.State != api.GraphStateRunning || len(g.Nodes) != 2 || g.ClaimedAt == nil {
		t.Fatalf("claimed graph: %+v", g)
	}
	agente := g.Nodes["agent"].ID
	if agente == "" {
		t.Fatalf("node agent has no id: %+v", g.Nodes)
	}
	for _, n := range g.Nodes {
		_, ref, _ := partirID(n.ID)
		if mc := f.maquina(ref); mc == nil || mc.Labels[LabelTenant] != "alice" || mc.State != api.StateRunning {
			t.Fatalf("node %s after the claim: %+v", n.ID, mc)
		}
	}
	if len(grafosLibres(t, f, "agente-pg")) != 0 {
		t.Fatal("the claimed graph is still free")
	}

	// Es de alice y solo de alice.
	if gs := decodifica[[]Grafo](t, pide(t, srv, tokenAlice, "GET", "/v1/graphs", nil)); len(gs) != 1 || gs[0].ID != g.ID {
		t.Fatalf("alice's graphs: %+v", gs)
	}
	if gs := decodifica[[]Grafo](t, pide(t, srv, tokenBob, "GET", "/v1/graphs", nil)); len(gs) != 0 {
		t.Fatalf("bob sees alice's graphs: %+v", gs)
	}
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/graphs/"+g.ID, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "DELETE", "/v1/graphs/"+g.ID, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/graphs/"+g.ID, nil), 200)

	// Entrega: el nodo admite exec y ficheros de su dueño, nadie más.
	exec := api.ExecRequest{Cmd: []string{"true"}}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+agente+"/exec?wait=1", exec), 200)
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/"+agente+"/exec?wait=1", exec), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes/"+agente+"/files?path=/etc/hostname", nil), 404)
	// Pero no es un sandbox suelto: no se ve, no se renueva ni se borra solo.
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+agente, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/sandboxes/"+agente, nil), 404)

	// Soltarlo borra el grafo entero.
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/graphs/"+g.ID, nil), 204)
	if n := f.grafosVivos(); n != 0 {
		t.Fatalf("%d graphs after the release, want 0", n)
	}
	_, ref, _ := partirID(agente)
	if f.maquina(ref) != nil {
		t.Fatal("the node machine outlived its graph")
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+agente+"/exec?wait=1", exec), 404)
}

func TestGrafoSinFondoSeLevantaYaReclamado(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(0))
	srv, _ := montar(t, map[string]*falso{"a": f})

	resp := pide(t, srv, tokenBob, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"})
	esperaCodigo(t, resp, 201)
	g := decodifica[Grafo](t, resp)
	if f.grafosUp != 1 || g.State != api.GraphStateRunning {
		t.Fatalf("ups %d, graph %+v", f.grafosUp, g)
	}
	for _, n := range g.Nodes {
		_, ref, _ := partirID(n.ID)
		if mc := f.maquina(ref); mc == nil || mc.Labels[LabelTenant] != "bob" {
			t.Fatalf("node %s: %+v", n.ID, mc)
		}
	}
}

func TestGrafoPeticionesMalas(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	esperaCodigo(t, pide(t, srv, "", "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 401)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", `{}`), 400)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", `{"template":"x","labels":{"tenant":"bob"}}`), 400)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", `{"template":"../x"}`), 400)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/graphs/a/nada", nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/graphs/zz/nada", nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "PUT", "/v1/graphs/a/nada", nil), 405)
}

func TestGrafoQueNoDespiertaNoVuelveAlFondo(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(1))
	daemons := map[string]*falso{"a": f}
	rellenar(t, daemons)
	viejo := grafosLibres(t, f, "agente-pg")[0].Grafo.ID
	f.mu.Lock()
	f.thawFalla = true
	f.mu.Unlock()
	srv, _ := montar(t, daemons)

	// El reclamado no despierta: se borra y se levanta uno nuevo.
	resp := pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"})
	esperaCodigo(t, resp, 201)
	g := decodifica[Grafo](t, resp)
	f.mu.Lock()
	_, sigue := f.grafos[viejo]
	f.mu.Unlock()
	if sigue {
		t.Fatal("the graph that failed to thaw is still there")
	}
	if g.ID == "a/"+viejo || f.grafosUp != 2 {
		t.Fatalf("expected a new graph; got %s after %d ups", g.ID, f.grafosUp)
	}
}

func TestGrafoCuentaEnLaCuota(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(0))
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 2, MaxPorPlantilla: 1})

	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 201)
	// Por plantilla: un segundo grafo de la misma no.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}), 429)
	// En el total: el grafo cuenta como un sandbox.
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 429)
}

// Un grafo mezclado (dos reclamaciones chocaron) no es de nadie: no se
// reclama, no se ve, y la limpieza lo borra a la segunda vuelta que lo ve.
func TestGrafoMezcladoNoEsDeNadieYSeLimpia(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(1))
	daemons := map[string]*falso{"a": f}
	rellenar(t, daemons)
	in := grafosLibres(t, f, "agente-pg")[0]
	f.mu.Lock()
	f.maquinas[in.Maquinas["db"].ID].Labels[LabelTenant] = "bob"
	f.grafos[in.Grafo.ID].CreatedAt = time.Now().Add(-time.Hour)
	f.mu.Unlock()
	srv, s := montar(t, daemons)

	if gs := decodifica[[]Grafo](t, pide(t, srv, tokenBob, "GET", "/v1/graphs", nil)); len(gs) != 0 {
		t.Fatalf("bob owns a mixed graph: %+v", gs)
	}
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/graphs/a/"+in.Grafo.ID, nil), 404)
	// Aunque la máquina de db lleve su etiqueta, el grafo no es suyo: nada de
	// exec en ella.
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/a/"+in.Maquinas["db"].ID+"/exec?wait=1",
		api.ExecRequest{Cmd: []string{"true"}}), 404)
	s.Limpiar(context.Background())
	if f.grafosVivos() != 1 {
		t.Fatal("the first sweep must only mark a broken graph")
	}
	s.Limpiar(context.Background())
	if f.grafosVivos() != 0 {
		t.Fatal("the second sweep must remove the broken graph")
	}
}

func TestLimpiarGrafoAbandonado(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(0))
	srv, s := montar(t, map[string]*falso{"a": f})
	viejo := decodifica[Grafo](t, pide(t, srv, tokenAlice, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}))
	nuevo := decodifica[Grafo](t, pide(t, srv, tokenBob, "POST", "/v1/graphs", CrearGrafoPeticion{Template: "agente-pg"}))

	hace := time.Now().Add(-30 * time.Hour)
	f.mu.Lock()
	for _, n := range viejo.Nodes {
		_, ref, _ := partirID(n.ID)
		mc := f.maquinas[ref]
		mc.StartedAt, mc.CreatedAt = &hace, hace
		mc.Labels[plantilla.EtiquetaReclamado] = strconv.FormatInt(hace.Unix(), 10)
	}
	f.mu.Unlock()

	s.Limpiar(context.Background())
	_, gid, _ := partirID(viejo.ID)
	_, gnuevo, _ := partirID(nuevo.ID)
	f.mu.Lock()
	_, sigueViejo := f.grafos[gid]
	_, sigueNuevo := f.grafos[gnuevo]
	f.mu.Unlock()
	if sigueViejo || !sigueNuevo {
		t.Fatalf("after the sweep: abandoned still there=%v, recent still there=%v", sigueViejo, sigueNuevo)
	}
}

func TestTemplatesIncluyeLosDeGrafo(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(0))
	srv, _ := montar(t, map[string]*falso{"a": f})
	tpls := decodifica[[]TemplateInfo](t, pide(t, srv, tokenAlice, "GET", "/v1/templates", nil))
	if len(tpls) != 1 || tpls[0].Name != "agente-pg" || tpls[0].Kind != plantilla.KindGrafo || len(tpls[0].Hosts) != 1 {
		t.Fatalf("templates: %+v", tpls)
	}
}

// Si no cabe, el fondo no deja nada; si no llega a congelarse, lo borra: una
// instancia a medias nunca queda en el fondo.
func TestPoolNoDejaGrafosAMedias(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(2))
	daemons := map[string]*falso{"a": f}

	f.mu.Lock()
	f.grafoUpCodigo = api.StatusInsufficientMemory
	f.mu.Unlock()
	rellenar(t, daemons)
	if n := f.grafosVivos(); n != 0 {
		t.Fatalf("%d graphs without room", n)
	}

	f.mu.Lock()
	f.grafoUpCodigo, f.freezeFalla = 0, true
	f.mu.Unlock()
	rellenar(t, daemons)
	if n := f.grafosVivos(); n != 0 || f.grafosUp == 0 {
		t.Fatalf("%d graphs left after a failed freeze (%d ups)", n, f.grafosUp)
	}
}

// Un thaw lento no para otra reclamación: el candado del frontal solo cubre
// etiquetar y comprobar, y las etiquetas ya reservan la instancia, así que las
// dos se llevan grafos distintos.
func TestThawLentoNoBloqueaOtraReclamacion(t *testing.T) {
	f := nuevoFalso(t)
	guardarGrafo(t, f, plantillaAgentePG(2))
	daemons := map[string]*falso{"a": f}
	rellenar(t, daemons)
	_, s := montar(t, daemons)

	espera, empezado := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.thawEspera, f.thawEmpezado = espera, empezado
	f.mu.Unlock()
	sirve := func(*hosts.Host) bool { return true }
	ctx := context.Background()

	type res struct {
		in plantilla.Instancia
		ok bool
	}
	lenta := make(chan res, 1)
	go func() {
		_, in, ok := s.reclamarGrafo(ctx, &Tenant{Nombre: "alice"}, "agente-pg", sirve)
		lenta <- res{in, ok}
	}()
	select {
	case <-empezado:
	case <-time.After(10 * time.Second):
		t.Fatal("the first claim never reached its thaw")
	}

	rapida := make(chan res, 1)
	go func() {
		_, in, ok := s.reclamarGrafo(ctx, &Tenant{Nombre: "bob"}, "agente-pg", sirve)
		rapida <- res{in, ok}
	}()
	var b res
	select {
	case b = <-rapida:
	case <-time.After(5 * time.Second):
		close(espera)
		t.Fatal("a slow thaw blocked another claim")
	}
	close(espera)
	a := <-lenta
	if !a.ok || !b.ok {
		t.Fatalf("claims: alice ok=%v, bob ok=%v", a.ok, b.ok)
	}
	if a.in.Grafo.ID == b.in.Grafo.ID {
		t.Fatalf("both claims got graph %s", a.in.Grafo.ID)
	}
	if !a.in.De("alice") || !b.in.De("bob") {
		t.Fatalf("owners: %q and %q", a.in.Tenant, b.in.Tenant)
	}
	if f.grafosUp != 2 {
		t.Fatalf("%d graph ups, want 2 (the pool's)", f.grafosUp)
	}
}
