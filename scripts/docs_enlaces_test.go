package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Enlaces relativos de los documentos que enseñan el producto: cada destino
// existe. Y docs/README.md enlaza todos los documentos de docs/.

var reEnlace = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func enlacesRelativos(t *testing.T, fichero string) []string {
	t.Helper()
	b, err := os.ReadFile(fichero)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	enBloque := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "```") {
			enBloque = !enBloque
			continue
		}
		if enBloque {
			continue
		}
		for _, m := range reEnlace.FindAllStringSubmatch(l, -1) {
			d := m[1]
			if strings.Contains(d, "://") || strings.HasPrefix(d, "#") || strings.HasPrefix(d, "mailto:") {
				continue
			}
			if i := strings.IndexAny(d, "#?"); i >= 0 {
				d = d[:i]
			}
			if d != "" {
				out = append(out, d)
			}
		}
	}
	return out
}

func TestEnlacesRelativos(t *testing.T) {
	ficheros := []string{"README.md", "README.es.md", "docs/README.md", "docs/quickstart-sandbox.md",
		"examples/sandbox/README.md", "ext/sandbox/README.md"}
	extra, _ := filepath.Glob(filepath.Join("..", "examples", "sandbox", "*", "README.md"))
	for _, e := range extra {
		r, _ := filepath.Rel("..", e)
		ficheros = append(ficheros, filepath.ToSlash(r))
	}
	for _, f := range ficheros {
		for _, d := range enlacesRelativos(t, filepath.Join("..", f)) {
			dest := filepath.Join("..", filepath.Dir(f), filepath.FromSlash(d))
			if _, err := os.Stat(dest); err != nil {
				t.Errorf("%s: el enlace %q no existe", f, d)
			}
		}
	}
}

func TestDocsReadmeEnlazaTodo(t *testing.T) {
	enlazados := map[string]bool{}
	for _, d := range enlacesRelativos(t, filepath.Join("..", "docs", "README.md")) {
		enlazados[filepath.ToSlash(filepath.Clean(d))] = true
	}
	docs, err := filepath.Glob(filepath.Join("..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 40 {
		t.Fatalf("solo %d documentos: ¿se ejecuta desde scripts/?", len(docs))
	}
	for _, d := range docs {
		n := filepath.Base(d)
		if n == "README.md" {
			continue
		}
		if !enlazados[n] {
			t.Errorf("docs/README.md no enlaza %s", n)
		}
	}
}
