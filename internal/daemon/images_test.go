package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
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
