package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func classOf(prefix string) classOpts {
	return classOpts{prefix: prefix, owner: defaultOwner, parallel: 4}
}

// members son las copias de la clase en el falso, por nombre.
func (ta *testApp) members(t *testing.T, o classOpts) map[string]*api.Machine {
	t.Helper()
	m := map[string]*api.Machine{}
	for _, mc := range ta.f.machines {
		if o.inClass(mc) {
			m[mc.Name] = mc
		}
	}
	return m
}

// noPasswordsIn falla si alguna clave de las copias aparece en s.
func noPasswordsIn(t *testing.T, ta *testApp, s string) {
	t.Helper()
	for id := range ta.f.machines {
		if pw, err := os.ReadFile(passwordFile(id)); err == nil && strings.Contains(s, strings.TrimSpace(string(pw))) {
			t.Fatalf("the password of %s was printed", id)
		}
	}
}

func TestClassNombres(t *testing.T) {
	if got := classNames("alumno", 3); strings.Join(got, ",") != "alumno-01,alumno-02,alumno-03" {
		t.Errorf("got %v", got)
	}
	if got := classNames("s", 120); got[0] != "s-001" || got[119] != "s-120" {
		t.Errorf("got %s..%s", got[0], got[119])
	}
	for _, n := range classNames(strings.Repeat("a", 40), classMax) {
		if !namePattern.MatchString(n) {
			t.Fatalf("%q is not a valid machine name", n)
		}
	}
	for _, p := range []string{"", "-x", "A", "a b", "a.b", strings.Repeat("a", 41), "a/b"} {
		if classPattern.MatchString(p) {
			t.Errorf("prefix %q accepted", p)
		}
	}
}

func TestClassCreaListaYEsIdempotente(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	o := classOf("alumno")
	if err := ta.classUp(ctx, o, "pg", 12, 0); err != nil {
		t.Fatal(err)
	}
	m := ta.members(t, o)
	if len(m) != 12 {
		t.Fatalf("%d copies", len(m))
	}
	ids := map[string]bool{}
	for _, name := range classNames("alumno", 12) {
		mc := m[name]
		if mc == nil || mc.Labels[labelState] != stateReady || mc.Labels[labelGolden] != "pg" {
			t.Fatalf("%s: %+v", name, mc)
		}
		ids[password(t, mc.ID)] = true
	}
	if len(ids) != 12 {
		t.Error("two copies share a password")
	}
	out := ta.out.String()
	for _, w := range []string{"COPY", "alumno-01", "alumno-12", "12 copies in class alumno, 12 ready", "kling db connect", "-passwords FILE"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	noPasswordsIn(t, ta, out+ta.err.String())

	// Otra vez con -n 14: solo las dos que faltan.
	ta.err.Reset()
	if err := ta.classUp(ctx, o, "pg", 14, 0); err != nil {
		t.Fatal(err)
	}
	m2 := ta.members(t, o)
	if len(m2) != 14 {
		t.Fatalf("%d copies", len(m2))
	}
	for name, mc := range m {
		if m2[name].ID != mc.ID {
			t.Errorf("%s was recreated", name)
		}
	}
	if !strings.Contains(ta.err.String(), "12 of 14 copies already exist") {
		t.Errorf("stderr:\n%s", ta.err)
	}
}

func TestClassNoPisaNombresAjenos(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	ta.f.newMachine("alumno-02", map[string]string{"x": "y"})
	n := len(ta.f.machines)
	err := ta.classUp(ctx, classOf("alumno"), "pg", 3, 0)
	if err == nil || !strings.Contains(err.Error(), "alumno-02") || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.machines) != n {
		t.Error("it created copies anyway")
	}
	// Un golden que no existe falla antes de lanzar nada.
	if err := ta.classUp(ctx, classOf("otra"), "nope", 3, 0); err == nil || len(ta.f.machines) != n {
		t.Fatalf("err = %v, %d machines", err, len(ta.f.machines))
	}
}

func TestClassFalloParcialYReintento(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	o := classOf("taller")
	o.parallel = 1 // orden fijo: falla la tercera rotación
	ta.f.failRotation = 3
	err := ta.classUp(ctx, o, "pg", 4, 0)
	if err == nil || !strings.Contains(err.Error(), "1 of 4 copies could not be created") || !strings.Contains(err.Error(), "again") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.members(t, o)) != 3 {
		t.Fatalf("%d copies, want the 3 that worked", len(ta.members(t, o)))
	}
	if err := ta.classUp(ctx, o, "pg", 4, 0); err != nil {
		t.Fatal(err)
	}
	if len(ta.members(t, o)) != 4 {
		t.Fatal("the retry did not fill the gap")
	}
}

