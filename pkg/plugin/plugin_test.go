package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dir es un directorio con extensiones de prueba: kling-hello compilada de
// testdata y varias rotas hechas con scripts.
var dir string

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "kling-plugins-")
	if err != nil {
		panic(err)
	}
	dir = d
	out, err := exec.Command("go", "build", "-o", filepath.Join(dir, "kling-hello"), "./testdata/kling-hello").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("building kling-hello: %v\n%s", err, out))
	}
	scripts := map[string]string{
		"kling-broken": "#!/bin/sh\necho 'this is not json'\n",
		"kling-slow":   "#!/bin/sh\nsleep 5\n",
		"kling-old":    "#!/bin/sh\necho '{\"manifest_version\":1,\"name\":\"old\",\"version\":\"1\",\"min_kling\":\"99.0.0\",\"commands\":[{\"name\":\"old\"}]}'\n",
		"kling-liar":   "#!/bin/sh\necho '{\"manifest_version\":1,\"name\":\"someone-else\",\"version\":\"1\",\"commands\":[]}'\n",
		// Estos no son extensiones y no se les debe pedir nada.
		"kling-bridge": "#!/bin/sh\necho called >> \"$0.called\"\n",
		"kling-guest":  "#!/bin/sh\necho called >> \"$0.called\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			panic(err)
		}
	}
	// Un fichero sin permiso de ejecución no cuenta.
	os.WriteFile(filepath.Join(dir, "kling-notexec"), []byte("#!/bin/sh\n"), 0o644)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func discover(t *testing.T) *Registry {
	t.Helper()
	ManifestTimeout = 1 * time.Second
	return Discover(context.Background(), Options{
		Core:    []string{"ps", "run", "status"},
		Version: "0.5.0",
		Path:    []string{dir},
	})
}

