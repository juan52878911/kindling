package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// grafoAuditado: web -> api (link), api -> db (credential) y un lazy sin
// máquina que no se lee.
func grafoAuditado() *api.Graph {
	return &api.Graph{Name: "tienda", Nodes: map[string]api.GraphNode{
		"web":   {MachineID: "m-web"},
		"api":   {MachineID: "m-api"},
		"db":    {MachineID: "m-db"},
		"cache": {Wake: api.GraphWakeLazy},
	}}
}

func ts(s int) time.Time { return time.Date(2026, 9, 29, 10, 0, s, 0, time.UTC) }

// auditoriaFalsa da a cada máquina sus registros y apunta lo que se pidió.
type auditoriaFalsa struct {
	por     map[string][]api.CredAuditRecord
	fallan  map[string]error
	pedidas []string
	colas   []int
}

func (a *auditoriaFalsa) leer(_ context.Context, ref string, q api.CredAuditQuery) ([]api.CredAuditRecord, error) {
	a.pedidas = append(a.pedidas, ref)
	a.colas = append(a.colas, q.Tail)
	if err := a.fallan[ref]; err != nil {
		return nil, err
	}
	return a.por[ref], nil
}

func nuevaAuditoriaFalsa() *auditoriaFalsa {
	return &auditoriaFalsa{por: map[string][]api.CredAuditRecord{
		"m-web": {
			{TS: ts(3), Kind: "link", Host: "api.graph:8081", Upstream: "machine:m-api", ReqBytes: 10, RespBytes: 20, MS: 4},
			{TS: ts(5), Kind: "link", Host: "db.graph:5432", Reason: "machine_unavailable", Denied: true},
			// Su propio tráfico HTTP con una credencial: no es de una arista.
			{TS: ts(1), Kind: "http", Method: "GET", Host: "api.stripe.com", Path: "/v1/balance", Status: 200, Creds: []string{"STRIPE_KEY"}},
		},
		"m-api": {
			{TS: ts(2), Kind: "postgres", Host: "db.graph", User: "app", Database: "shop", Auth: "scram-sha-256", Creds: []string{"PGPASSWORD"}, Upstream: "machine:0123"},
			{TS: ts(3), Kind: "postgres", Reason: "bad_placeholder", Denied: true},
			{TS: ts(4), Kind: "postgres", Host: "rds.example.com", User: "app", Creds: []string{"OTRA"}},
			{TS: ts(6), Kind: "dropped", Dropped: 2},
		},
	}}
}

