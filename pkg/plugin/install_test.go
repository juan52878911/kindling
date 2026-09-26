package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// script es una "extensión" de mentira: un sh que imprime manifest con
// --kling-manifest. Basta para probar la instalación sin compilar nada.
func script(manifest string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = --kling-manifest ]; then echo '" + manifest + "'; exit 0; fi\necho ran \"$@\"\n")
}

func sumOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func asset(bin string) string { return bin + "-" + runtime.GOOS + "-" + runtime.GOARCH }

// release sirve una release falsa en /v1.0.0/: los assets que se le den y un
// SHA256SUMS con sus hashes. sums permite estropear o quitar entradas.
type release struct {
	files map[string][]byte
	sums  map[string]string // nombre -> hash; nil = los reales
	noSum bool              // sin SHA256SUMS (404)
}

func (rel *release) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/v1.0.0/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == "SHA256SUMS" {
			if rel.noSum {
				http.NotFound(w, r)
				return
			}
			sums := rel.sums
			if sums == nil {
				sums = map[string]string{}
				for n, b := range rel.files {
					sums[n] = sumOf(b)
				}
			}
			for n, h := range sums {
				fmt.Fprintf(w, "%s  %s\n", h, n)
			}
			return
		}
		b, ok := rel.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
}

const demoManifest = `{"manifest_version":1,"name":"demo","version":"2.0.0","commands":[{"name":"demo"}],"companions":["kling-demo-helper"]}`

func demoRelease() *release {
	return &release{files: map[string][]byte{
		asset("kling-demo"):        script(demoManifest),
		asset("kling-demo-helper"): []byte("#!/bin/sh\necho helper\n"),
	}}
}

func install(t *testing.T, rel *release, o InstallOptions) (*Installed, string, error) {
	t.Helper()
	ManifestTimeout = 10 * time.Second
	srv := httptest.NewTLSServer(rel.handler())
	t.Cleanup(srv.Close)
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.Name == "" {
		o.Name = "demo"
	}
	if o.CoreVersion == "" {
		o.CoreVersion = "1.0.0"
	}
	o.Client = srv.Client()
	o.ReleaseURL = srv.URL
	if o.From != "" {
		o.From = srv.URL + o.From
	}
	res, err := Install(context.Background(), o)
	return res, o.Dir, err
}

