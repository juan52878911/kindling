package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/scram"
	"github.com/juan52878911/kindling/pkg/api"
)

var ctx = context.Background()

// password lee la clave guardada de la copia, o falla el test.
func password(t *testing.T, id string) string {
	t.Helper()
	pw, err := dbstate.ReadPassword(id)
	if err != nil {
		t.Fatalf("password of %s: %v", id, err)
	}
	return pw
}

// assertVerifierMatches comprueba que el verificador que recibió el invitado
// es el de la clave que quedó en el host.
func assertVerifierMatches(t *testing.T, v, pw string) {
	t.Helper()
	m := regexp.MustCompile(`^SCRAM-SHA-256\$(\d+):([^$]+)\$[^:]+:.+$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("verifier format %q", v)
	}
	salt, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		t.Fatal(err)
	}
	want, err := scram.Verifier(pw, salt, scram.Iterations)
	if err != nil {
		t.Fatal(err)
	}
	if v != want {
		t.Fatalf("the guest got a verifier for another password")
	}
}

// assertSecretOnlyInStdin: la clave no aparece en ningún argv, ni en ningún
// stdin (al invitado va el verificador), ni en las etiquetas.
func assertNoLeak(t *testing.T, f *fakeKling, pw string) {
	t.Helper()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.args, " "), pw) {
			t.Fatalf("password in argv: %v", c.args)
		}
		if strings.Contains(c.stdin, pw) {
			t.Fatalf("password in the stdin of %v", c.args)
		}
		for k, v := range c.labels {
			if strings.Contains(k+v, pw) {
				t.Fatal("password in a label")
			}
		}
	}
}

func TestUpRotaYMarcaLista(t *testing.T) {
	ta := newTestApp(t)
	mc, err := ta.up(ctx, "pg", "c1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	live := ta.f.machines[mc.ID]
	if live.Labels[labelState] != stateReady || live.Labels[labelOwner] != "local" || live.Labels[labelGolden] != "pg" {
		t.Fatalf("labels %v", live.Labels)
	}
	if live.Labels[api.LabelKind] != api.KindSandbox || live.Labels[api.LabelPorts] != "5432,8080" {
		t.Fatalf("kind/ports %v", live.Labels)
	}
	// Nace en preparing: run -from lo lleva en sus etiquetas.
	run := ta.f.calls[1].args
	if run[0] != "run" || !strings.Contains(strings.Join(run, " "), "-label kling.db.state=preparing") {
		t.Fatalf("run %v", run)
	}
	pw := password(t, mc.ID)
	assertVerifierMatches(t, ta.f.verifier[mc.ID], pw)
	assertNoLeak(t, ta.f, pw)
	ta.printReady(mc)
	if strings.Contains(ta.out.String(), pw) {
		t.Fatal("password on stdout after up")
	}
	// Ready solo DESPUÉS de rotar.
	rot, ready := -1, -1
	for i, c := range ta.f.calls {
		if len(c.args) > 0 && c.args[0] == "exec" && strings.Contains(c.stdin, "ALTER ROLE") {
			rot = i
		}
		if c.labels[labelState] == stateReady {
			ready = i
		}
	}
	if rot < 0 || ready < rot {
		t.Fatalf("ready (%d) before the rotation (%d)", ready, rot)
	}
}

func TestUpRotacionFallidaDestruye(t *testing.T) {
	for name, set := range map[string]func(*fakeKling){
		"psql falla":           func(f *fakeKling) { f.failRotation = 1 },
		"verificador distinto": func(f *fakeKling) { f.wrongVerifier = true },
		"postgres no arranca":  func(f *fakeKling) { f.pgDown = true },
	} {
		t.Run(name, func(t *testing.T) {
			ta := newTestApp(t)
			set(ta.f)
			_, err := ta.up(ctx, "pg", "c1", 0, "local")
			if err == nil {
				t.Fatal("want error")
			}
			if len(ta.f.machines) != 0 {
				t.Fatalf("the copy was not destroyed: %v", ta.f.machines)
			}
			d, _ := dbstate.Dir()
			if ents, _ := os.ReadDir(filepath.Join(d, dbstate.CopiesDir)); len(ents) != 0 {
				t.Fatalf("password files left: %v", ents)
			}
			if strings.Contains(err.Error(), "SCRAM") {
				t.Fatalf("the error quotes the statement: %v", err)
			}
		})
	}
}

func TestUpNoBorraAjenasPorNombre(t *testing.T) {
	ta := newTestApp(t)
	other := ta.f.newMachine("c1", map[string]string{})
	if _, err := ta.up(ctx, "pg", "c1", 0, "local"); err == nil {
		t.Fatal("want error: name in use")
	}
	if ta.f.machines[other.ID] == nil {
		t.Fatal("up removed a machine that was not its own")
	}
}

func readyCopy(t *testing.T, ta *testApp, name string) *api.Machine {
	t.Helper()
	mc, err := ta.up(ctx, "pg", name, 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

func TestForkPreparaCadaCopia(t *testing.T) {
	ta := newTestApp(t)
	src := readyCopy(t, ta, "src")
	ta.f.machines[src.ID].State = api.StateWarm // congelada: fork la descongela
	start := len(ta.f.calls)
	copies, err := ta.fork(ctx, "src", 3, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 3 {
		t.Fatalf("%d copies", len(copies))
	}
	seen := map[string]bool{password(t, src.ID): true}
	for _, c := range copies {
		live := ta.f.machines[c.ID]
		if live.Labels[labelState] != stateReady {
			t.Fatalf("%s not ready", c.Name)
		}
		pw := password(t, c.ID)
		if seen[pw] {
			t.Fatal("two copies share a password")
		}
		seen[pw] = true
		assertVerifierMatches(t, ta.f.verifier[c.ID], pw)
		assertNoLeak(t, ta.f, pw)
	}
	// Nacen en preparing: el fork lleva la etiqueta, sin ventana en ready.
	forked := false
	for _, c := range ta.f.calls[start:] {
		if len(c.args) > 1 && c.args[0] == "sandbox" && c.args[1] == "fork" {
			forked = true
			if !strings.Contains(strings.Join(c.args, " "), "-label "+labelState+"="+statePreparing) {
				t.Fatalf("fork without the preparing label: %v", c.args)
			}
		}
	}
	if !forked {
		t.Fatal("no sandbox fork call")
	}
	if ta.f.calls[start+1].args[0] != "thaw" {
		t.Fatalf("frozen source not thawed: %v", ta.f.calls[start+1].args)
	}
}

func TestForkTodoONada(t *testing.T) {
	ta := newTestApp(t)
	src := readyCopy(t, ta, "src")
	ta.f.failRotation = ta.f.rotations + 2 // falla la segunda copia
	if _, err := ta.fork(ctx, "src", 3, "local"); err == nil {
		t.Fatal("want error")
	}
	if len(ta.f.machines) != 1 || ta.f.machines[src.ID] == nil {
		t.Fatalf("machines left: %v", ta.f.machines)
	}
	d, _ := dbstate.Dir()
	ents, _ := os.ReadDir(filepath.Join(d, dbstate.CopiesDir))
	if len(ents) != 1 || ents[0].Name() != src.ID {
		t.Fatalf("password dirs: %v", ents)
	}
}

func TestForkRechaza(t *testing.T) {
	ta := newTestApp(t)
	src := readyCopy(t, ta, "src")
	if _, err := ta.fork(ctx, "src", 1, "otro"); err == nil {
		t.Fatal("fork of someone else's copy")
	}
	ta.f.machines[src.ID].Labels[labelState] = statePreparing
	if _, err := ta.fork(ctx, "src", 1, "local"); err == nil {
		t.Fatal("fork of a copy that is not ready")
	}
	if _, err := ta.fork(ctx, "src", 0, "local"); err == nil {
		t.Fatal("-n 0")
	}
}

func TestConnect(t *testing.T) {
	ta := newTestApp(t)
	mc := readyCopy(t, ta, "c1")
	pw := password(t, mc.ID)

	ta.out.Reset()
	if err := ta.connect(ctx, "c1", "local", "info"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ta.out.String(), pw) || !strings.Contains(ta.out.String(), mc.IP) {
		t.Fatalf("info: %q", ta.out.String())
	}

	ta.out.Reset()
	if err := ta.connect(ctx, "c1", "local", "dsn"); err != nil {
		t.Fatal(err)
	}
	want := "postgres://app:" + pw + "@" + mc.IP + ":5432/appdb?sslmode=disable\n"
	if ta.out.String() != want {
		t.Fatalf("dsn %q, want %q", ta.out.String(), want)
	}

	// En una terminal pregunta; sin un sí no imprime nada.
	ta.tty = true
	ta.out.Reset()
	ta.stdin = strings.NewReader("n\n")
	if err := ta.connect(ctx, "c1", "local", "dsn"); err == nil || ta.out.Len() != 0 {
		t.Fatalf("dsn without confirmation: %v %q", err, ta.out.String())
	}
	ta.stdin = strings.NewReader("y\n")
	if err := ta.connect(ctx, "c1", "local", "dsn"); err != nil || !strings.Contains(ta.out.String(), pw) {
		t.Fatalf("dsn confirmed: %v %q", err, ta.out.String())
	}

	ta.out.Reset()
	if err := ta.connect(ctx, "c1", "local", "psql"); err != nil {
		t.Fatal(err)
	}
	if ta.psql[0] != "PGPASSWORD="+pw || strings.Contains(strings.Join(ta.psql[1:], " "), pw) {
		t.Fatalf("psql %v", ta.psql)
	}
	if ta.out.Len() != 0 {
		t.Fatalf("psql printed %q", ta.out.String())
	}
	assertNoLeak(t, ta.f, pw)
}

func TestConnectMacUsaElReenvio(t *testing.T) {
	ta := newTestApp(t)
	ta.f.macOS = true
	mc := readyCopy(t, ta, "c1")
	if err := ta.connect(ctx, "c1", "local", "dsn"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "@"+ta.f.machines[mc.ID].Forwards["5432"]+"/") {
		t.Fatalf("dsn %q", ta.out.String())
	}
	delete(ta.f.machines[mc.ID].Forwards, "5432")
	if err := ta.connect(ctx, "c1", "local", "info"); err == nil {
		t.Fatal("no forward for 5432 must be an error, not the shared guest IP")
	}
}

func TestConnectRechaza(t *testing.T) {
	ta := newTestApp(t)
	mc := readyCopy(t, ta, "c1")
	live := ta.f.machines[mc.ID]

	if err := ta.connect(ctx, "c1", "otro", "dsn"); err == nil {
		t.Fatal("someone else's copy")
	}
	live.State = api.StateWarm
	if err := ta.connect(ctx, "c1", "local", "dsn"); err == nil {
		t.Fatal("frozen copy")
	}
	live.State = api.StateRunning
	live.Labels[labelState] = statePreparing
	if err := ta.connect(ctx, "c1", "local", "dsn"); err == nil {
		t.Fatal("copy being prepared")
	}
	// Una máquina que heredó ready (p. ej. un fork hecho a mano) no tiene la
	// contraseña de SU id: no se entrega la de nadie.
	orphan := ta.f.newMachine("orphan", clone(live.Labels))
	orphan.Labels[labelState] = stateReady
	for _, mode := range []string{"info", "dsn", "psql"} {
		if err := ta.connect(ctx, "orphan", "local", mode); err == nil {
			t.Fatalf("%s: copy with an inherited ready and no password of its own", mode)
		}
	}
	if err := ta.connect(ctx, "nada", "local", "info"); err == nil {
		t.Fatal("missing copy")
	}
	if strings.Contains(ta.out.String(), password(t, mc.ID)) {
		t.Fatal("password printed by a refused connect")
	}
}

func TestReset(t *testing.T) {
	ta := newTestApp(t)
	old := readyCopy(t, ta, "c1")
	mc, err := ta.reset(ctx, "c1", "local")
	if err != nil {
		t.Fatal(err)
	}
	if mc.ID == old.ID || mc.Name != "c1" || ta.f.machines[old.ID] != nil {
		t.Fatalf("reset: %+v", mc)
	}
	if _, err := dbstate.ReadPassword(old.ID); err == nil {
		t.Fatal("the old password survived the reset")
	}
	password(t, mc.ID)
	if _, err := ta.reset(ctx, "c1", "otro"); err == nil {
		t.Fatal("reset of someone else's copy")
	}
}

func TestEtiquetasValidas(t *testing.T) {
	for _, k := range dbLabelKeys {
		if !api.KeyPattern.MatchString(k) {
			t.Errorf("label %q does not match api.KeyPattern", k)
		}
	}
	ta := newTestApp(t)
	readyCopy(t, ta, "c1")
	for _, c := range ta.f.calls {
		for i, a := range c.args {
			if a == "-label" {
				k, _, _ := strings.Cut(c.args[i+1], "=")
				if !api.KeyPattern.MatchString(k) {
					t.Errorf("run label %q does not match api.KeyPattern", k)
				}
			}
		}
		for k := range c.labels {
			if !api.KeyPattern.MatchString(k) {
				t.Errorf("SetLabels key %q does not match api.KeyPattern", k)
			}
		}
	}
	if err := validOwner("a/b"); err == nil {
		t.Error("owner with '/'")
	}
}

func TestRolDeLaPlantilla(t *testing.T) {
	ta := newTestApp(t)
	ta.f.snaps["pg"].Labels[labelRole] = "robert'); DROP"
	if _, err := ta.up(ctx, "pg", "c1", 0, "local"); err == nil {
		t.Fatal("a role that is not a plain identifier must be refused")
	}
	if len(ta.f.machines) != 0 {
		t.Fatal("machine created with a bad role")
	}
	ta.f.snaps["pg"].Labels[labelRole] = "postgres"
	if _, err := ta.up(ctx, "pg", "c1", 0, "local"); err == nil {
		t.Fatal("the superuser as the app role")
	}
}

func TestMergePorts(t *testing.T) {
	for in, want := range map[string]string{"": "5432", "8080": "5432,8080", "5432": "5432", " 9000,5432,x": "5432,9000"} {
		if got := mergePorts(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestParseIntercalado(t *testing.T) {
	fs, _, owner := newFlags("up")
	name := fs.String("name", "", "")
	pos, err := parse(fs, []string{"pg", "-name", "x", "-owner", "ci"})
	if err != nil || len(pos) != 1 || pos[0] != "pg" || *name != "x" || *owner != "ci" {
		t.Fatalf("%v %v %q %q", pos, err, *name, *owner)
	}
}
