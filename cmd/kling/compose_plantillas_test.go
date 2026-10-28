package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const composePlantillas = `name: dev
services:
  app:
    image: node:22-alpine
    command: ["node", "/app/server.js"]
    volumes: ["./src:/app"]
    environment: {DB_URL: "postgres://db:5432/x"}
    depends_on: [db]
  db:
    image: postgres:16-alpine
    environment: {POSTGRES_PASSWORD: dev}
    ports: ["5432:5432"]
    healthcheck: {test: ["CMD-SHELL", "pg_isready"]}
  data:
    image: redis:7-alpine
    volumes: ["cache:/data"]
volumes:
  cache: {}
`

func traducirParaPlantillas(t *testing.T, c string) *traduccionCompose {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr, err := traducirComposeEn([]byte(c), "dev", false, sinEntorno, base)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// La clave cambia con todo lo que decide la plantilla, y solo con eso.
func TestClavePlantilla(t *testing.T) {
	tr := traducirParaPlantillas(t, composePlantillas)
	db := tr.por["db"]
	k := clavePlantilla(db, "dev", "v1")
	if k != clavePlantilla(db, "dev", "v1") {
		t.Fatal("the key is not stable")
	}
	otra := *db
	otra.env = [][2]string{{"POSTGRES_PASSWORD", "otra"}}
	otraCmd := *db
	otraCmd.cmd = []string{"postgres", "-c", "fsync=off"}
	otraSalud := *db
	otraSalud.salud = nil
	for nombre, s := range map[string]*servicioCompose{"env": &otra, "command": &otraCmd, "healthcheck": &otraSalud} {
		if clavePlantilla(s, "dev", "v1") == k {
			t.Errorf("changing %s keeps the key", nombre)
		}
	}
	if clavePlantilla(db, "dev", "v2") == k {
		t.Error("another daemon version keeps the key")
	}
	if n := nombrePlantilla(strings.Repeat("p", 60), "nodo", k); len(n) > 64 {
		t.Errorf("template name too long: %q", n)
	}
}

// Los servicios con volúmenes arrancan en frío; los demás, de su plantilla,
// que espera a sus carpetas vivas. El grafo nombra la plantilla.
func TestPlanPlantillas(t *testing.T) {
	tr := traducirParaPlantillas(t, composePlantillas)
	ya := nombrePlantilla("dev", "db", clavePlantilla(tr.por["db"], "dev", "v1"))
	ps := planPlantillas(tr, []*api.Snapshot{{Name: ya}}, "v1", false)
	got := map[string]*plantillaCompose{}
	for _, p := range ps {
		got[p.s.nodo] = p
	}
	if got["data"].nombre != "" {
		t.Errorf("a service with a named volume got a template: %+v", got["data"])
	}
	if !got["db"].existe || got["db"].nombre != ya || got["db"].vivas != 0 {
		t.Errorf("db: %+v", got["db"])
	}
	if got["app"].existe || got["app"].vivas != 1 {
		t.Errorf("app: %+v", got["app"])
	}
	if ps := planPlantillas(tr, []*api.Snapshot{{Name: ya}}, "v1", true); ps[2].existe {
		t.Error("-fresh reused a template")
	}
	tr.desde = map[string]string{"app": "dev-app-x", "db": ya}
	g := grafoDeCompose(tr, "dev", tr.vivos, tr.por, func(string, ...any) {})
	for _, want := range []string{"app: {from: dev-app-x, shares: [", "db: {from: " + ya + ", ports: [5432]}", "data: {image: dev-data"} {
		if !strings.Contains(g, want) {
			t.Errorf("graph lacks %q:\n%s", want, g)
		}
	}
	if _, err := leerGrafo("dev.yaml", []byte(g), nil); err != nil {
		t.Errorf("graph with templates is not valid: %v\n%s", err, g)
	}
	// La orden de import de una plantilla no lleva el entorno; la de un
	// servicio en frío, sí.
	if l := ordenImportCon(tr.por["db"], "dev", false, false); strings.Contains(l, "POSTGRES_PASSWORD") || strings.Contains(l, "-replace") {
		t.Errorf("template import line: %s", l)
	}
	if l := ordenImportCon(tr.por["db"], "dev", true, true); !strings.Contains(l, "-e POSTGRES_PASSWORD=dev") || !strings.Contains(l, "-replace") {
		t.Errorf("cold import line: %s", l)
	}
}

func TestViejasDe(t *testing.T) {
	l := func(p, n string) map[string]string {
		return map[string]string{labelComposeProject: p, labelComposeNode: n}
	}
	snaps := []*api.Snapshot{
		{Name: "dev-db-1", Labels: l("dev", "db")}, {Name: "dev-db-2", Labels: l("dev", "db")},
		{Name: "otro-db-1", Labels: l("otro", "db")}, {Name: "suelta"},
	}
	if got := viejasDe(snaps, "dev", "db", "dev-db-2"); !reflect.DeepEqual(got, []string{"dev-db-1"}) {
		t.Errorf("viejasDe = %v", got)
	}
}

func TestSaludArgv(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{[]string{"CMD", "pg_isready", "-U", "x"}, []string{"pg_isready", "-U", "x"}},
		{[]string{"CMD-SHELL", "redis-cli ping | grep PONG"}, []string{"sh", "-c", "redis-cli ping | grep PONG"}},
		{[]string{"curl -f localhost"}, []string{"sh", "-c", "curl -f localhost"}},
		{[]string{"NONE"}, nil},
		{nil, nil},
	} {
		if got := saludArgv(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("saludArgv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// opsFalsas hace de daemon para prepararPlantilla.
type opsFalsas struct {
	mu      sync.Mutex
	run     api.RunRequest
	execs   [][]string
	fallos  int // execs que acaban con 1 antes de pasar
	commit  api.CommitRequest
	borrada string
}

func (o *opsFalsas) Run(_ context.Context, r api.RunRequest) (*api.Machine, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.run = r
	return &api.Machine{Name: r.Name, Ready: api.ReadyYes}, nil
}

func (o *opsFalsas) Exec(_ context.Context, _ string, r api.ExecRequest, _ func(string, []byte) error) (*api.ExecResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.execs = append(o.execs, r.Cmd)
	if len(o.execs) <= o.fallos {
		return &api.ExecResult{ExitCode: 1}, nil
	}
	return &api.ExecResult{}, nil
}

func (o *opsFalsas) CommitWith(_ context.Context, _ string, r api.CommitRequest) (*api.Snapshot, error) {
	o.commit = r
	return &api.Snapshot{Name: r.Name}, nil
}

func (o *opsFalsas) Remove(_ context.Context, ref string) error {
	o.borrada = ref
	return nil
}

func TestPrepararPlantilla(t *testing.T) {
	old := plantillaPaso
	plantillaPaso = time.Millisecond
	defer func() { plantillaPaso = old }()
	tr := traducirParaPlantillas(t, composePlantillas)
	ps := map[string]*plantillaCompose{}
	for _, p := range planPlantillas(tr, nil, "v1", false) {
		ps[p.s.nodo] = p
	}
	logf := func(string, ...any) {}

	// db: entorno en la máquina, healthcheck dentro hasta que pasa, y guardada
	// esperando a estar lista.
	db := &opsFalsas{fallos: 2}
	p := ps["db"]
	if err := prepararPlantilla(context.Background(), db, "dev", p, logf); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.run.Env, []string{"POSTGRES_PASSWORD=dev"}) || db.run.Image != "dev-db" || !db.run.WaitReady || db.run.ShareWait != 0 {
		t.Errorf("db prep run: %+v", db.run)
	}
	if len(db.execs) != 3 || !reflect.DeepEqual(db.execs[0], []string{"sh", "-c", "pg_isready"}) {
		t.Errorf("db healthcheck execs: %q", db.execs)
	}
	if db.commit.Name != p.nombre || db.commit.SkipReady || db.borrada != p.nombre+"-prep" {
		t.Errorf("db commit %+v, removed %q", db.commit, db.borrada)
	}

	// app: espera a su carpeta sin dársela; se guarda en cuanto el agente
	// contesta, sin esperar a "listo" (su servicio aún no arrancó).
	app := &opsFalsas{}
	p = ps["app"]
	if err := prepararPlantilla(context.Background(), app, "dev", p, logf); err != nil {
		t.Fatal(err)
	}
	if app.run.ShareWait != 1 || len(app.run.Shares) != 0 || app.run.WaitReady {
		t.Errorf("app prep run: %+v", app.run)
	}
	if !app.commit.SkipReady || !reflect.DeepEqual(app.execs, [][]string{{"true"}}) {
		t.Errorf("app commit %+v execs %q", app.commit, app.execs)
	}
}
