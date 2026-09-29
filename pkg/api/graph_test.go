package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// tienda es el grafo de los ejemplos: web -> api (link), api -> db
// (credential), db lazy.
func tienda() Graph {
	return Graph{Name: "tienda", Nodes: map[string]GraphNode{
		"db":  {From: "pg16-golden", Ports: []int{5432}, Wake: GraphWakeLazy, IdleFreezeSeconds: 120},
		"api": {From: "api-node", Ports: []int{8081}, Egress: "allowlist", AllowDomains: []string{"api.stripe.com"}},
		"web": {From: "web-static", Ports: []int{80}},
	}, Edges: []GraphEdge{
		{From: "api", To: "db", Kind: GraphEdgeCredential, User: "app", Database: "shop", Env: "PGPASSWORD"},
		{From: "web", To: "api", Kind: GraphEdgeLink, Port: 8081},
	}}
}

func TestValidateGraphBueno(t *testing.T) {
	g := tienda()
	g.ID, g.State, g.Generation = "puesto-desde-fuera", "running", 7
	n := g.Nodes["api"]
	n.MachineID = "0123456789abcdef"
	g.Nodes["api"] = n
	if err := ValidateGraph(&g); err != nil {
		t.Fatal(err)
	}
	// Lo que pone el daemon no se acepta de fuera.
	if g.ID != "" || g.State != "" || g.Generation != 0 || g.Nodes["api"].MachineID != "" {
		t.Fatalf("campos del daemon aceptados: %+v", g)
	}
	// Normaliza: wake por defecto y puerto por defecto de credential.
	if g.Nodes["web"].Wake != GraphWakeEager || g.Edges[0].Port != GraphPGPort {
		t.Fatalf("sin normalizar: %+v %+v", g.Nodes["web"], g.Edges[0])
	}
	if !g.HasNetworkEdges() {
		t.Fatal("tiene aristas entre máquinas")
	}
	if got := g.SortedNodeNames(); strings.Join(got, ",") != "api,db,web" {
		t.Fatalf("orden: %v", got)
	}
	if k := g.Edges[0].Key(); k != "api/PGPASSWORD" {
		t.Fatalf("Key = %q", k)
	}
	if h := g.Edges[1].Host(); h != "api.graph" {
		t.Fatalf("Host = %q", h)
	}
}

