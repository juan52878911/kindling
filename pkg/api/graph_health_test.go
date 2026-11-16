package api

import (
	"strings"
	"testing"
	"time"
)

// Health solo en depends, con test, y con exec en el destino.
func TestValidateGraphHealth(t *testing.T) {
	base := func() Graph {
		return Graph{Name: "g", Nodes: map[string]GraphNode{
			"api": {Image: "a"},
			"db":  {Image: "b", Ports: []int{5432}, AllowExec: true},
		}}
	}
	g := base()
	g.Edges = []GraphEdge{{From: "api", To: "db", Kind: GraphEdgeDepends, Health: &GraphHealth{Test: []string{"pg_isready"}}}}
	if err := ValidateGraph(&g); err != nil {
		t.Fatal(err)
	}
	for nombre, c := range map[string]struct {
		e    GraphEdge
		sinX bool
		want string
	}{
		"en link":   {GraphEdge{From: "api", To: "db", Kind: GraphEdgeLink, Port: 5432, Health: &GraphHealth{Test: []string{"x"}}}, false, "only for depends"},
		"sin test":  {GraphEdge{From: "api", To: "db", Kind: GraphEdgeDepends, Health: &GraphHealth{}}, false, "test is empty"},
		"negativo":  {GraphEdge{From: "api", To: "db", Kind: GraphEdgeDepends, Health: &GraphHealth{Test: []string{"x"}, Retries: -1}}, false, "negative"},
		"sin exec":  {GraphEdge{From: "api", To: "db", Kind: GraphEdgeDepends, Health: &GraphHealth{Test: []string{"x"}}}, true, "allow_exec"},
		"demasiado": {GraphEdge{From: "api", To: "db", Kind: GraphEdgeDepends, Health: &GraphHealth{Test: []string{"x"}, TimeoutSeconds: 601}}, false, "out of range"},
	} {
		g := base()
		if c.sinX {
			n := g.Nodes["db"]
			n.AllowExec = false
			g.Nodes["db"] = n
		}
		g.Edges = []GraphEdge{c.e}
		if err := ValidateGraph(&g); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", nombre, err, c.want)
		}
	}
	iv, pl, re, ar, pa := GraphHealth{}.Tiempos()
	if iv != 30*time.Second || pl != 30*time.Second || re != 3 || ar != 0 || pa != 200*time.Millisecond {
		t.Errorf("defaults: %s %s %d %s %s", iv, pl, re, ar, pa)
	}
}

// F12: el paso entre intentos tiene un mínimo; con start_interval_ms: 1 el
// daemon lanzaría un exec por milisegundo.
func TestSaludPasoMinimo(t *testing.T) {
	for _, ms := range []int{1, 50, 99} {
		if _, _, _, _, pa := (GraphHealth{StartIntervalMS: ms}).Tiempos(); pa != GraphHealthMinStartInterval {
			t.Errorf("start_interval_ms %d: paso %s", ms, pa)
		}
	}
	if _, _, _, _, pa := (GraphHealth{StartIntervalMS: 1500}).Tiempos(); pa != 1500*time.Millisecond {
		t.Errorf("start_interval_ms 1500: paso %s", pa)
	}
}
