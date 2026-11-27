package aigw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// bloquesJSONDeDocs saca de un .md los bloques ```json que son un registro
// entero (empiezan por { y tienen "models").
func bloquesJSONDeDocs(t *testing.T, ruta string) []string {
	t.Helper()
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var cur []string
	dentro := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case !dentro && strings.HasPrefix(l, "```json"):
			dentro, cur = true, nil
		case dentro && strings.HasPrefix(l, "```"):
			dentro = false
			s := strings.TrimSpace(strings.Join(cur, "\n"))
			if strings.HasPrefix(s, "{") && strings.Contains(s, `"models"`) {
				out = append(out, s)
			}
		case dentro:
			cur = append(cur, l)
		}
	}
	return out
}

// esBloqueHTTP dice si un registro de los docs tiene algún modelo kind http.
func esBloqueHTTP(src string) bool {
	var c Config
	if json.Unmarshal([]byte(src), &c) != nil {
		return false
	}
	for _, m := range c.Models {
		if m != nil && m.Kind == KindHTTP {
			return true
		}
	}
	return false
}

// TestConfigHTTPDeDocs: los ejemplos de kind http de los docs cargan con
// LoadConfig (las credenciales de auth, como ficheros 0600 junto al ai.json)
// y cada modelo kind http tiene su backend.
func TestConfigHTTPDeDocs(t *testing.T) {
	n := 0
	for _, md := range []string{"ai-gateway.md", "chispa-serverless.md"} {
		for _, src := range bloquesJSONDeDocs(t, filepath.Join("..", "..", "docs", md)) {
			if !esBloqueHTTP(src) {
				continue
			}
			n++
			var crudo Config
			if err := json.Unmarshal([]byte(src), &crudo); err != nil {
				t.Fatalf("%s: %v", md, err)
			}
			dir := t.TempDir()
			for _, m := range crudo.Models {
				if m.Auth != nil && m.Auth.File != "" {
					if err := os.WriteFile(filepath.Join(dir, m.Auth.File), []byte("clave\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			p := filepath.Join(dir, "ai.json")
			if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(p)
			if err != nil {
				t.Fatalf("%s: a kind http example does not load: %v\n%s", md, err, src)
			}
			for name, m := range crudo.Models {
				if m.Kind == KindHTTP && c.Backend(name) == nil {
					t.Errorf("%s: model %q has no backend", md, name)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no kind http example in the docs")
	}
}

// TestConfigViejoIntacto: todo registro de hoy (los de docs/ y los de los
// tests) carga igual que antes de la ronda 8 —ParseConfig no toca los
// ModelConfig— y cada modelo de réplica tiene su backend expandido con el
// mismo dorado. Es la condición de fusión de Z1 (plan §5, riesgo 9).
func TestConfigViejoIntacto(t *testing.T) {
	var regs []string
	for _, md := range []string{"ai-gateway.md", "chispa-serverless.md", "intent.md", "mejora-continua.md"} {
		bs := bloquesJSONDeDocs(t, filepath.Join("..", "..", "docs", md))
		if len(bs) == 0 {
			t.Fatalf("%s: no registry block found", md)
		}
		for _, b := range bs {
			// Los ejemplos de kind http son de la ronda 8: no son registros
			// de antes (los cubre TestConfigHTTPDeDocs).
			if !esBloqueHTTP(b) {
				regs = append(regs, b)
			}
		}
	}
	// Los de los tests: los que usan gateway_test.go, intent_test.go y
	// chispaguest_test.go, con todos los kinds de hoy.
	regs = append(regs,
		`{"models":{"j":{"kind":"chispa","path":"m.chispa"},"v":{"kind":"von","snapshot":"von-smol"}},
		  "tasks":{"t":{"chispa":"j","escalate_to":"v"}}}`,
		`{"models":{"c":{"kind":"chispa","backend":"microvm","snapshot":"commits","max_replicas":3}},
		  "tasks":{"t":{"chispa":"c"}}}`,
		`{"models":{"e":{"kind":"embed","snapshot":"enc-e5","max_replicas":1},"v":{"kind":"von","snapshot":"s"}},
		  "tasks":{"g":{"von":"v","max_tokens":64}}}`,
	)
	for i, src := range regs {
		var crudo Config
		if err := json.Unmarshal([]byte(src), &crudo); err != nil {
			t.Fatalf("registry %d: %v", i, err)
		}
		c, err := ParseConfig(strings.NewReader(src))
		if err != nil {
			t.Fatalf("registry %d does not load any more: %v\n%s", i, err, src)
		}
		if !reflect.DeepEqual(c.Models, crudo.Models) {
			t.Errorf("registry %d: ParseConfig changed the models", i)
		}
		replicas := 0
		for n, m := range c.Models {
			b := c.Backend(n)
			if !m.isReplica() {
				if b != nil {
					t.Errorf("registry %d: in-process chispa %q has a backend", i, n)
				}
				continue
			}
			replicas++
			if b == nil {
				t.Errorf("registry %d: model %q (%s) has no backend", i, n, m.Kind)
				continue
			}
			if b.Name != n || b.Snapshot != m.Snapshot || b.Upstream != "" || b.Port != DefaultBackendPort ||
				b.MaxReplicas != m.MaxReplicas || b.Auth != nil || len(b.Tenants) != 0 || b.UpstreamModel != "" {
				t.Errorf("registry %d: model %q expands to %+v", i, n, b)
			}
		}
		if got := len(c.Backends()); got != replicas {
			t.Errorf("registry %d: Backends() has %d, want %d", i, got, replicas)
		}
	}
}
