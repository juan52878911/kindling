package api

import (
	"strings"
	"testing"
)

// Lo que el resolver del allowlist no sabe casar se rechaza al pedirlo, con un
// mensaje que dice qué escribir: antes fallaba cerrado y en silencio.
func TestValidateAllowDomainsNombres(t *testing.T) {
	for _, c := range []struct {
		dominio string
		vale    bool
		pista   string
	}{
		{"example.com", true, ""},
		{"API.GitHub.com.", true, ""},
		{" pypi.org ", true, ""},
		{"_dmarc.example.com", true, ""},
		{"xn--bcher-kva.example", true, ""},
		{"localhost", true, ""},
		{"", true, ""},
		{"*.a.com", false, `write "a.com"`},
		{"*", false, "wildcards"},
		{"bücher.example", false, "punycode"},
		{"93.184.216.34", false, "not IP addresses"},
		{"[2001:db8::1]", false, "not IP addresses"},
		{"https://example.com", false, "not a domain name"},
		{"example.com/path", false, "not a domain name"},
		{"-a.com", false, "not a domain name"},
		{"a..com", false, "not a domain name"},
		{strings.Repeat("a", 64) + ".com", false, "not a domain name"},
	} {
		err := ValidateAllowDomains([]string{"ok.com", c.dominio})
		if (err == nil) != c.vale {
			t.Errorf("%q: err = %v, quería válido=%v", c.dominio, err, c.vale)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.pista) {
			t.Errorf("%q: el error %q no dice %q", c.dominio, err, c.pista)
		}
	}
}

// Un nodo de grafo pasa por la misma comprobación que run.
func TestValidarNodoAllowDomains(t *testing.T) {
	n := &GraphNode{Image: "x", Egress: "allowlist", AllowDomains: []string{"*.a.com"}}
	if err := validarNodo("web", n); err == nil || !strings.Contains(err.Error(), "wildcards") {
		t.Fatalf("validarNodo con *.a.com = %v; quería el error de comodines", err)
	}
}