func find(r *Registry, name string) *Plugin {
	for _, p := range r.Plugins {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func TestDescubrimiento(t *testing.T) {
	r := discover(t)

	hello := find(r, "hello")
	if hello == nil || hello.Err != nil {
		t.Fatalf("kling-hello: %+v", hello)
	}
	if hello.Manifest.Version != "1.2.3" || !hello.Manifest.HasHook(HookStatus) {
		t.Fatalf("manifiesto de kling-hello mal leído: %+v", hello.Manifest)
	}
	if r.Lookup("hello") != hello || r.Lookup("fail") != hello {
		t.Fatal("los comandos de kling-hello deben enrutarse a ella")
	}
	// Los comandos del núcleo ganan siempre.
	if r.Lookup("ps") != nil || len(hello.Shadowed) != 1 || hello.Shadowed[0] != "ps" {
		t.Fatalf("un comando del núcleo no puede cederse a una extensión: shadowed=%v", hello.Shadowed)
	}

	for name, want := range map[string]string{
		"broken": "valid manifest",
		"slow":   "took more than",
		"old":    "needs kling 99.0.0",
		"liar":   "its manifest says",
	} {
		p := find(r, name)
		if p == nil || p.Err == nil || !strings.Contains(p.Err.Error(), want) {
			t.Errorf("kling-%s: quería un error con %q, salió %+v", name, want, p)
		}
		if r.Lookup(name) != nil {
			t.Errorf("una extensión inutilizable (%s) no puede recibir comandos", name)
		}
	}
	for _, name := range []string{"bridge", "guest", "notexec"} {
		if find(r, name) != nil {
			t.Errorf("kling-%s no es una extensión y no debe listarse", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "kling-bridge.called")); err == nil {
		t.Error("se ejecutó kling-bridge para pedirle un manifiesto")
	}
}

// La primera extensión con un nombre gana: una copia más atrás en la búsqueda
// no la sustituye.
func TestLaPrimeraGana(t *testing.T) {
	otro := t.TempDir()
	os.WriteFile(filepath.Join(otro, "kling-hello"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	r := Discover(context.Background(), Options{Version: "0.5.0", Path: []string{dir, otro}})
	if p := find(r, "hello"); p == nil || p.Path != filepath.Join(dir, "kling-hello") {
		t.Fatalf("ganó la que no era: %+v", p)
	}
}

func TestEjecutarComoHijo(t *testing.T) {
	r := discover(t)
	hello := r.Lookup("hello")

	// La salida de un hijo va a la terminal del padre: se redirige para leerla.
	old := os.Stdout
	rd, wr, _ := os.Pipe()
	os.Stdout = wr
	code, err := RunChild(hello, "hello", []string{"mundo"}, "/tmp/cfg.json")
	wr.Close()
	os.Stdout = old
	out, _ := io.ReadAll(rd)
	if err != nil || code != 0 {
		t.Fatalf("hello: code=%d err=%v", code, err)
	}
	if got := strings.TrimSpace(string(out)); got != "hola mundo api=1 config=/tmp/cfg.json" {
		t.Fatalf("la extensión no recibió argumentos o entorno: %q", got)
	}

	// El código de salida llega intacto: `mcp heal` depende de ello.
	code, err = RunChild(hello, "fail", nil, "")
	if err != nil || code != 7 {
		t.Fatalf("fail: code=%d err=%v, quería 7", code, err)
	}
}

func TestGanchos(t *testing.T) {
	r := discover(t)
	hello := r.Lookup("hello")

	out, err := RunHook(context.Background(), hello, HookStatus, nil, "")
	if err != nil || strings.TrimSpace(string(out)) != "hello:        ✓ fine" {
		t.Fatalf("status: %q %v", out, err)
	}
	out, err = RunHook(context.Background(), hello, HookStatus, []string{"-json"}, "")
	var obj map[string]bool
	if err != nil || json.Unmarshal(out, &obj) != nil || !obj["ok"] {
		t.Fatalf("status -json: %q %v", out, err)
	}
	// Un gancho que se cuelga no cuelga a kling.
	HookTimeout = 300 * time.Millisecond
	defer func() { HookTimeout = 5 * time.Second }()
	t0 := time.Now()
	_, err = RunHook(context.Background(), hello, HookUp, nil, "")
	if err == nil || !strings.Contains(err.Error(), "took more than") || time.Since(t0) > 3*time.Second {
		t.Fatalf("un gancho colgado debe cortarse por plazo: err=%v tras %s", err, time.Since(t0))
	}
	if got := r.WithHook(HookStatus); len(got) != 1 || got[0] != hello {
		t.Fatalf("WithHook(status): %v", got)
	}
}

func TestIncorporadas(t *testing.T) {
	var llamado []string
	b := &Builtin{
		Manifest: Manifest{ManifestVersion: 1, Name: "mcp", Version: "0.5.0",
			Commands: []Command{{Name: "mcp"}, {Name: "run", TopLevel: true}}, Hooks: []string{HookStatus}},
		Commands: map[string]func([]string) error{
			"mcp": func(a []string) error { llamado = a; return &ExitError{Code: 3} },
		},
		Hooks: map[string]func([]string, io.Writer) error{
			HookStatus: func(_ []string, w io.Writer) error { _, err := io.WriteString(w, "mcp: ok\n"); return err },
		},
	}
	r := Discover(context.Background(), Options{Core: []string{"run"}, Builtins: []*Builtin{b}, Path: []string{}})
	p := r.Lookup("mcp")
	if p == nil || r.Lookup("run") != nil {
		t.Fatalf("incorporada mal registrada")
	}
	err := Exec(p, "mcp", []string{"heal"}, "")
	if ExitCode(err) != 3 || len(llamado) != 1 || llamado[0] != "heal" {
		t.Fatalf("una incorporada corre en proceso y conserva su código: code=%d args=%v", ExitCode(err), llamado)
	}
	out, err := RunHook(context.Background(), p, HookStatus, nil, "")
	if err != nil || string(out) != "mcp: ok\n" {
		t.Fatalf("gancho incorporado: %q %v", out, err)
	}
	// Una incorporada va antes que una externa con el mismo nombre.
	r = Discover(context.Background(), Options{Builtins: []*Builtin{b}, Path: []string{dir}})
	if find(r, "mcp").Builtin == nil {
		t.Fatal("la incorporada debe ganar")
	}
}

func TestServe(t *testing.T) {
	m := Manifest{Name: "x", Version: "1", Commands: []Command{{Name: "a"}}}
	cmds := map[string]func([]string) error{
		"a": func([]string) error { return &ExitError{Code: 4, Err: errors.New("boom")} },
	}
	var out, errb bytes.Buffer
	if code := serve(m, cmds, nil, []string{"--kling-manifest"}, &out, &errb); code != 0 {
		t.Fatalf("--kling-manifest: %d", code)
	}
	var got Manifest
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.ManifestVersion != ManifestVersion || got.Validate() != nil {
		t.Fatalf("el manifiesto impreso no vale: %s (%v)", out.Bytes(), err)
	}
	if code := serve(m, cmds, nil, []string{"a"}, &out, &errb); code != 4 {
		t.Fatalf("un ExitError debe dar su código: %d", code)
	}
	if code := serve(m, cmds, nil, []string{"zzz"}, &out, &errb); code != 2 {
		t.Fatalf("comando desconocido: %d", code)
	}
	if code := serve(m, cmds, nil, []string{"--kling-hook", "status"}, &out, &errb); code != 2 {
		t.Fatalf("gancho no implementado: %d", code)
	}
}

func TestVersionAtLeast(t *testing.T) {
	casos := []struct {
		have, want string
		ok         bool
	}{
		{"0.6.0", "0.6.0", true},
		{"v0.6.1", "0.6.0", true},
		{"0.5.9", "0.6.0", false},
		{"v0.6.0-3-gabc123", "0.6.0", true},
		{"1.0", "0.9.9", true},
		{"dev", "9.9.9", true}, // compilado a mano: no se le bloquea
		{"0.1.0", "", true},
	}
	for _, c := range casos {
		if got := VersionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("VersionAtLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
}

func TestValidate(t *testing.T) {
	malos := []Manifest{
		{ManifestVersion: 3, Name: "a"},
		{ManifestVersion: 0, Name: "a"},
		{ManifestVersion: 1, Name: "Mayus"},
		{ManifestVersion: 2, Name: "a", Commands: []Command{{Name: "a"}, {Name: "x"}}},
		{ManifestVersion: 1, Name: "a", Commands: []Command{{Name: "x"}, {Name: "x"}}},
		{ManifestVersion: 1, Name: "a", Hooks: []string{"reboot"}},
		{ManifestVersion: 1, Name: "a", Config: []ConfigKey{{Key: "k", Type: "float"}}},
	}
	for i, m := range malos {
		if m.Validate() == nil {
			t.Errorf("manifiesto %d debió rechazarse: %+v", i, m)
		}
	}
}