func TestValidateGraphRechaza(t *testing.T) {
	casos := map[string]func(g *Graph){
		"nombre con mayúsculas":   func(g *Graph) { g.Name = "Tienda" },
		"nombre largo":            func(g *Graph) { g.Name = strings.Repeat("a", 25) },
		"sin nodos":               func(g *Graph) { g.Nodes, g.Edges = nil, nil },
		"nodo que no es DNS":      func(g *Graph) { g.Nodes["mal_nombre"] = GraphNode{Image: "min"} },
		"nodo sin from ni image":  func(g *Graph) { g.Nodes["x"] = GraphNode{} },
		"nodo con from e image":   func(g *Graph) { g.Nodes["x"] = GraphNode{From: "a", Image: "b"} },
		"wake raro":               func(g *Graph) { n := g.Nodes["db"]; n.Wake = "sometimes"; g.Nodes["db"] = n },
		"egress raro":             func(g *Graph) { n := g.Nodes["db"]; n.Egress = "lan"; g.Nodes["db"] = n },
		"puerto fuera de rango":   func(g *Graph) { n := g.Nodes["db"]; n.Ports = []int{70000}; g.Nodes["db"] = n },
		"puerto repetido":         func(g *Graph) { n := g.Nodes["db"]; n.Ports = []int{5432, 5432}; g.Nodes["db"] = n },
		"shares con plantilla":    func(g *Graph) { n := g.Nodes["db"]; n.Shares = []ShareSpec{{Mount: "/x"}}; g.Nodes["db"] = n },
		"etiqueta de grafo":       func(g *Graph) { n := g.Nodes["db"]; n.Labels = map[string]string{LabelGraph: "x"}; g.Nodes["db"] = n },
		"etiqueta kling.ports":    func(g *Graph) { n := g.Nodes["db"]; n.Labels = map[string]string{LabelPorts: "1"}; g.Nodes["db"] = n },
		"arista a nodo que falta": func(g *Graph) { g.Edges[1].To = "cache" },
		"arista desde nodo falta": func(g *Graph) { g.Edges[1].From = "cache" },
		"arista a sí mismo":       func(g *Graph) { g.Edges[1].To = "web"; g.Nodes["web"] = GraphNode{Image: "x", Ports: []int{8081}} },
		"puerto no expuesto":      func(g *Graph) { g.Edges[1].Port = 9090 },
		"link al 53":              func(g *Graph) { g.Edges[1].Port = 53; n := g.Nodes["api"]; n.Ports = []int{53}; g.Nodes["api"] = n },
		"link al puerto del agente": func(g *Graph) {
			g.Edges[1].Port = GuestPort
			n := g.Nodes["api"]
			n.Ports = []int{GuestPort}
			g.Nodes["api"] = n
		},
		"credential al puerto del agente": func(g *Graph) {
			n := g.Nodes["db"]
			n.Ports = append(n.Ports, GuestPort)
			g.Nodes["db"] = n
			for i := range g.Edges {
				if g.Edges[i].Kind == GraphEdgeCredential {
					g.Edges[i].Port = GuestPort
				}
			}
		},
		"link al 443":             func(g *Graph) { g.Edges[1].Port = 443; n := g.Nodes["api"]; n.Ports = []int{443}; g.Nodes["api"] = n },
		"link con env":            func(g *Graph) { g.Edges[1].Env = "X" },
		"credential sin env":      func(g *Graph) { g.Edges[0].Env = "" },
		"credential env inválido": func(g *Graph) { g.Edges[0].Env = "pg-pass" },
		"credential sin usuario":  func(g *Graph) { g.Edges[0].User = "" },
		"credential sin base":     func(g *Graph) { g.Edges[0].Database = "" },
		"base con comillas":       func(g *Graph) { g.Edges[0].Database = `shop"; DROP` },
		"tipo desconocido":        func(g *Graph) { g.Edges[1].Kind = "tunnel" },
		"share con puerto":        func(g *Graph) { g.Edges[1].Kind = GraphEdgeShare },
		"mcp como arista":         func(g *Graph) { g.Edges[1].Kind = GraphEdgeMCP },
		"link con mount":          func(g *Graph) { g.Edges[1].Mount = "/data" },
		"arista repetida":         func(g *Graph) { g.Edges = append(g.Edges, g.Edges[1]) },
		"dos aristas al mismo puerto desde un nodo": func(g *Graph) {
			g.Nodes["cache"] = GraphNode{Image: "redis", Ports: []int{8081}}
			g.Edges = append(g.Edges, GraphEdge{From: "web", To: "cache", Kind: GraphEdgeLink, Port: 8081})
		},
		"dos credential con la misma variable": func(g *Graph) {
			g.Nodes["db2"] = GraphNode{Image: "pg", Ports: []int{5433}}
			g.Edges = append(g.Edges, GraphEdge{From: "api", To: "db2", Kind: GraphEdgeCredential, Port: 5433, Env: "PGPASSWORD", User: "a", Database: "b"})
		},
	}
	for nombre, mod := range casos {
		t.Run(nombre, func(t *testing.T) {
			g := tienda()
			mod(&g)
			if err := ValidateGraph(&g); err == nil {
				t.Fatal("aceptado")
			}
		})
	}
}

// mcp no está en esta versión: el mensaje dice por qué (el puente solo
// escucha en el puerto del agente) y qué usar en su lugar.
func TestValidateGraphMCPFueraDeVersion(t *testing.T) {
	g := tienda()
	g.Edges[1].Kind = GraphEdgeMCP
	err := ValidateGraph(&g)
	if err == nil || !strings.Contains(err.Error(), "not in this version") || !strings.Contains(err.Error(), "use a link edge") {
		t.Fatalf("mcp: %v", err)
	}
}

// cadena es a -> b -> c por aristas depends (a depende de b, b de c), con d
// suelto.
func cadena() Graph {
	return Graph{Name: "cadena", Nodes: map[string]GraphNode{
		"a": {Image: "min"},
		"b": {Image: "min", Ports: []int{8081}},
		"c": {Image: "min", Wake: GraphWakeLazy},
		"d": {Image: "min"},
	}, Edges: []GraphEdge{
		{From: "a", To: "b", Kind: GraphEdgeDepends, Port: 8081},
		{From: "b", To: "c", Kind: GraphEdgeDepends},
	}}
}

