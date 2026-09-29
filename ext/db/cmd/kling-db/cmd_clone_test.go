package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbmask"
	"github.com/juan52878911/kindling/pkg/api"
)

// cloneFake es el kling de las pruebas de clone: contesta cada paso por el
// marcador que lleva su stdin y apunta todo lo que se le pide.
type cloneFake struct {
	mu      sync.Mutex
	calls   []call
	creds   []api.CredentialSpec
	alive   map[string]bool
	removed []string
	// templates que "existen".
	templates map[string]bool
	// probe es la línea que contesta la comprobación del rol.
	probe string
	// catalog es el JSON del catálogo.
	catalog string
	// maskOut y maskErr son la salida y el error del enmascarado.
	maskOut string
	maskErr error
	// dumpErr hace fallar el volcado de producción.
	dumpErr error
	// seedContent es lo que "copia" kling cp al host.
	seedContent string
}

func newCloneFake() *cloneFake {
	return &cloneFake{
		alive:     map[string]bool{},
		templates: map[string]bool{},
		probe:     "probe|f|0|160004\n",
		catalog: `{"tables":[{"schema":"public","name":"users","partitioned":false,"rows":3},{"schema":"public","name":"orders","partitioned":false,"rows":2}],` +
			`"columns":[{"schema":"public","table":"users","column":"id","type":"integer","category":"N","generated":false},` +
			`{"schema":"public","table":"users","column":"email","type":"text","category":"S","generated":false},` +
			`{"schema":"public","table":"users","column":"full_name","type":"text","category":"S","generated":false},` +
			`{"schema":"public","table":"orders","column":"customer_email","type":"text","category":"S","generated":false},` +
			`{"schema":"public","table":"orders","column":"total","type":"numeric","category":"N","generated":false}]}`,
		// orders (customer_email), users (email, full_name)
		maskOut:     "kc|0|2|2|0\nkc|1|3|3,3|0,0\n",
		seedContent: "-- masked dump\n",
	}
}

func (f *cloneFake) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{args: append([]string(nil), args...), stdin: string(in)})
	switch args[0] {
	case "template":
		if f.templates[args[2]] {
			return []byte(`{"name":"` + args[2] + `"}`), nil
		}
		return nil, errors.New("no template")
	case "run":
		for i, a := range args {
			if a == "-name" {
				f.alive[args[i+1]] = true
			}
		}
		return nil, nil
	case "rm":
		f.removed = append(f.removed, args[len(args)-1])
		delete(f.alive, args[len(args)-1])
		return nil, nil
	case "cp":
		dst := args[2]
		return nil, os.WriteFile(dst, []byte(f.seedContent), 0o600)
	case "exec":
		s := string(in)
		switch {
		case strings.Contains(s, "kling-db:clone-setup"):
			return nil, nil
		case strings.Contains(s, "kling-db:clone-probe"):
			return []byte(f.probe), nil
		case strings.Contains(s, "kling-db:clone-dump"):
			return nil, f.dumpErr
		case strings.Contains(s, dbmask.CatalogMarker):
			return []byte(f.catalog), nil
		case strings.Contains(s, dbmask.MaskMarker):
			return []byte(f.maskOut), f.maskErr
		}
		return nil, nil // true, pg_dump del enmascarado
	}
	return nil, fmt.Errorf("unexpected kling %v", args)
}

func (f *cloneFake) SetLabels(context.Context, string, map[string]string) error { return nil }

func (f *cloneFake) SetCredential(_ context.Context, ref string, spec api.CredentialSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = append(f.creds, spec)
	return nil
}

func (f *cloneFake) RemoveCredential(context.Context, string, string, string) error { return nil }

type cloneTest struct {
	f       *cloneFake
	c       *cloner
	out     *bytes.Buffer
	golden  [][]string
	seedSaw string
	tmp     string
}

func newCloneTest(t *testing.T) *cloneTest {
	t.Helper()
	t.Setenv("KLING_DB_STATE", t.TempDir())
	ct := &cloneTest{f: newCloneFake(), out: &bytes.Buffer{}, tmp: t.TempDir()}
	a := &app{k: ct.f, stdout: ct.out, stderr: ct.out, stdin: strings.NewReader(""), sleep: func(time.Duration) {}}
	ct.c = &cloner{a: a, tmpDir: ct.tmp, buildGolden: func(_ context.Context, args []string) error {
		ct.golden = append(ct.golden, args)
		// El seed existe mientras se construye, y es 0600.
		seed := args[2]
		st, err := os.Stat(seed)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0o600 {
			return fmt.Errorf("seed mode %v", st.Mode().Perm())
		}
		b, _ := os.ReadFile(seed)
		ct.seedSaw = string(b)
		return nil
	}}
	return ct
}