func TestClassPasswordsAFichero(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	o := classOf("alumno")
	if err := ta.classUp(ctx, o, "pg", 3, 0); err != nil {
		t.Fatal(err)
	}
	ta.out.Reset()
	o.passwords = filepath.Join(t.TempDir(), "claves.tsv")
	if err := ta.classLs(ctx, o, false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(o.passwords)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("passwords file: %v %v", st, err)
	}
	b, _ := os.ReadFile(o.passwords)
	for name, mc := range ta.members(t, o) {
		if !strings.Contains(string(b), name+"\tpostgres://app:"+password(t, mc.ID)+"@") {
			t.Errorf("%s missing from the file:\n%s", name, b)
		}
	}
	noPasswordsIn(t, ta, ta.out.String()+ta.err.String())
	if !strings.Contains(ta.err.String(), o.passwords) {
		t.Errorf("did not say where: %s", ta.err)
	}
	// -json no lleva claves, solo la ruta del fichero de cada una.
	ta.out.Reset()
	o.passwords = ""
	if err := ta.classLs(ctx, o, true); err != nil {
		t.Fatal(err)
	}
	var rows []classRow
	if err := json.Unmarshal(ta.out.Bytes(), &rows); err != nil || len(rows) != 3 || rows[0].PasswordFile == "" || !rows[0].Ready {
		t.Fatalf("json: %v %s", err, ta.out)
	}
	noPasswordsIn(t, ta, ta.out.String())
}

func TestClassResetYRm(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	o := classOf("alumno")
	if err := ta.classUp(ctx, o, "pg", 4, 0); err != nil {
		t.Fatal(err)
	}
	// Una copia de otro dueño con el mismo prefijo no es de la clase.
	foreign := ta.f.newMachine("ajena", map[string]string{labelGolden: "pg", labelOwner: "otro", labelClass: "alumno", labelState: stateReady})
	before := ta.members(t, o)

	if err := ta.classReset(ctx, o, []string{"alumno-03"}); err != nil {
		t.Fatal(err)
	}
	after := ta.members(t, o)
	if len(after) != 4 {
		t.Fatalf("%d copies after reset", len(after))
	}
	for name, mc := range before {
		changed := after[name].ID != mc.ID
		if changed != (name == "alumno-03") {
			t.Errorf("%s changed=%v", name, changed)
		}
	}
	if _, err := os.Stat(passwordFile(before["alumno-03"].ID)); err == nil {
		t.Error("the old password of alumno-03 is still on the host")
	}
	if err := ta.classReset(ctx, o, []string{"ajena"}); err == nil {
		t.Fatal("reset of a copy outside the class")
	}

	if err := ta.classReset(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	for name, mc := range ta.members(t, o) {
		if mc.ID == after[name].ID {
			t.Errorf("%s was not reset", name)
		}
	}

	ta.out.Reset()
	if err := ta.classRm(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if len(ta.members(t, o)) != 0 || ta.f.find(foreign.ID) == nil {
		t.Fatal("rm removed the wrong copies")
	}
	if !strings.Contains(ta.out.String(), "alumno-04") {
		t.Errorf("rm output: %s", ta.out)
	}
	if err := ta.classRm(ctx, o, nil); err == nil {
		t.Error("rm of an empty class worked")
	}
}

// Un reset suelto (kling db reset) tampoco saca la copia de su clase ni de su rama.
func TestResetConservaPertenencia(t *testing.T) {
	ta := newTestApp(t)
	ctx := context.Background()
	mc, err := ta.upFrom(ctx, "pg", "pg", "c1", 0, defaultOwner,
		[][2]string{{labelClass, "alumno"}, {labelRepo, "abcdef012345"}, {labelBranch, "main-abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := ta.reset(ctx, "c1", defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	live := ta.f.find(nc.ID)
	if nc.ID == mc.ID || live.Labels[labelClass] != "alumno" || live.Labels[labelRepo] != "abcdef012345" || live.Labels[labelBranch] != "main-abcdef" {
		t.Fatalf("labels after reset: %v", live.Labels)
	}
}

func TestBoundedRespetaElTope(t *testing.T) {
	var cur, peak atomic.Int32
	errs := bounded(context.Background(), 20, 3, func(i int) error {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return nil
	})
	if len(errs) != 20 || peak.Load() > 3 || peak.Load() < 2 {
		t.Fatalf("peak %d", peak.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range bounded(ctx, 3, 1, func(int) error { return nil }) {
		// Con el contexto ya cancelado, alguna puede entrar antes que el
		// select lo vea; ninguna devuelve otra cosa que nil o ctx.Err().
		if err != nil && err != context.Canceled {
			t.Fatal(err)
		}
	}
}

func TestClassUso(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	for _, args := range [][]string{
		{},
		{"pg"},
		{"-n", "3"},
		{"ls", "x"},
		{"ls", "-n", "3"},
		{"rm", "-json"},
		{"rm", "-passwords", "f"},
		{"-n", "3", "a", "b"},
	} {
		if err := cmdClass(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"-n", "3", "-prefix", "Bad", "pg"},
		{"-n", "3", "-parallel", "0", "pg"},
		{"-n", "3", "-passwords", "-", "pg"},
	} {
		if err := cmdClass(args); err == nil || strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