// lsDir son los nombres de dir, para comprobar que un fallo no deja nada.
func lsDir(t *testing.T, dir string) []string {
	t.Helper()
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestInstalarDesdeLaRelease(t *testing.T) {
	res, dir, err := install(t, demoRelease(), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "kling-demo")
	st, err := os.Stat(bin)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("binario: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "kling-demo-helper")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("el compañero tiene que instalarse ejecutable: %v", err)
	}
	sc, err := ReadSidecar(bin)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "demo" || sc.Version != "2.0.0" || sc.SHA256 != sumOf(script(demoManifest)) ||
		!strings.HasSuffix(sc.URL, "/v1.0.0/"+asset("kling-demo")) || sc.Installed.IsZero() {
		t.Fatalf("kling-demo.json: %+v", sc)
	}
	if res.Replaced != nil || len(res.Companions) != 1 {
		t.Fatalf("resultado: %+v", res)
	}
	if got := strings.Join(lsDir(t, dir), " "); got != "kling-demo kling-demo-helper kling-demo.json" {
		t.Fatalf("quedan temporales o sobra algo: %s", got)
	}

	// El compañero no es una extensión: no se le pide manifiesto ni se lista.
	r := Discover(context.Background(), Options{Path: []string{dir}, Version: "1.0.0"})
	if len(r.Plugins) != 1 || r.Plugins[0].Name != "demo" || r.Plugins[0].Err != nil {
		for _, p := range r.Plugins {
			t.Logf("%s: %v", p.Name, p.Err)
		}
		t.Fatal("tras instalar solo tiene que verse demo, y sin error")
	}

	// Reinstalar es actualizar: dice qué había.
	res, _, err = install(t, demoRelease(), InstallOptions{Dir: dir})
	if err != nil || res.Replaced == nil || res.Replaced.Version != "2.0.0" {
		t.Fatalf("reinstalar: %+v %v", res, err)
	}

	// rm quita binario, .json y compañero, y nada más.
	os.WriteFile(filepath.Join(dir, "otro"), nil, 0o644)
	removed, err := Remove(dir, "demo")
	if err != nil || len(removed) != 3 {
		t.Fatalf("Remove: %v %v", removed, err)
	}
	if got := strings.Join(lsDir(t, dir), " "); got != "otro" {
		t.Fatalf("tras rm queda: %s", got)
	}
	if _, err := Remove(dir, "demo"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("rm de algo que no está: %v", err)
	}
}

// Un compañero que usa otra extensión instalada no se borra con la primera.
func TestRmRespetaCompanerosCompartidos(t *testing.T) {
	_, dir, err := install(t, demoRelease(), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(SidecarPath(filepath.Join(dir, "kling-demo")))
	os.WriteFile(filepath.Join(dir, "kling-otra.json"), []byte(strings.Replace(string(b), `"demo"`, `"otra"`, 1)), 0o644)
	if _, err := Remove(dir, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "kling-demo-helper")); err != nil {
		t.Fatal("el compañero lo sigue usando otra extensión")
	}
}

func TestRmNoTocaLoDeFuera(t *testing.T) {
	fuera := t.TempDir()
	os.WriteFile(filepath.Join(fuera, "kling-ajena"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("KLING_PLUGIN_PATH", fuera)
	_, err := Remove(t.TempDir(), "ajena")
	if err == nil || !strings.Contains(err.Error(), "outside the extensions directory") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fuera, "kling-ajena")); err != nil {
		t.Fatal("no se puede borrar lo que no instaló kling")
	}
}

func TestHashQueNoCoincide(t *testing.T) {
	rel := demoRelease()
	rel.sums = map[string]string{
		asset("kling-demo"):        strings.Repeat("0", 64),
		asset("kling-demo-helper"): sumOf(rel.files[asset("kling-demo-helper")]),
	}
	_, dir, err := install(t, rel, InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got := lsDir(t, dir); len(got) != 0 {
		t.Fatalf("un hash malo no puede dejar nada escrito: %v", got)
	}

	// -sha256 también se comprueba, aunque SHA256SUMS esté bien.
	_, _, err = install(t, demoRelease(), InstallOptions{SHA256: strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("con -sha256 malo: %v", err)
	}
}

func TestCompaneroSinVerificarSeRechaza(t *testing.T) {
	rel := demoRelease()
	rel.sums = map[string]string{asset("kling-demo"): sumOf(rel.files[asset("kling-demo")])}
	_, dir, err := install(t, rel, InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "companion kling-demo-helper") {
		t.Fatalf("err = %v", err)
	}
	if got := lsDir(t, dir); len(got) != 0 {
		t.Fatalf("una instalación a medias no puede quedar: %v", got)
	}
}

func TestManifiestoInvalido(t *testing.T) {
	for name, body := range map[string][]byte{
		"basura":     script(`no es json`),
		"otro":       script(`{"manifest_version":1,"name":"impostor","version":"1","commands":[]}`),
		"compañero":  script(`{"manifest_version":1,"name":"demo","version":"1","commands":[],"companions":["../../bin/sh"]}`),
		"min_kling":  script(`{"manifest_version":1,"name":"demo","version":"1","min_kling":"9.0.0","commands":[]}`),
		"no arranca": []byte("\x7fELFbasura"),
	} {
		t.Run(name, func(t *testing.T) {
			rel := &release{files: map[string][]byte{asset("kling-demo"): body}}
			_, dir, err := install(t, rel, InstallOptions{})
			if err == nil {
				t.Fatal("tenía que rechazarse")
			}
			if got := lsDir(t, dir); len(got) != 0 {
				t.Fatalf("quedó algo: %v", got)
			}
		})
	}
}

func TestSoloHTTPS(t *testing.T) {
	rel := &release{files: map[string][]byte{asset("kling-demo"): script(`{"manifest_version":1,"name":"demo","version":"1","commands":[]}`)}}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()
	o := InstallOptions{Name: "demo", CoreVersion: "1.0.0", Dir: t.TempDir(), From: srv.URL + "/v1.0.0/" + asset("kling-demo")}
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http tiene que rechazarse: %v", err)
	}
	o.ReleaseURL, o.From = srv.URL, ""
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("una release por http también: %v", err)
	}
	// Solo la opción interna de los tests lo deja pasar.
	o.allowHTTP = true
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatalf("con allowHTTP: %v", err)
	}
}

