package daemon

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// La tabla "Capacidades" de docs/api.md es el contrato que leen las
// extensiones: tiene que nombrar exactamente lo que anuncia GET /info, ni una
// que el daemon no anuncie ni una que falte.
func TestCapabilitiesMatchAPIDoc(t *testing.T) {
	b, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Capacidades")
	if i < 0 {
		t.Fatal("docs/api.md has no Capacidades section")
	}
	doc = doc[i:]
	if j := strings.Index(doc[1:], "\n## "); j >= 0 {
		doc = doc[:j+1]
	}
	fila := regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\|")
	var enDoc []string
	for _, m := range fila.FindAllStringSubmatch(doc, -1) {
		enDoc = append(enDoc, m[1])
	}
	for _, c := range Capabilities {
		if !slices.Contains(enDoc, c) {
			t.Errorf("capability %q is announced but not documented in docs/api.md", c)
		}
	}
	for _, c := range enDoc {
		if !slices.Contains(Capabilities, c) {
			t.Errorf("docs/api.md documents capability %q, which the daemon does not announce", c)
		}
	}
}