// Una línea de tiempo con los registros de las aristas de todos los nodos,
// por tiempo (a igual instante, por nodo), sin leer el lazy sin máquina.
func TestAuditoriaGrafoLineaDeTiempo(t *testing.T) {
	a := nuevaAuditoriaFalsa()
	var avisos bytes.Buffer
	recs, err := auditoriaGrafo(context.Background(), a.leer, grafoAuditado(), api.CredAuditQuery{}, false, 0, &avisos)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.pedidas, ",") != "m-api,m-db,m-web" {
		t.Fatalf("leyó %v", a.pedidas)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Node+"/"+r.Kind+"/"+r.Host)
	}
	want := "api/postgres/db.graph|api/postgres/|web/link/api.graph:8081|web/link/db.graph:5432|api/dropped/"
	if strings.Join(got, "|") != want {
		t.Fatalf("línea de tiempo:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
	if avisos.Len() != 0 {
		t.Fatalf("avisos: %s", avisos.String())
	}
	// Filtrando por arista se lee el registro entero de cada nodo.
	for _, c := range a.colas {
		if c != 0 {
			t.Fatalf("pidió tail %d filtrando", c)
		}
	}

	// -all: también lo demás, y las últimas N del total.
	a = nuevaAuditoriaFalsa()
	recs, err = auditoriaGrafo(context.Background(), a.leer, grafoAuditado(), api.CredAuditQuery{}, true, 3, &avisos)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Host != "rds.example.com" || recs[2].Kind != "dropped" {
		t.Fatalf("-all -tail 3: %+v", recs)
	}
	if a.colas[0] != 3 {
		t.Fatalf("con -all pide la cola de cada nodo: %v", a.colas)
	}
}

// Un nodo que no se puede leer se avisa y no para a los demás; si no se lee
// ninguno, es un error.
func TestAuditoriaGrafoNodoIlegible(t *testing.T) {
	a := nuevaAuditoriaFalsa()
	a.fallan = map[string]error{"m-db": errors.New("machine gone")}
	var avisos bytes.Buffer
	recs, err := auditoriaGrafo(context.Background(), a.leer, grafoAuditado(), api.CredAuditQuery{}, false, 0, &avisos)
	if err != nil || len(recs) == 0 {
		t.Fatalf("%v %v", recs, err)
	}
	if !strings.Contains(avisos.String(), "node db: machine gone") {
		t.Fatalf("avisos: %q", avisos.String())
	}
	a.fallan = map[string]error{"m-db": errors.New("x"), "m-web": errors.New("x"), "m-api": errors.New("x")}
	if _, err := auditoriaGrafo(context.Background(), a.leer, grafoAuditado(), api.CredAuditQuery{}, false, 0, &avisos); err == nil {
		t.Fatal("sin ningún registro legible no dio error")
	}
	// Un grafo sin máquinas (todo lazy) no es un error: no hay nada.
	g := &api.Graph{Nodes: map[string]api.GraphNode{"a": {Wake: api.GraphWakeLazy}}}
	if recs, err := auditoriaGrafo(context.Background(), a.leer, g, api.CredAuditQuery{}, false, 0, &avisos); err != nil || len(recs) != 0 {
		t.Fatalf("grafo sin máquinas: %v %v", recs, err)
	}
}

// La tabla lleva el nodo; el JSON, "node" junto a los campos de siempre; los
// descartados, a stderr; y nada de lo que el invitado eligió llega crudo al
// terminal.
func TestEscribirAuditoriaGrafo(t *testing.T) {
	recs := []registroGrafo{
		{Node: "web", CredAuditRecord: api.CredAuditRecord{TS: ts(1), Kind: "link", Host: "api.graph:8081", Upstream: "machine:m-api", MS: 3}},
		{Node: "api", CredAuditRecord: api.CredAuditRecord{TS: ts(2), Kind: "postgres", Host: "db.graph", User: "app", Database: "shop", Creds: []string{"PGPASSWORD"}, Reason: "not_allowed", Denied: true}},
		{Node: "api", CredAuditRecord: api.CredAuditRecord{TS: ts(3), Kind: "dropped", Dropped: 4}},
		{Node: "web", CredAuditRecord: api.CredAuditRecord{TS: ts(4), Kind: "link", Host: "api.graph:8081\x1b[2J"}},
	}
	var out, errOut bytes.Buffer
	if err := escribirAuditoriaGrafo(&out, &errOut, recs, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "NODE") {
		t.Fatalf("tabla:\n%s", out.String())
	}
	if !strings.Contains(lines[1], " web ") || !strings.Contains(lines[1], "LINK") || !strings.Contains(lines[1], "machine:m-api") || !strings.HasSuffix(lines[1], " ok") {
		t.Errorf("fila link: %q", lines[1])
	}
	if !strings.Contains(lines[2], " api ") || !strings.Contains(lines[2], "app@shop") || !strings.HasSuffix(lines[2], "DENIED(not_allowed)") {
		t.Errorf("fila credential: %q", lines[2])
	}
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Error("un carácter de control llegó a la tabla")
	}
	if errOut.String() != "node api: dropped 4 records\n" {
		t.Errorf("stderr: %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := escribirAuditoriaGrafo(&out, &errOut, recs[:2], true); err != nil {
		t.Fatal(err)
	}
	var primera map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &primera); err != nil {
		t.Fatal(err)
	}
	if primera["node"] != "web" || primera["kind"] != "link" || primera["host"] != "api.graph:8081" || primera["ts"] == nil {
		t.Fatalf("JSON: %v", primera)
	}
}

func TestDeArista(t *testing.T) {
	for _, c := range []struct {
		r    api.CredAuditRecord
		want bool
	}{
		{api.CredAuditRecord{Kind: "link"}, true},
		{api.CredAuditRecord{Kind: "dropped"}, true},
		{api.CredAuditRecord{Kind: "postgres", Host: "db.graph"}, true},
		{api.CredAuditRecord{Kind: "mysql", Host: "db.graph"}, true},
		{api.CredAuditRecord{Kind: "postgres"}, true},
		{api.CredAuditRecord{Kind: "postgres", Host: "db.example.com"}, false},
		{api.CredAuditRecord{Kind: "postgres", Host: "graph"}, false},
		{api.CredAuditRecord{Kind: "http", Host: "x.graph"}, false},
	} {
		if got := deArista(c.r); got != c.want {
			t.Errorf("%+v: %v", c.r, got)
		}
	}
}