// Una redirección de https a http es lo mismo que pedir http.
func TestRedireccionAHTTPSeRechaza(t *testing.T) {
	plano := httptest.NewServer(http.NotFoundHandler())
	defer plano.Close()
	rel := demoRelease()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			rel.handler().ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, plano.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	_, err := Install(context.Background(), InstallOptions{Name: "demo", CoreVersion: "1.0.0", Dir: t.TempDir(),
		Client: srv.Client(), ReleaseURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromConSha256SinSums(t *testing.T) {
	body := script(`{"manifest_version":1,"name":"demo","version":"1","commands":[]}`)
	rel := &release{files: map[string][]byte{"cualquier-nombre": body}, noSum: true}
	if _, _, err := install(t, rel, InstallOptions{From: "/v1.0.0/cualquier-nombre"}); err == nil || !strings.Contains(err.Error(), "-sha256") {
		t.Fatalf("sin SHA256SUMS ni -sha256 no se instala: %v", err)
	}
	res, _, err := install(t, rel, InstallOptions{From: "/v1.0.0/cualquier-nombre", SHA256: strings.ToUpper(sumOf(body))})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Sidecar.URL, "/v1.0.0/cualquier-nombre") {
		t.Fatalf("url: %s", res.Sidecar.URL)
	}
}

func TestDesdeFichero(t *testing.T) {
	src := t.TempDir()
	for n, b := range demoRelease().files {
		os.WriteFile(filepath.Join(src, n), b, 0o644)
	}
	dst := t.TempDir()
	ManifestTimeout = 10 * time.Second
	res, err := Install(context.Background(), InstallOptions{Name: "demo", CoreVersion: "dev",
		File: filepath.Join(src, asset("kling-demo")), Dir: dst})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Sidecar.URL, "file://") || len(res.Companions) != 1 {
		t.Fatalf("%+v", res)
	}
	// Con un SHA256SUMS al lado, se usa.
	os.WriteFile(filepath.Join(src, "SHA256SUMS"), []byte(strings.Repeat("1", 64)+"  "+asset("kling-demo")+"\n"), 0o644)
	_, err = Install(context.Background(), InstallOptions{Name: "demo", File: filepath.Join(src, asset("kling-demo")), Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestSinEtiquetaEnDesarrollo(t *testing.T) {
	for _, v := range []string{"dev", "v0.12.0-5-gabc123", "0.13.0-dirty", ""} {
		_, err := Install(context.Background(), InstallOptions{Name: "demo", CoreVersion: v, Dir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "@v") {
			t.Errorf("%q: %v", v, err)
		}
	}
	if DevVersion("v0.13.0") || DevVersion("0.13.0") {
		t.Error("una versión de release sirve de etiqueta")
	}
}

func TestInstallDir(t *testing.T) {
	t.Setenv("KLING_PLUGIN_PATH", "/uno"+string(os.PathListSeparator)+"/dos")
	if d, _ := InstallDir(); d != "/uno" {
		t.Errorf("KLING_PLUGIN_PATH: %s", d)
	}
	t.Setenv("KLING_PLUGIN_PATH", "")
	t.Setenv("XDG_DATA_HOME", "/datos")
	if d, _ := InstallDir(); d != filepath.Join("/datos", "kling", "plugins") {
		t.Errorf("XDG_DATA_HOME: %s", d)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/casa")
	if d, _ := InstallDir(); d != filepath.Join("/casa", ".local", "share", "kling", "plugins") {
		t.Errorf("HOME: %s", d)
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	got := ParseSums([]byte(h + "  kling-mcp-linux-amd64\n" + strings.ToUpper(h) + " *dist/kling-x\nbasura\n"))
	if got["kling-mcp-linux-amd64"] != h || got["kling-x"] != h || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

// Una extensión desactivada se lista pero no sirve comandos; a una externa ni
// se la ejecuta, y a una incorporada tampoco se le enruta nada.
func TestDesactivadas(t *testing.T) {
	d := t.TempDir()
	marca := filepath.Join(d, "ejecutada")
	os.WriteFile(filepath.Join(d, "kling-apagada"), []byte("#!/bin/sh\ntouch "+marca+"\n"), 0o755)
	b := &Builtin{Manifest: Manifest{ManifestVersion: 1, Name: "ai", Version: "1",
		Commands: []Command{{Name: "ai"}, {Name: "ask"}}, Hooks: []string{HookStatus}}}
	r := Discover(context.Background(), Options{
		Builtins: []*Builtin{b}, Path: []string{d}, Disabled: []string{"ai", "apagada"},
	})
	if len(r.Plugins) != 2 {
		t.Fatalf("las desactivadas se listan igual: %d", len(r.Plugins))
	}
	for _, p := range r.Plugins {
		if !p.Disabled || p.Err == nil || !strings.Contains(p.Err.Error(), "kling plugins enable "+p.Name) {
			t.Errorf("%s: disabled=%v err=%v", p.Name, p.Disabled, p.Err)
		}
	}
	if r.Lookup("ai") != nil || r.Lookup("ask") != nil || len(r.Commands()) != 0 || len(r.WithHook(HookStatus)) != 0 {
		t.Fatal("una desactivada no recibe comandos ni ganchos")
	}
	if r.DisabledFor("ask") == nil || r.DisabledFor("apagada") == nil || r.DisabledFor("ps") != nil {
		t.Fatal("DisabledFor tiene que encontrar a quien serviría el comando")
	}
	if _, err := os.Stat(marca); err == nil {
		t.Fatal("a una externa desactivada no se le pide el manifiesto")
	}
	if err := Exec(r.Plugins[0], "ai", nil, ""); err == nil {
		t.Fatal("Exec de una desactivada tiene que fallar")
	}
}

func TestCompanionsEnElManifiesto(t *testing.T) {
	m := Manifest{ManifestVersion: 1, Name: "mcp", Version: "1", Companions: []string{"kling-bridge"}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"bridge", "kling-../x", "kling-Bridge", "kling-mcp", "kling-a/b"} {
		m.Companions = []string{c}
		if m.Validate() == nil {
			t.Errorf("%q tenía que ser inválido", c)
		}
	}
}
