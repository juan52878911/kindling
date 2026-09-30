package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// -switch deja el congelado al proceso de fondo: la otra rama sigue en marcha
// al volver, y el proceso se lanza con el dueño y el daemon de esta llamada.
func TestBranchSwitchCongelaEnSegundoPlano(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	var got [][]string
	ta.background = func(args []string) error {
		got = append(got, append([]string(nil), args...))
		return nil
	}
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	if err := ta.branchSwitch(ctx, "ssh://u@h", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"branch", "-settle", "-owner", defaultOwner, "-H", "ssh://u@h"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("background = %v, want %v", got, want)
	}
	if st := ta.copyOf(t, "main").State; st != api.StateRunning {
		t.Errorf("main is %s: -switch froze it instead of leaving it to the background", st)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", branchEnvFile)); err != nil {
		t.Error(err)
	}
	// Lo que hará el proceso de fondo.
	ri, _ := ta.repo(ctx)
	if err := ta.settle(ctx, ri, defaultOwner); err != nil {
		t.Fatal(err)
	}
	if st := ta.copyOf(t, "main").State; st != api.StateWarm {
		t.Errorf("after settle main is %s", st)
	}
	// Con -wait, en el acto.
	got = nil
	gitT(t, dir, "checkout", "-q", "main")
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || ta.copyOf(t, "feat").State != api.StateWarm {
		t.Errorf("-wait: background %v, feat %s", got, ta.copyOf(t, "feat").State)
	}
}

// Si no se puede lanzar el proceso de fondo, se congela en el acto (y se dice).
func TestBranchSwitchSinSegundoPlanoCongelaYa(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.background = func([]string) error { return errors.New("no exec") }
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	if st := ta.copyOf(t, "main").State; st != api.StateWarm {
		t.Errorf("main is %s", st)
	}
	if !strings.Contains(ta.err.String(), "freezing them now") {
		t.Errorf("stderr: %s", ta.err)
	}
}

func TestRegistroDeSegundoPlano(t *testing.T) {
	p := filepath.Join(t.TempDir(), backgroundLog)
	f, err := openBackgroundLog(p)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("uno\n")
	f.Close()
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	// Pasado el tope, se empieza de cero.
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), maxBackgroundLog+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err = openBackgroundLog(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, _ := os.Stat(p); st.Size() > 100 {
		t.Errorf("not truncated: %d bytes", st.Size())
	}
	// Un enlace en su lugar no se sigue.
	q := filepath.Join(t.TempDir(), "otro")
	os.WriteFile(q, []byte("no tocar"), 0o600)
	os.Remove(p)
	if err := os.Symlink(q, p); err != nil {
		t.Skip(err)
	}
	if f, err := openBackgroundLog(p); err == nil {
		f.Close()
		t.Error("followed a symlink")
	}
}

// La traza solo lleva subcomandos: ni nombres de rama ni de máquina.
func TestTraceNameSinArgumentos(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"thaw", "db-abc-feat-secret"}, "kling thaw"},
		{[]string{"sandbox", "fork", "id", "-label", "kling.db.branch=x"}, "kling sandbox fork"},
		{[]string{"exec", "-i", "id", "--", "su", "-s", "/bin/sh", "postgres", "-c", psqlSuper}, "kling exec su …psql"},
		{[]string{"exec", "-i", "id", "--", "sh", "-s"}, "kling exec sh"},
	} {
		if got := traceName(c.args); got != c.want {
			t.Errorf("traceName(%v) = %q, want %q", c.args, got, c.want)
		}
	}
	var b bytes.Buffer
	tr := &tracer{w: &b}
	tr.span("phase x")()
	if !strings.HasPrefix(b.String(), "trace ") || !strings.HasSuffix(b.String(), "  phase x\n") {
		t.Errorf("line %q", b.String())
	}
	var nilT *tracer
	nilT.span("nada")() // sin KLING_DB_TRACE no hace nada
}
