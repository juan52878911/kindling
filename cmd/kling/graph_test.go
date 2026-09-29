package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El grafo del diseño (docs/grafos-diseno.md), tal cual, en YAML.
const tiendaYAML = `# mi tienda de prueba
name: tienda
nodes:
  db:    {from: pg16-golden, ports: [5432], wake: lazy, idle_freeze: 120}
  api:   {from: api-node, ports: [8081], egress: allowlist, allow_domains: [api.stripe.com]}
  web:   {from: web-static, ports: [80]}
edges:
  - {from: api, to: db, kind: credential, port: 5432, user: app, database: shop, env: PGPASSWORD, secret_env: SHOP_PG_PASS}
  - {from: web, to: api, kind: link, port: 8081}
`

// Y lo mismo en JSON.
const tiendaJSON = `{
  "name": "tienda",
  "nodes": {
    "db":  {"from": "pg16-golden", "ports": [5432], "wake": "lazy", "idle_freeze": 120},
    "api": {"from": "api-node", "ports": [8081], "egress": "allowlist", "allow_domains": ["api.stripe.com"]},
    "web": {"from": "web-static", "ports": [80]}
  },
  "edges": [
    {"from": "api", "to": "db", "kind": "credential", "port": 5432, "user": "app", "database": "shop", "env": "PGPASSWORD", "secret_env": "SHOP_PG_PASS"},
    {"from": "web", "to": "api", "kind": "link", "port": 8081}
  ]
}`

func sinStdin(e fileEdge, unica bool) (string, error) {
	return leerClaveDe("/nada", strings.NewReader(""), true)(e, unica)
}

func TestLeerGrafoYAMLyJSONIguales(t *testing.T) {
	t.Setenv("SHOP_PG_PASS", "s3cret")
	y, err := leerGrafo("tienda.yaml", []byte(tiendaYAML), sinStdin)
	if err != nil {
		t.Fatal(err)
	}
	j, err := leerGrafo("tienda.json", []byte(tiendaJSON), sinStdin)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(y, j) {
		t.Fatalf("YAML y JSON difieren:\n%+v\n%+v", y, j)
	}
	if y.Secrets["api/PGPASSWORD"] != "s3cret" || len(y.Secrets) != 1 {
		t.Fatalf("claves: %v", y.Secrets)
	}
	db := y.Graph.Nodes["db"]
	if db.Wake != api.GraphWakeLazy || db.IdleFreezeSeconds != 120 || db.Ports[0] != 5432 || y.Graph.Nodes["api"].AllowDomains[0] != "api.stripe.com" {
		t.Fatalf("nodos: %+v", y.Graph.Nodes)
	}
	// La clave no va en el grafo: ni en la arista ni al serializarlo.
	b, _ := json.Marshal(y.Graph)
	if bytes.Contains(b, []byte("s3cret")) {
		t.Fatal("la clave va dentro del grafo")
	}
	// Sin extensión: JSON si empieza por '{', YAML si no.
	if _, err := leerGrafo("tienda", []byte(tiendaYAML), sinStdin); err != nil {
		t.Fatalf("YAML sin extensión: %v", err)
	}
}

func TestLeerGrafoClaves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pg.pass"), []byte("desde-fichero\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := strings.Replace(tiendaYAML, "secret_env: SHOP_PG_PASS", "secret_file: pg.pass", 1)
	req, err := leerGrafo(filepath.Join(dir, "g.yaml"), []byte(base), leerClaveDe(dir, strings.NewReader(""), true))
	if err != nil || req.Secrets["api/PGPASSWORD"] != "desde-fichero" {
		t.Fatalf("secret_file: %v %v", req.Secrets, err)
	}
	// La única sin fuente, por stdin (si no es una terminal).
	sin := strings.Replace(tiendaYAML, ", secret_env: SHOP_PG_PASS", "", 1)
	req, err = leerGrafo("g.yaml", []byte(sin), leerClaveDe(dir, strings.NewReader("de-stdin\n"), false))
	if err != nil || req.Secrets["api/PGPASSWORD"] != "de-stdin" {
		t.Fatalf("stdin: %v %v", req.Secrets, err)
	}
	// En una terminal no se lee stdin: se pide la fuente.
	if _, err := leerGrafo("g.yaml", []byte(sin), leerClaveDe(dir, strings.NewReader("x"), true)); err == nil || !strings.Contains(err.Error(), "secret_env") {
		t.Fatalf("sin fuente en una terminal: %v", err)
	}
	// Variable vacía: error, no una clave vacía.
	t.Setenv("SHOP_PG_PASS", "")
	if _, err := leerGrafo("g.yaml", []byte(tiendaYAML), sinStdin); err == nil || !strings.Contains(err.Error(), "SHOP_PG_PASS") {
		t.Fatalf("variable vacía: %v", err)
	}
}

