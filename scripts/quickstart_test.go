package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// El documento docs/quickstart-sandbox.md y scripts/quickstart-sandbox.sh
// tienen que decir lo mismo: lo que el documento manda teclear es, línea a
// línea, lo que `--dry-run` imprime. Si uno cambia sin el otro, este test falla.

// ordenesDelDocumento devuelve las líneas de órdenes de los bloques ```sh que
// están entre <!-- quickstart:begin --> y <!-- quickstart:end -->, sin líneas
// en blanco ni comentarios.
func ordenesDelDocumento(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "docs", "quickstart-sandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	dentro, enBloque := false, false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(l, "<!-- quickstart:begin -->"):
			dentro = true
		case strings.Contains(l, "<!-- quickstart:end -->"):
			dentro = false
		case dentro && strings.HasPrefix(l, "```sh"):
			enBloque = true
		case dentro && enBloque && strings.HasPrefix(l, "```"):
			enBloque = false
		case dentro && enBloque:
			if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestQuickstartDocumentoIgualScript(t *testing.T) {
	doc := ordenesDelDocumento(t)
	if len(doc) < 8 {
		t.Fatalf("el documento tiene solo %d órdenes entre los marcadores quickstart", len(doc))
	}
	cmd := exec.Command("bash", "quickstart-sandbox.sh", "--dry-run")
	cmd.Env = append(os.Environ(), "KLING_SANDBOX_TOKEN=", "E2B_API_KEY=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--dry-run: %v\n%s", err, out)
	}
	var script []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "+ ") {
			script = append(script, strings.TrimPrefix(l, "+ "))
		}
	}
	if len(script) != len(doc) {
		t.Fatalf("el documento tiene %d órdenes y el script %d\ndoc:\n%s\nscript:\n%s",
			len(doc), len(script), strings.Join(doc, "\n"), strings.Join(script, "\n"))
	}
	for i := range doc {
		if doc[i] != script[i] {
			t.Errorf("orden %d distinta\n doc:    %s\n script: %s", i+1, doc[i], script[i])
		}
	}
}

// El quickstart cubre lo que promete: arranque, token, plantilla, y el primer
// sandbox con curl, Python, TypeScript y el shim de E2B.
func TestQuickstartCubreLosCaminos(t *testing.T) {
	doc := strings.Join(ordenesDelDocumento(t), "\n")
	for _, w := range []string{"kling up", "kling sbx gateway", "templates/python.json", "curl ",
		"kindling_sandbox", "@kindling/sandbox", "kling sbx e2b", "pip install", "npm install"} {
		if !strings.Contains(doc, w) {
			t.Errorf("el quickstart no menciona %q", w)
		}
	}
}

// La plantilla que usan el quickstart y los README de los SDK existe y es
// una receta válida: nombre «python», una imagen y pasos.
func TestPlantillaPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "examples", "sandbox", "templates", "python.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, w := range []string{`"name"`, `"python"`, `"image"`, `"steps"`} {
		if !strings.Contains(s, w) {
			t.Errorf("python.json no tiene %s", w)
		}
	}
}
