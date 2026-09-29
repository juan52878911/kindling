package plantilla

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func grafoBueno() PlantillaGrafo {
	return PlantillaGrafo{
		Kind: KindGrafo, Nombre: "agente-pg", Pool: 2,
		Nodes: map[string]api.GraphNode{
			"agent": {From: "sbx-agent", AllowExec: true, Labels: map[string]string{"equipo": "x"}},
			"db":    {From: "pg16-golden", Ports: []int{5432}},
		},
		Edges: []api.GraphEdge{{From: "agent", To: "db", Kind: api.GraphEdgeLink, Port: 5432}},
	}
}

func TestValidarGrafo(t *testing.T) {
	p := grafoBueno()
	if err := ValidarGrafo(&p); err != nil {
		t.Fatalf("a good template: %v", err)
	}
	if p.Nodes["db"].Wake != api.GraphWakeEager {
		t.Fatalf("wake was not normalized: %+v", p.Nodes["db"])
	}

	casos := []struct {
		nombre string
		tocar  func(*PlantillaGrafo)
		dice   string
	}{
		{"sin kind", func(p *PlantillaGrafo) { p.Kind = "" }, "kind"},
		{"nombre malo", func(p *PlantillaGrafo) { p.Nombre = "../x" }, "name"},
		{"pool enorme", func(p *PlantillaGrafo) { p.Pool = MaxPoolGrafo + 1 }, "pool"},
		{"sin nodos", func(p *PlantillaGrafo) { p.Nodes = nil }, "no nodes"},
		{"lazy", func(p *PlantillaGrafo) {
			n := p.Nodes["db"]
			n.Wake = api.GraphWakeLazy
			p.Nodes["db"] = n
		}, "lazy"},
		{"credential", func(p *PlantillaGrafo) {
			p.Edges = []api.GraphEdge{{From: "agent", To: "db", Kind: api.GraphEdgeCredential, Env: "PGPASSWORD", User: "app", Database: "app"}}
		}, "credential"},
		{"volumen", func(p *PlantillaGrafo) {
			n := p.Nodes["agent"]
			n.Volumes = []api.VolumeAttachment{{Name: "datos"}}
			p.Nodes["agent"] = n
		}, "volumes"},
		{"tenant en etiqueta", func(p *PlantillaGrafo) { p.Nodes["agent"].Labels["tenant"] = "bob" }, "reserved"},
		{"kind en etiqueta", func(p *PlantillaGrafo) { p.Nodes["agent"].Labels["kind"] = "sandbox" }, "reserved"},
		{"kling. en etiqueta", func(p *PlantillaGrafo) { p.Nodes["agent"].Labels["kling.owner"] = "bob" }, "reserved"},
		{"arista al agente", func(p *PlantillaGrafo) { p.Edges[0].Port = api.GuestPort }, "guest agent"},
	}
	for _, c := range casos {
		p := grafoBueno()
		c.tocar(&p)
		err := ValidarGrafo(&p)
		if err == nil || !strings.Contains(err.Error(), c.dice) {
			t.Errorf("%s: %v, want an error about %q", c.nombre, err, c.dice)
		}
	}
}

func TestEsGrafo(t *testing.T) {
	if !EsGrafo([]byte(`{"kind":"graph","name":"x"}`)) || EsGrafo([]byte(`{"name":"x","image":"base"}`)) || EsGrafo([]byte(`{`)) {
		t.Fatal("EsGrafo")
	}
}

func TestPeticionGrafoEtiquetaCadaNodo(t *testing.T) {
	p := grafoBueno()
	req := PeticionGrafo(p, NombreInstancia(), map[string]string{EtiquetaTenant: "alice"})
	if err := api.ValidateGraph(&req.Graph); err != nil {
		t.Fatalf("the request is not a valid graph: %v", err)
	}
	for nombre, n := range req.Graph.Nodes {
		if n.Labels[api.LabelKind] != KindNodoGrafo || n.Labels[EtiquetaPlantilla] != "agente-pg" || n.Labels[EtiquetaTenant] != "alice" {
			t.Fatalf("node %s labels: %v", nombre, n.Labels)
		}
	}
	// La plantilla no se toca: sus nodos no heredan el tenant de una instancia.
	if _, ok := p.Nodes["agent"].Labels[EtiquetaTenant]; ok {
		t.Fatal("PeticionGrafo wrote into the template's labels")
	}
	if a, b := NombreInstancia(), NombreInstancia(); a == b || !strings.HasPrefix(a, PrefijoGrafo) || len(a) > 24 {
		t.Fatalf("instance names %q %q", a, b)
	}
}

func TestInstancias(t *testing.T) {
	maq := func(id, gid, nodo, tpl, tenant string) *api.Machine {
		l := map[string]string{api.LabelKind: KindNodoGrafo, api.LabelGraph: gid, api.LabelGraphNode: nodo, EtiquetaPlantilla: tpl}
		if tenant != "" {
			l[EtiquetaTenant] = tenant
		}
		return &api.Machine{ID: id, Labels: l}
	}
	grafo := func(id, nombre string, nodos map[string]string) *api.Graph {
		g := &api.Graph{ID: id, Name: nombre, Nodes: map[string]api.GraphNode{}}
		for n, mid := range nodos {
			g.Nodes[n] = api.GraphNode{MachineID: mid}
		}
		return g
	}
	gs := []*api.Graph{
		grafo("g1", "sbxg-1", map[string]string{"a": "m1", "b": "m2"}), // libre
		grafo("g2", "sbxg-2", map[string]string{"a": "m3", "b": "m4"}), // de alice
		grafo("g3", "sbxg-3", map[string]string{"a": "m5", "b": "m6"}), // mezclado
		grafo("g4", "sbxg-4", map[string]string{"a": "m7", "b": "nada"}),
		grafo("g5", "sbxg-5", map[string]string{"a": "m9"}), // su máquina dice otro grafo
		grafo("g6", "mio", map[string]string{"a": "m10"}),   // no es del fondo
	}
	ms := []*api.Machine{
		maq("m1", "g1", "a", "t", ""), maq("m2", "g1", "b", "t", ""),
		maq("m3", "g2", "a", "t", "alice"), maq("m4", "g2", "b", "t", "alice"),
		maq("m5", "g3", "a", "t", "alice"), maq("m6", "g3", "b", "t", "bob"),
		maq("m7", "g4", "a", "t", "alice"),
		maq("m9", "g1", "a", "t", ""),
		maq("m10", "g6", "a", "t", ""),
	}
	por := map[string]Instancia{}
	for _, in := range Instancias(gs, ms) {
		por[in.Grafo.ID] = in
	}
	if len(por) != 4 {
		t.Fatalf("instances %v, want g1..g4", por)
	}
	if in := por["g1"]; !in.Libre() || in.Plantilla != "t" || in.Rota() {
		t.Errorf("g1: %+v", in)
	}
	if in := por["g2"]; in.Libre() || !in.De("alice") || in.De("bob") || in.Rota() {
		t.Errorf("g2: %+v", in)
	}
	if in := por["g3"]; !in.Mezclada || in.De("alice") || in.De("bob") || in.Libre() || !in.Rota() {
		t.Errorf("g3: %+v", in)
	}
	if in := por["g4"]; in.Completa || in.Libre() || !in.Rota() || !in.De("alice") {
		t.Errorf("g4: %+v", in)
	}
}
