package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

func envOptsT(name string) envOpts {
	return envOpts{name: name, appTpl: "app-tpl", golden: "pg", owner: defaultOwner, allow: []string{"api.example.com"}, appPort: 8081}
}

func envApp(t *testing.T) *testApp {
	t.Helper()
	ta := newTestApp(t)
	ta.f.snaps["app-tpl"] = &api.Snapshot{Name: "app-tpl"}
	return ta
}

// passwordFiles cuenta las claves guardadas en el host.
func passwordFiles(t *testing.T) int {
	t.Helper()
	d, _ := dbstate.Dir()
	ents, _ := os.ReadDir(filepath.Join(d, dbstate.CopiesDir))
	return len(ents)
}

func TestEnvUpCreaGrafoYPreparaBase(t *testing.T) {
	ta := envApp(t)
	g, err := ta.envUp(context.Background(), envOptsT("tienda"))
	if err != nil {
		t.Fatal(err)
	}
	db := ta.f.find("tienda-db")
	if db == nil || db.ID != g.Nodes["db"].MachineID {
		t.Fatalf("no database machine: %+v", db)
	}
	if db.Labels[labelState] != stateReady || db.Labels[labelOwner] != defaultOwner || db.Labels[labelGolden] != "pg" {
		t.Errorf("labels: %v", db.Labels)
	}
	// La clave del host es la que recibió el daemon por stdin.
	pw, err := dbstate.ReadPassword(db.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ta.f.graphSecrets["tienda"]; got != pw {
		t.Errorf("the daemon got a different key than the host stored")
	}
	if ta.f.verifier[db.ID] == "" {
		t.Error("no verifier set in the guest")
	}
	if err := checkReady(ta.f.find(db.ID), defaultOwner); err != nil {
		t.Errorf("checkReady: %v", err)
	}
	// La arista es la credential del attach.
	if len(g.Edges) != 1 || g.Edges[0].Kind != api.GraphEdgeCredential || g.Edges[0].To != "db" || g.Edges[0].From != "app" {
		t.Errorf("edges: %+v", g.Edges)
	}
	// Ni la clave ni su verificador aparecen en el fichero del grafo ni en argv.
	for _, f := range ta.f.graphFiles {
		if strings.Contains(f, pw) || strings.Contains(f, "SCRAM") {
			t.Error("the graph file carries a secret")
		}
	}
	for _, c := range ta.f.calls {
		if strings.Contains(strings.Join(c.args, " "), pw) {
			t.Errorf("the key travelled in argv: %v", c.args)
		}
		// El fichero temporal se borra.
		if len(c.args) > 2 && c.args[0] == "graph" && c.args[1] == "up" {
			if _, err := os.Stat(c.args[2]); err == nil {
				t.Error("the graph file was left behind")
			}
		}
	}
}

func TestEnvUpDeshaceSiFallaLaBase(t *testing.T) {
	ta := envApp(t)
	ta.f.pgDown = true
	if _, err := ta.envUp(context.Background(), envOptsT("tienda")); err == nil {
		t.Fatal("expected error")
	}
	if len(ta.f.graphs) != 0 || len(ta.f.machines) != 0 {
		t.Errorf("left behind: %d graphs, %d machines", len(ta.f.graphs), len(ta.f.machines))
	}
	if n := passwordFiles(t); n != 0 {
		t.Errorf("password files left: %d", n)
	}
}

func TestEnvUpValidaYNoTocaNada(t *testing.T) {
	ta := envApp(t)
	bad := []envOpts{envOptsT("Mal Nombre"), envOptsT("x"), envOptsT("x"), envOptsT("x")}
	bad[1].golden = "no-existe"
	bad[2].appTpl = "-x"
	bad[3].owner = "a b"
	for _, o := range bad {
		if _, err := ta.envUp(context.Background(), o); err == nil {
			t.Errorf("expected error for %+v", o)
		}
	}
	if len(ta.f.graphs) != 0 {
		t.Error("a graph was created")
	}
}

func TestEnvUpFalloDelGrafoNoDejaClave(t *testing.T) {
	ta := envApp(t)
	ta.f.graphUpFails = true
	if _, err := ta.envUp(context.Background(), envOptsT("tienda")); err == nil {
		t.Fatal("expected error")
	}
	if n := passwordFiles(t); n != 0 {
		t.Errorf("password files left: %d", n)
	}
}

func TestEnvDown(t *testing.T) {
	ta := envApp(t)
	g, err := ta.envUp(context.Background(), envOptsT("tienda"))
	if err != nil {
		t.Fatal(err)
	}
	id := g.Nodes["db"].MachineID
	if ok, err := ta.envDown(context.Background(), "tienda", "otro"); err == nil || ok {
		t.Errorf("another owner must not remove it: ok=%v err=%v", ok, err)
	}
	ok, err := ta.envDown(context.Background(), "tienda", defaultOwner)
	if err != nil || !ok {
		t.Fatalf("down: %v %v", ok, err)
	}
	if len(ta.f.graphs) != 0 {
		t.Error("graph still there")
	}
	if _, err := dbstate.ReadPassword(id); err == nil {
		t.Error("password left behind")
	}
	if ok, err := ta.envDown(context.Background(), "tienda", defaultOwner); ok || err != nil {
		t.Errorf("second down: ok=%v err=%v", ok, err)
	}
}

func TestBranchEnvCreaYEsIdempotente(t *testing.T) {
	ta, _ := branchTestApp(t)
	ta.f.snaps["app-tpl"] = &api.Snapshot{Name: "app-tpl"}
	ctx := context.Background()
	if err := ta.branchEnv(ctx, "feat/Login", "app-tpl", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.graphs) != 1 {
		t.Fatalf("graphs: %d", len(ta.f.graphs))
	}
	ups := len(ta.f.graphFiles)
	if err := ta.branchEnv(ctx, "feat/Login", "app-tpl", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.graphFiles) != ups || !strings.Contains(ta.out.String(), "ready") {
		t.Error("second call must not create another graph")
	}
	// Sin golden ni copias: error claro.
	if err := ta.branchEnv(ctx, "otra", "app-tpl", "", defaultOwner); err == nil {
		t.Error("expected error without golden")
	}
	// -rm de la rama quita el entorno.
	if err := ta.branchRm(ctx, "feat/Login", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.graphs) != 0 {
		t.Error("environment survived branch -rm")
	}
	if err := ta.branchRm(ctx, "feat/Login", defaultOwner); err == nil {
		t.Error("second -rm should say the branch has nothing")
	}
}

func TestBranchEnvName(t *testing.T) {
	long := "feature/una-rama-con-un-nombre-larguisimo-y-mas"
	a, b := branchEnvName("abcdef123456", long), branchEnvName("abcdef123456", long+"2")
	if !envNamePattern.MatchString(a) || a == b {
		t.Errorf("names: %s %s", a, b)
	}
	if x, y := branchEnvName("abcdef123456", "feat/x"), branchEnvName("abcdef123456", "feat-x"); x == y {
		t.Errorf("feat/x and feat-x collide: %s", x)
	}
}

// "No existe" sale de la lista de grafos, no del texto de un error: un fallo
// cuyo mensaje dice "404" o "not found" es un error, no un entorno ausente.
func TestGraphOfNoAdivinaPorElTexto(t *testing.T) {
	ta := envApp(t)
	ctx := context.Background()
	if g, err := ta.graphOf(ctx, "nada"); g != nil || err != nil {
		t.Fatalf("grafo ausente: %v %v", g, err)
	}
	if _, err := ta.envUp(ctx, envOptsT("tienda")); err != nil {
		t.Fatal(err)
	}
	if g, err := ta.graphOf(ctx, "tienda"); err != nil || g == nil || g.Name != "tienda" || g.Nodes["db"].MachineID == "" {
		t.Fatalf("grafo presente: %+v %v", g, err)
	}
	if g, err := ta.graphOf(ctx, "tiend"); g != nil || err != nil {
		t.Fatalf("un prefijo no es el grafo: %v %v", g, err)
	}
	for _, msg := range []string{"proxy: 404 not found", "daemon: graph \"tienda\" not found"} {
		ta.f.graphLsFails = errors.New(msg)
		if g, err := ta.graphOf(ctx, "tienda"); err == nil || g != nil {
			t.Fatalf("%q: dio %v %v, quería el error", msg, g, err)
		}
		if ok, err := ta.envDown(ctx, "tienda", defaultOwner); err == nil || ok {
			t.Fatalf("%q: env down siguió (%v %v)", msg, ok, err)
		}
	}
	ta.f.graphLsFails = nil
	if ok, err := ta.envDown(ctx, "tienda", defaultOwner); err != nil || !ok {
		t.Fatalf("env down: %v %v", ok, err)
	}
}