const clonePW = "s3cret-prod-Passw0rd"

func (ct *cloneTest) opts(t *testing.T, rules string) cloneOpts {
	t.Helper()
	src, err := parseCloneURL("postgres://reader@db.example.com:6543/shop")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := dbmask.ParseRules([]byte(rules))
	if err != nil {
		t.Fatal(err)
	}
	return cloneOpts{source: src, rules: rs, password: clonePW, golden: "shop-masked", mem: "2G"}
}

const cloneRules = "users.email: email\nusers.full_name: name\norders.customer_email: email\n"

// sinRastro comprueba lo que tiene que cumplirse SIEMPRE: la contraseña solo
// fue al proxy (ni argv ni stdin de ningún kling), la máquina de construcción
// está borrada y no queda ningún temporal en el host.
func (ct *cloneTest) sinRastro(t *testing.T) {
	t.Helper()
	for _, c := range ct.f.calls {
		if strings.Contains(strings.Join(c.args, " "), clonePW) || strings.Contains(c.stdin, clonePW) {
			t.Fatalf("the password reached kling: %v", c.args)
		}
	}
	if strings.Contains(ct.out.String(), clonePW) {
		t.Fatal("the password was printed")
	}
	if len(ct.f.alive) != 0 {
		t.Fatalf("builder left alive: %v", ct.f.alive)
	}
	ents, _ := os.ReadDir(ct.tmp)
	if len(ents) != 0 {
		t.Fatalf("temporary files left: %v", ents)
	}
}

func TestCloneBien(t *testing.T) {
	ct := newCloneTest(t)
	rep, err := ct.c.run(context.Background(), ct.opts(t, cloneRules))
	if err != nil {
		t.Fatalf("%v\n%s", err, ct.out)
	}
	ct.sinRastro(t)
	// La credencial: al proxy, con el upstream fijado y verify-full contra el host.
	if len(ct.f.creds) != 1 {
		t.Fatalf("creds: %+v", ct.f.creds)
	}
	cr := ct.f.creds[0]
	if cr.Secret != clonePW || cr.Upstream != "db.example.com:6543" || cr.UpstreamTLS != "" || cr.TLSServerName != "db.example.com" ||
		cr.User != "reader" || cr.Database != "shop" || cr.Domain != cloneDomain || cr.Type != "postgres" {
		t.Fatalf("credential: %+v", cr)
	}
	// La máquina de construcción: egress allowlist y borrado (no congelado) al vencer.
	run := strings.Join(ct.f.calls[1].args, " ")
	for _, want := range []string{"-egress allowlist", "-on-ttl remove", "-allow-exec", "-image pg16"} {
		if !strings.Contains(run, want) {
			t.Fatalf("run %q lacks %q", run, want)
		}
	}
	// El golden, desde el volcado enmascarado y DESPUÉS de borrar la construcción.
	if len(ct.golden) != 1 || ct.golden[0][0] != "build" || ct.golden[0][1] != "-seed" || ct.golden[0][len(ct.golden[0])-1] != "shop-masked" {
		t.Fatalf("golden: %v", ct.golden)
	}
	if ct.seedSaw != ct.f.seedContent {
		t.Fatalf("seed: %q", ct.seedSaw)
	}
	if len(ct.f.removed) != 1 {
		t.Fatalf("removed: %v", ct.f.removed)
	}
	// El enmascarado lleva su sal; el informe no.
	var mask string
	for _, c := range ct.f.calls {
		if strings.Contains(c.stdin, dbmask.MaskMarker) {
			mask = c.stdin
		}
	}
	if mask == "" {
		t.Fatal("no masking SQL")
	}
	var txt bytes.Buffer
	_ = rep.WriteText(&txt)
	_ = rep.WriteJSON(&txt)
	for _, line := range strings.Split(mask, "\n") {
		if i := strings.Index(line, "convert_to('"); i >= 0 {
			salt := line[i+len("convert_to('") : i+len("convert_to('")+64]
			if strings.Contains(txt.String(), salt) || strings.Contains(ct.out.String(), salt) {
				t.Fatal("the salt leaked")
			}
			break
		}
	}
	if !strings.Contains(txt.String(), "public.users.email") || !strings.Contains(txt.String(), "3 of 3 rows") {
		t.Fatalf("report:\n%s", txt.String())
	}
}

