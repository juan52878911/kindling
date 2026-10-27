package imagen

import (
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
)

func arbol(t *testing.T, files map[string]int64) *ext4.Node {
	t.Helper()
	now := time.Unix(0, 0)
	root := ext4.NewDir(0o755, 0, 0, now)
	for p, size := range files {
		n := &ext4.Node{Mode: ext4.ModeReg | 0o644, Size: size, Data: ext4.Bytes(make([]byte, size))}
		if err := root.Put(p, n, now); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func existe(r *ext4.Node, p string) bool { return r.Lookup(p) != nil }

func TestReglasEmbebidasValidas(t *testing.T) {
	r, err := CargarReglasSlim()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Familias) < 2 {
		t.Fatalf("expected postgres and timescaledb, got %d families", len(r.Familias))
	}
}

func TestParseReglasRechaza(t *testing.T) {
	for _, js := range []string{
		`{"familias":[{"nombre":"x"}]}`,
		`{"familias":[{"nombre":"x","detecta":["a"],"quita":["[a"]}]}`,
		`{"familias":[{"nombre":"x","detecta":["a"],"ultimaVersion":[{"dir":"d","patron":"(a)"}]}]}`,
		`{"familias":[{"nombre":"x","detecta":["a"],"añadeArgs":{"siEmpieza":"x"}}]}`,
		`nope`,
	} {
		if _, err := ParseReglasSlim([]byte(js)); err == nil {
			t.Errorf("%s: accepted", js)
		}
	}
}

func TestCasaRuta(t *testing.T) {
	for _, c := range []struct {
		g, p string
		ok   bool
	}{
		{"usr/lib/libLLVM*", "usr/lib/libLLVM.so.21.1", true},
		{"usr/lib/libLLVM*", "usr/lib/x/libLLVM.so", false},
		{"usr/lib/*/libz3*", "usr/lib/x86_64-linux-gnu/libz3.so.4", true},
		{"usr/**/bitcode", "usr/lib/postgresql/17/lib/bitcode", true},
		{"usr/lib", "usr/lib/x", false},
	} {
		if ok, _ := casaRuta(c.g, c.p); ok != c.ok {
			t.Errorf("casaRuta(%q,%q)=%v", c.g, c.p, ok)
		}
	}
}

func TestSlimPostgresAlpine(t *testing.T) {
	r, _ := CargarReglasSlim()
	tree := arbol(t, map[string]int64{
		"usr/local/bin/postgres":                     10,
		"usr/lib/libLLVM.so.21.1":                    100,
		"usr/local/lib/postgresql/llvmjit.so":        1,
		"usr/local/lib/postgresql/bitcode/a/b.bc":    20,
		"usr/local/lib/postgresql/plpgsql.so":        5,
		"usr/local/include/postgresql/server/x.h":    9,
		"usr/lib/libssl.so.3":                        4,
		"usr/local/share/postgresql/extension/x.sql": 1,
	})
	dry, err := r.Aplica(tree, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !existe(tree, "usr/lib/libLLVM.so.21.1") {
		t.Fatal("dry run removed files")
	}
	inf, _ := r.Aplica(tree, nil, false)
	if inf.Bytes != dry.Bytes || inf.Bytes != 100+1+20+9 || inf.Ficheros != 4 {
		t.Fatalf("report %+v, dry %+v", inf, dry)
	}
	for _, p := range []string{"usr/lib/libLLVM.so.21.1", "usr/local/lib/postgresql/bitcode", "usr/local/include", "usr/local/lib/postgresql/llvmjit.so"} {
		if existe(tree, p) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range []string{"usr/local/bin/postgres", "usr/local/lib/postgresql/plpgsql.so", "usr/lib/libssl.so.3", "usr/local/share/postgresql/extension/x.sql"} {
		if !existe(tree, p) {
			t.Errorf("%s was removed", p)
		}
	}
	got := ArgsSlimSobre([]string{"docker-entrypoint.sh", "postgres"}, inf.Args)
	if !slices.Equal(got, []string{"docker-entrypoint.sh", "postgres", "-c", "jit=off"}) {
		t.Errorf("argv %v", got)
	}
}

func TestSlimNoCasaOtraFamilia(t *testing.T) {
	r, _ := CargarReglasSlim()
	tree := arbol(t, map[string]int64{"usr/bin/redis-server": 1, "usr/lib/libLLVM.so.1": 50})
	inf, _ := r.Aplica(tree, nil, false)
	if len(inf.Familias) != 0 || !existe(tree, "usr/lib/libLLVM.so.1") {
		t.Fatalf("a non-postgres image was trimmed: %+v", inf)
	}
}

func TestSlimLocaleDelEntorno(t *testing.T) {
	r, _ := CargarReglasSlim()
	tree := arbol(t, map[string]int64{
		"usr/lib/postgresql/17/bin/postgres":   1,
		"usr/lib/locale/en_US.utf8/LC_CTYPE":   3,
		"usr/lib/locale/C.utf8/LC_CTYPE":       3,
		"usr/lib/locale/de_DE.utf8/LC_CTYPE":   3,
		"usr/share/locale/de/LC_MESSAGES/x.mo": 3,
	})
	if _, err := r.Aplica(tree, []string{"LANG=en_US.utf8"}, false); err != nil {
		t.Fatal(err)
	}
	if !existe(tree, "usr/lib/locale/en_US.utf8") || !existe(tree, "usr/lib/locale/C.utf8") {
		t.Error("the image's own locale was removed")
	}
	if existe(tree, "usr/lib/locale/de_DE.utf8") || existe(tree, "usr/share/locale/de") {
		t.Error("other locales kept")
	}
}

func TestSlimTimescaleUltimaVersion(t *testing.T) {
	r, _ := CargarReglasSlim()
	const lib, ext = "usr/local/lib/postgresql/", "usr/local/share/postgresql/extension/"
	tree := arbol(t, map[string]int64{
		"usr/local/bin/postgres":               1,
		lib + "timescaledb.so":                 1,
		lib + "timescaledb-2.9.0.so":           10,
		lib + "timescaledb-2.10.0.so":          10,
		lib + "timescaledb-tsl-2.9.0.so":       10,
		lib + "timescaledb-tsl-2.10.0.so":      10,
		ext + "timescaledb--2.9.0.sql":         2,
		ext + "timescaledb--2.10.0.sql":        2,
		ext + "timescaledb--2.9.0--2.10.0.sql": 2,
	})
	if _, err := r.Aplica(tree, nil, false); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		lib + "timescaledb-2.10.0.so":          true,
		lib + "timescaledb-2.9.0.so":           false,
		lib + "timescaledb-tsl-2.10.0.so":      true,
		lib + "timescaledb-tsl-2.9.0.so":       false,
		lib + "timescaledb.so":                 true,
		ext + "timescaledb--2.10.0.sql":        true,
		ext + "timescaledb--2.9.0.sql":         false,
		ext + "timescaledb--2.9.0--2.10.0.sql": false,
	} {
		if existe(tree, p) != want {
			t.Errorf("%s: exists=%v, want %v", p, !want, want)
		}
	}
}

func TestArgsSlimSobreNoRepite(t *testing.T) {
	a := []ArgsSlim{{SiEmpieza: "postgres", Args: []string{"-c", "jit=off"}}}
	in := []string{"postgres", "-c", "jit=off"}
	if got := ArgsSlimSobre(in, a); !slices.Equal(got, in) {
		t.Errorf("%v", got)
	}
	if got := ArgsSlimSobre([]string{"redis-server"}, a); len(got) != 1 {
		t.Errorf("%v", got)
	}
}
