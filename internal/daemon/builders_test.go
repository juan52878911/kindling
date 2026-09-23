package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func instalarConstructor(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestConstructorExterno(t *testing.T) {
	s, h := testServer(t)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)

	// Un constructor que lee su petición y deja la imagen donde toca.
	instalarConstructor(t, bdir, "toy", `
set -e
grep -q '"answer": 42' "$1/request.json"
echo "building $KLING_IMAGE_NAME on ${BASE_IMAGE:-none}"
echo rootfs > "$KLING_ROOT/images/$KLING_IMAGE_NAME.ext4"
`)
	body := `{"name":"juguete","base":"min","builder":"toy","spec":{"answer":42,"secret":"s3"}}`
	rr := call(t, h, "POST", "/images", body)
	if rr.Code != 200 {
		t.Fatalf("construir con constructor externo: %d %s", rr.Code, rr.Body)
	}
	var res api.BuildImageResult
	json.Unmarshal(rr.Body.Bytes(), &res)
	if !strings.Contains(res.Output, "building juguete on min") {
		t.Fatalf("la salida del constructor tiene que volver: %q", res.Output)
	}
	st, err := os.Stat(s.recipePath("juguete"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("la receta debe existir con 0600 (el spec puede llevar secretos): %v %v", st, err)
	}
	var rec api.ImageRecipe
	b, _ := os.ReadFile(s.recipePath("juguete"))
	json.Unmarshal(b, &rec)
	var spec struct{ Answer int }
	json.Unmarshal(rec.Spec, &spec)
	if rec.Builder != "toy" || rec.Base != "min" || spec.Answer != 42 {
		t.Fatalf("receta: %s", b)
	}
	// El directorio de trabajo se limpia.
	if left, _ := filepath.Glob(filepath.Join(s.root, "build", "juguete.*")); len(left) > 0 {
		t.Fatalf("quedó el directorio de trabajo: %v", left)
	}

	instalarConstructor(t, bdir, "roto", "echo 'no puedo'; exit 3\n")
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"roto"}`); rr.Code != 500 || !strings.Contains(rr.Body.String(), "no puedo") {
		t.Fatalf("un constructor que falla: %d %s", rr.Code, rr.Body)
	}
	instalarConstructor(t, bdir, "vago", "exit 0\n")
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"vago"}`); rr.Code != 500 || !strings.Contains(rr.Body.String(), "left no image") {
		t.Fatalf("un constructor que no deja imagen: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"no-existe"}`); rr.Code != 412 {
		t.Fatalf("constructor no instalado: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"../toy"}`); rr.Code != 412 {
		t.Fatalf("un nombre de constructor con ruta: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"../x","builder":"toy"}`); rr.Code != 400 {
		t.Fatalf("un nombre de imagen con ruta: %d", rr.Code)
	}
}

// Sin el modo inseguro de pruebas, un constructor que no es de root no se
// ejecuta: el daemon lo correría como root.
func TestConstructorDebeSerDeRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("como root los ficheros del test son de root")
	}
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	instalarConstructor(t, bdir, "ajeno", "exit 0\n")
	if _, err := builderPath("ajeno"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("un constructor que no es de root debe rechazarse: %v", err)
	}
}