func TestCloneFalloNoDejaGolden(t *testing.T) {
	cases := map[string]func(*cloneFake, *cloneOpts, *testing.T){
		"regla que no cabe": func(f *cloneFake, _ *cloneOpts, _ *testing.T) {
			f.maskOut = "kc|0|2|2|0\n"
			f.maskErr = errors.New("psql:<stdin>:9: ERROR:  22P02")
		},
		"valor que no cambió": func(f *cloneFake, _ *cloneOpts, _ *testing.T) {
			f.maskOut = "kc|0|2|2|1\nkc|1|3|3,3|0,0\n"
			f.maskErr = errors.New("psql:<stdin>:12: ERROR:  22012")
		},
		"sospechosa sin regla": func(_ *cloneFake, o *cloneOpts, t *testing.T) {
			rs, _ := dbmask.ParseRules([]byte("users.email: email\norders.customer_email: email\n"))
			o.rules = rs
		},
		"regla sobre columna inexistente": func(_ *cloneFake, o *cloneOpts, t *testing.T) {
			rs, _ := dbmask.ParseRules([]byte(cloneRules + "users.emial: email\n"))
			o.rules = rs
		},
		"rol que escribe": func(f *cloneFake, _ *cloneOpts, _ *testing.T) {
			f.probe = "probe|f|3|160004\n"
		},
		"superusuario": func(f *cloneFake, o *cloneOpts, _ *testing.T) {
			f.probe = "probe|t|0|160004\n"
			o.allowWriter = true
		},
		"postgres demasiado nuevo": func(f *cloneFake, _ *cloneOpts, _ *testing.T) {
			f.probe = "probe|f|0|170002\n"
		},
		"volcado que falla": func(f *cloneFake, _ *cloneOpts, _ *testing.T) {
			f.dumpErr = errors.New("pg_dump: error: connection failed")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ct := newCloneTest(t)
			o := ct.opts(t, cloneRules)
			setup(ct.f, &o, t)
			_, err := ct.c.run(context.Background(), o)
			if err == nil {
				t.Fatal("clone succeeded")
			}
			if len(ct.golden) != 0 {
				t.Fatal("a golden was built")
			}
			ct.sinRastro(t)
			if strings.Contains(err.Error(), clonePW) {
				t.Fatal("the password is in the error")
			}
		})
	}
}

func TestCloneValorViejoNombraLaColumna(t *testing.T) {
	ct := newCloneTest(t)
	ct.f.maskOut = "kc|0|2|2|1\nkc|1|3|3,3|0,0\n"
	ct.f.maskErr = errors.New("ERROR:  22012")
	_, err := ct.c.run(context.Background(), ct.opts(t, cloneRules))
	if err == nil || !strings.Contains(err.Error(), "public.orders.customer_email") {
		t.Fatalf("want the column in the error, got %v", err)
	}
}

func TestCloneAllowWriterYUnmasked(t *testing.T) {
	ct := newCloneTest(t)
	ct.f.probe = "probe|f|3|160004\n"
	o := ct.opts(t, "users.email: email\n")
	o.allowWriter = true
	o.mask.AllowUnmasked = true
	ct.f.maskOut = "kc|0|3|3|0\n"
	rep, err := ct.c.run(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, ct.out)
	}
	ct.sinRastro(t)
	if len(rep.Unmasked) != 2 {
		t.Fatalf("unmasked: %v", rep.Unmasked)
	}
}