func TestValidateGraphDepends(t *testing.T) {
	g := cadena()
	if err := ValidateGraph(&g); err != nil {
		t.Fatal(err)
	}
	if g.HasNetworkEdges() {
		t.Fatal("depends no es una arista de red")
	}
	if !g.HasPortDepends() {
		t.Fatal("a -> b espera a un puerto")
	}
	arr, err := g.StartOrder()
	if err != nil || strings.Join(arr, ",") != "c,b,a,d" {
		t.Fatalf("arranque: %v %v", arr, err)
	}
	par, err := g.StopOrder()
	if err != nil || strings.Join(par, ",") != "a,b,c,d" {
		t.Fatalf("parada: %v %v", par, err)
	}
	if d := g.Dependencies("a"); len(d) != 1 || d[0] != "b" {
		t.Fatalf("dependencias de a: %v", d)
	}
	// Sin aristas depends, los dos órdenes son el de los nombres.
	tg := tienda()
	if o, _ := tg.StartOrder(); strings.Join(o, ",") != "api,db,web" {
		t.Fatalf("sin depends: %v", o)
	}
	if o, _ := tg.StopOrder(); strings.Join(o, ",") != "api,db,web" {
		t.Fatalf("sin depends, parada: %v", o)
	}

	casos := map[string]func(g *Graph){
		"ciclo": func(g *Graph) {
			g.Edges = append(g.Edges, GraphEdge{From: "c", To: "a", Kind: GraphEdgeDepends})
		},
		"ciclo de dos": func(g *Graph) {
			g.Edges = append(g.Edges, GraphEdge{From: "b", To: "a", Kind: GraphEdgeDepends})
		},
		"puerto no expuesto":    func(g *Graph) { g.Edges[0].Port = 9090 },
		"puerto del agente":     func(g *Graph) { g.Edges[0].Port = GuestPort },
		"depends con env":       func(g *Graph) { g.Edges[1].Env = "X" },
		"depends con mount":     func(g *Graph) { g.Edges[1].Mount = "/data" },
		"depends a sí mismo":    func(g *Graph) { g.Edges[1].To = "b" },
		"depends a nodo que no": func(g *Graph) { g.Edges[1].To = "z" },
		"depends repetida":      func(g *Graph) { g.Edges = append(g.Edges, g.Edges[1]) },
	}
	for nombre, mod := range casos {
		t.Run(nombre, func(t *testing.T) {
			g := cadena()
			mod(&g)
			err := ValidateGraph(&g)
			if err == nil {
				t.Fatal("aceptado")
			}
			if strings.HasPrefix(nombre, "ciclo") && !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("el ciclo no se nombra: %v", err)
			}
		})
	}
	// Un depends no ocupa el puerto de la dirección del grafo: un link al
	// mismo puerto desde el mismo nodo sigue valiendo.
	g = cadena()
	g.Edges = append(g.Edges, GraphEdge{From: "a", To: "b", Kind: GraphEdgeLink, Port: 8081})
	if err := ValidateGraph(&g); err != nil {
		t.Fatalf("depends y link al mismo puerto: %v", err)
	}
}

// taller es web y worker viendo la carpeta /data de files.
func taller() Graph {
	return Graph{Name: "taller", Nodes: map[string]GraphNode{
		"files":  {Image: "min"},
		"web":    {Image: "min"},
		"worker": {Image: "min"},
	}, Edges: []GraphEdge{
		{From: "web", To: "files", Kind: GraphEdgeShare, Mount: "/data"},
		{From: "worker", To: "files", Kind: GraphEdgeShare, Mount: "/data", Mode: "rw"},
	}}
}

