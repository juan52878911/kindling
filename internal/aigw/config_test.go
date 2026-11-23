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
		regs = append(regs, bs...)
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