func TestCloneAntesDeArrancar(t *testing.T) {
	for name, mod := range map[string]func(*cloneOpts, *cloneFake){
		"sin contraseña":        func(o *cloneOpts, _ *cloneFake) { o.password = "" },
		"contraseña no ascii":   func(o *cloneOpts, _ *cloneFake) { o.password = "clave\n" },
		"golden que ya existe":  func(_ *cloneOpts, f *cloneFake) { f.templates["shop-masked"] = true },
		"nombre de golden malo": func(o *cloneOpts, _ *cloneFake) { o.golden = "Shop Masked" },
		"disable con -ca": func(o *cloneOpts, _ *cloneFake) {
			o.source.tls = "disable"
			o.caPEM = "x"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ct := newCloneTest(t)
			o := ct.opts(t, cloneRules)
			mod(&o, ct.f)
			if _, err := ct.c.run(context.Background(), o); err == nil {
				t.Fatal("accepted")
			}
			for _, c := range ct.f.calls {
				if c.args[0] == "run" {
					t.Fatal("a builder was started")
				}
			}
		})
	}
	// -replace deja reconstruir uno que existe.
	ct := newCloneTest(t)
	ct.f.templates["shop-masked"] = true
	o := ct.opts(t, cloneRules)
	o.replace = true
	if _, err := ct.c.run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestCloneURL(t *testing.T) {
	s, err := parseCloneURL("postgresql://ro@10.0.3.25/app?sslmode=disable")
	if err != nil || s.port != "5432" || s.tls != "disable" || s.db != "app" || s.host != "10.0.3.25" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = parseCloneURL("postgres://ro@[::1]:5433")
	if err != nil || s.host != "::1" || s.db != "ro" || s.String() != "ro@[::1]:5433/ro" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{
		"postgres://ro:" + clonePW + "@db/app", // contraseña en la URL
		"mysql://ro@db/app",
		"postgres://db/app",                    // sin usuario
		"postgres://ro@db/app?sslmode=require", // ni verify-full ni disable
		"postgres://ro@db/app?host=/tmp",
		"postgres://ro@db:99999/app",
	} {
		_, err := parseCloneURL(bad)
		if err == nil {
			t.Errorf("accepted %q", bad)
		} else if strings.Contains(err.Error(), clonePW) {
			t.Errorf("the error quotes the password: %v", err)
		}
	}
}

func TestCloneScripts(t *testing.T) {
	s := &cloneSource{host: "h", port: "5432", user: "o'brien", db: "shop"}
	for _, sc := range []string{cloneProbeScript(s), cloneDumpScript(s)} {
		if !strings.Contains(sc, `U='o'\''brien'`) {
			t.Fatalf("user not quoted:\n%s", sc)
		}
		if !strings.Contains(sc, "169.254.169.254") || !strings.Contains(sc, "-h "+cloneDomain) {
			t.Fatal("the builder must connect through the proxy with the MMDS placeholder")
		}
	}
	dump := cloneDumpScript(s)
	// El volcado va por una tubería al Postgres del tmpfs, nunca a un fichero.
	if !strings.Contains(dump, "2>\"$C/dump.err\" \\\n  | su -s /bin/sh postgres") || strings.Contains(dump, "pg_dump -f") ||
		!strings.Contains(dump, "VERBOSITY=sqlstate") || !strings.Contains(dump, "--no-blobs") {
		t.Fatalf("dump script:\n%s", dump)
	}
	if !strings.Contains(cloneSetupScript, "mount -t tmpfs") || !strings.Contains(cloneSetupScript, "/proc/swaps") ||
		!strings.Contains(cloneSetupScript, "listen_addresses = ''") {
		t.Fatal("setup script")
	}
	if got := defaultGoldenName("Shop Prod!"); got != "shop-prod-masked" || !goldenNamePattern.MatchString(got) {
		t.Fatalf("default name %q", got)
	}
	if got := defaultGoldenName("!!!"); got != "clone-masked" {
		t.Fatalf("default name %q", got)
	}
	if !goldenNamePattern.MatchString(defaultGoldenName(strings.Repeat("a", 100))) {
		t.Fatal("long default name")
	}
}

func TestCloneUsoIncorrecto(t *testing.T) {
	for _, args := range [][]string{
		{"clone"},
		{"clone", "postgres://ro@db/app"}, // sin -mask
		{"clone", "postgres://ro:" + clonePW + "@db/app", "-mask", "x.yaml"},
		{"clone", "postgres://ro@db/app", "-mask", "x.yaml", "-image", "a", "-from", "b"},
	} {
		out, code := runExt(t, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, out)
		}
		if strings.Contains(out, clonePW) {
			t.Errorf("%v: the password was printed", args)
		}
	}
}