func TestValidateGraphShare(t *testing.T) {
	g := taller()
	if err := ValidateGraph(&g); err != nil {
		t.Fatal(err)
	}
	if g.Edges[0].Mode != "ro" || g.Edges[1].Mode != "rw" {
		t.Fatalf("modo por defecto: %+v", g.Edges)
	}
	if g.HasNetworkEdges() || g.HasPortDepends() {
		t.Fatal("share no es una arista de red")
	}
	casos := map[string]func(g *Graph){
		"con plantilla el que ve": func(g *Graph) { g.Nodes["web"] = GraphNode{From: "tpl"} },
		"con plantilla el dueño":  func(g *Graph) { g.Nodes["files"] = GraphNode{From: "tpl"} },
		"modo copy":               func(g *Graph) { g.Edges[0].Mode = "copy" },
		"modo raro":               func(g *Graph) { g.Edges[0].Mode = "wr" },
		"sin mount":               func(g *Graph) { g.Edges[0].Mount = "" },
		"mount relativo":          func(g *Graph) { g.Edges[0].Mount = "data" },
		"mount en /":              func(g *Graph) { g.Edges[0].Mount = "/" },
		"mount sucio":             func(g *Graph) { g.Edges[0].Mount = "/data/../etc" },
		"con puerto":              func(g *Graph) { g.Edges[0].Port = 5432 },
		"con env":                 func(g *Graph) { g.Edges[0].Env = "X" },
		"repetida":                func(g *Graph) { g.Edges = append(g.Edges, g.Edges[0]) },
		"dos carpetas en la misma ruta": func(g *Graph) {
			// web ya ve /data de files; /data de worker sería otra carpeta.
			g.Edges = append(g.Edges, GraphEdge{From: "web", To: "worker", Kind: GraphEdgeShare, Mount: "/data"})
		},
		"anidadas": func(g *Graph) {
			g.Edges = append(g.Edges, GraphEdge{From: "web", To: "worker", Kind: GraphEdgeShare, Mount: "/data/sub"})
		},
		"pisa un share propio": func(g *Graph) {
			n := g.Nodes["files"]
			n.Shares = []ShareSpec{{Mode: "ro", Mount: "/data", Source: "/srv"}}
			g.Nodes["files"] = n
		},
		"demasiadas": func(g *Graph) {
			for i := 0; i < 8; i++ {
				g.Edges = append(g.Edges, GraphEdge{From: "web", To: "worker", Kind: GraphEdgeShare, Mount: fmt.Sprintf("/m%d", i)})
			}
		},
	}
	for nombre, mod := range casos {
		t.Run(nombre, func(t *testing.T) {
			g := taller()
			mod(&g)
			if err := ValidateGraph(&g); err == nil {
				t.Fatal("aceptado")
			}
		})
	}
	// Dos nodos que ven la carpeta del otro, en rutas distintas: vale.
	g = taller()
	g.Edges = append(g.Edges, GraphEdge{From: "files", To: "web", Kind: GraphEdgeShare, Mount: "/out"})
	if err := ValidateGraph(&g); err != nil {
		t.Fatalf("carpetas cruzadas: %v", err)
	}
}

// Tamaño: 32 nodos y 64 aristas como mucho.
func TestValidateGraphTamaño(t *testing.T) {
	g := Graph{Name: "grande", Nodes: map[string]GraphNode{}}
	for i := 0; i <= GraphMaxNodes; i++ {
		g.Nodes[fmt.Sprintf("n%d", i)] = GraphNode{Image: "min", Ports: []int{1000}}
	}
	if err := ValidateGraph(&g); err == nil || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("33 nodos: %v", err)
	}
	delete(g.Nodes, "n32")
	for i := 0; i <= GraphMaxEdges; i++ {
		g.Edges = append(g.Edges, GraphEdge{From: fmt.Sprintf("n%d", i%32), To: fmt.Sprintf("n%d", (i+1)%32), Kind: GraphEdgeLink, Port: 1000})
	}
	if err := ValidateGraph(&g); err == nil || !strings.Contains(err.Error(), "edges") {
		t.Fatalf("65 aristas: %v", err)
	}
}

// La clave de una arista no se serializa nunca.
func TestGraphEdgeSecretNoSeSerializa(t *testing.T) {
	g := tienda()
	g.Edges[0].Secret = "s3cret"
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "s3cret") {
		t.Fatalf("la clave sale en el JSON: %s", b)
	}
	var otro Graph
	_ = json.Unmarshal([]byte(`{"name":"x","nodes":{},"edges":[{"from":"a","to":"b","kind":"credential","secret":"no"}]}`), &otro)
	if otro.Edges[0].Secret != "" {
		t.Fatal("la clave entra por el JSON del grafo")
	}
}

func TestEtiquetasDeGrafoReservadas(t *testing.T) {
	for _, k := range []string{LabelGraph, LabelGraphNode, "kling.graph.algo"} {
		if !IsGraphLabel(k) || ValidateNoGraphLabels(map[string]string{k: "x"}) == nil {
			t.Errorf("%s no está reservada", k)
		}
		if ValidateForkLabels(map[string]string{k: "x"}) == nil {
			t.Errorf("fork acepta %s", k)
		}
	}
	for _, k := range []string{"kling.graphs", "graph", "kling.db.owner"} {
		if IsGraphLabel(k) {
			t.Errorf("%s no debería estar reservada", k)
		}
	}
}