func TestLeerGrafoRechaza(t *testing.T) {
	t.Setenv("SHOP_PG_PASS", "x")
	casos := map[string]string{
		"campo desconocido":         strings.Replace(tiendaYAML, "wake: lazy", "wake: lazy, wakey: 1", 1),
		"clave en un link":          strings.Replace(tiendaYAML, "port: 8081}", "port: 8081, secret_env: X}", 1),
		"validación del grafo":      strings.Replace(tiendaYAML, "port: 8081}", "port: 9999}", 1),
		"campo del daemon es texto": strings.Replace(tiendaYAML, "ports: [80]", "ports: [ochenta]", 1),
	}
	for nombre, doc := range casos {
		if _, err := leerGrafo("g.yaml", []byte(doc), sinStdin); err == nil {
			t.Errorf("%s: aceptado", nombre)
		}
	}
}

func TestParseYAML(t *testing.T) {
	doc := `---
# comentario
name: "con # dentro"   # y fuera
lista:
- uno
- 'dos ''tres'''
- 3
mapa:
  vacio:
  bool: true
  nulo: ~
  url: http://example.org/a#b
anidada:
  - from: a
    to: b
  - - x
    - y
  - {a: [1, {b: c}], d: "e"}
`
	v, err := parseYAML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	want := `{"anidada":[{"from":"a","to":"b"},["x","y"],{"a":[1,{"b":"c"}],"d":"e"}],"lista":["uno","dos 'tres'",3],` +
		`"mapa":{"bool":true,"nulo":null,"url":"http://example.org/a#b","vacio":null},"name":"con # dentro"}`
	if string(b) != want {
		t.Fatalf("\n got %s\nwant %s", b, want)
	}
}

func TestParseYAMLRechaza(t *testing.T) {
	casos := map[string]string{
		"tabulador":        "a:\n\tb: 1\n",
		"ancla":            "a: &x 1\n",
		"alias":            "a: *x\n",
		"bloque literal":   "a: |\n  texto\n",
		"dos documentos":   "a: 1\n---\nb: 2\n",
		"sangría rara":     "a: 1\n  b: 2\n",
		"clave repetida":   "a: 1\na: 2\n",
		"flujo sin cerrar": "a: [1, 2\n",
		"comilla abierta":  "a: \"x\n",
		"sin dos puntos":   "a: 1\nb\n",
		"vacío":            "# nada\n",
	}
	for nombre, doc := range casos {
		if _, err := parseYAML([]byte(doc)); err == nil {
			t.Errorf("%s: aceptado", nombre)
		}
	}
}

// graph up contra un daemon falso: lo que viaja es el grafo y la clave en
// Secrets; lo que se imprime no lleva la clave.
func TestGraphUpContraDaemonFalso(t *testing.T) {
	t.Setenv("SHOP_PG_PASS", "s3cret")
	req, err := leerGrafo("tienda.yaml", []byte(tiendaYAML), sinStdin)
	if err != nil {
		t.Fatal(err)
	}
	var recibido api.GraphRequest
	mux := fakeSnapshotsMux()
	mux.HandleFunc("POST /graphs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&recibido)
		g := recibido.Graph
		g.ID = "0123456789abcdef"
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(g)
	})
	c := fakeDaemon(t, mux)
	g, err := c.GraphUp(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if recibido.Secrets["api/PGPASSWORD"] != "s3cret" || len(recibido.Graph.Edges) != 2 {
		t.Fatalf("petición: %+v", recibido)
	}
	var out bytes.Buffer
	escribirGrafo(&out, g)
	if strings.Contains(out.String(), "s3cret") || !strings.Contains(out.String(), "web -> api.graph:8081") {
		t.Fatalf("inspect:\n%s", out.String())
	}
	// Un daemon sin la capacidad: el 404 de la ruta se explica.
	c2 := fakeDaemon(t, http.NewServeMux())
	_, err = c2.GraphUp(t.Context(), req)
	var h *errWithHint
	if !errors.As(errSinGrafos(err), &h) || !strings.Contains(h.hint, "graphs") {
		t.Fatalf("daemon viejo: %v", err)
	}
}

// Las aristas share y depends se leen del fichero y se ven en inspect.
func TestLeerGrafoShareYDepends(t *testing.T) {
	const f = `name: taller
nodes:
  files:  {image: min, ports: [8081]}
  web:    {image: min}
edges:
  - {from: web, to: files, kind: share, mount: /data}
  - {from: web, to: files, kind: depends, port: 8081}
`
	req, err := leerGrafo("taller.yaml", []byte(f), sinStdin)
	if err != nil {
		t.Fatal(err)
	}
	g := req.Graph
	if err := api.ValidateGraph(&g); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	escribirGrafo(&out, &g)
	for _, want := range []string{"web -> files  share    /data (ro)", "web -> files  depends  waits until port 8081 answers"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inspect sin %q:\n%s", want, out.String())
		}
	}
	// Una fuente de clave en una arista que no es credential no vale.
	mal := strings.Replace(f, "mount: /data}", "mount: /data, secret_env: X}", 1)
	if _, err := leerGrafo("taller.yaml", []byte(mal), sinStdin); err == nil {
		t.Fatal("secret_env en una arista share")
	}
}
