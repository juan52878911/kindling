package net

import (
	"strings"
	"testing"
)

// Un -allow con un sufijo multiinquilino o público tiene que avisar (permite
// cualquier sitio de terceros bajo él, y la allowlist filtra por IP); un
// dominio corriente, no: un aviso que salta siempre no lo lee nadie.
func TestAvisosAllowMultiinquilino(t *testing.T) {
	casos := []struct {
		dominio string
		avisa   string // subcadena del aviso; "" = sin aviso
	}{
		// El sufijo en sí.
		{"github.io", "shared hosting suffix"},
		{"amazonaws.com", "shared hosting suffix"},
		{"herokuapp.com", "shared hosting suffix"},
		{"cloudfront.net", "shared hosting suffix"},
		{"vercel.app", "shared hosting suffix"},
		{"pages.dev", "shared hosting suffix"},
		{"azurewebsites.net", "shared hosting suffix"},
		{"appspot.com", "shared hosting suffix"},
		{"blob.core.windows.net", "shared hosting suffix"},
		// Normalización: mayúsculas, punto final, espacios y comodín.
		{" GitHub.IO. ", "shared hosting suffix"},
		{"*.vercel.app", "shared hosting suffix"},
		// Un sitio concreto bajo uno de ellos.
		{"juan.github.io", "shared infrastructure (github.io)"},
		{"mibucket.s3.amazonaws.com", "shared infrastructure (amazonaws.com)"},
		{"d111111abcdef8.cloudfront.net", "shared infrastructure (cloudfront.net)"},
		// Sufijos públicos.
		{"com", "public suffix"},
		{"io", "public suffix"},
		{"co.uk", "public suffix"},
		{"com.co", "public suffix"},
		// Dominios normales: sin aviso.
		{"github.com", ""},
		{"api.github.com", ""},
		{"pypi.org", ""},
		{"registry.npmjs.org", ""},
		{"example.co.uk", ""},
		{"api.anthropic.com", ""},
		{"googleapis.com", ""},
		{"notgithub.io.example.com", ""},
		{"evilgithub.io", ""},
	}
	for _, c := range casos {
		av := AvisosAllowMultiinquilino([]string{c.dominio})
		switch {
		case c.avisa == "" && len(av) != 0:
			t.Errorf("%q: no debía avisar y avisa: %q", c.dominio, av)
		case c.avisa != "" && len(av) != 1:
			t.Errorf("%q: debía dar un aviso y da %d: %q", c.dominio, len(av), av)
		case c.avisa != "" && !strings.Contains(av[0], c.avisa):
			t.Errorf("%q: el aviso %q no dice %q", c.dominio, av[0], c.avisa)
		}
	}
}

// Varios dominios: un aviso por cada uno que lo merece, sin repetir.
func TestAvisosAllowMultiinquilinoLista(t *testing.T) {
	av := AvisosAllowMultiinquilino([]string{"github.com", "github.io", "GITHUB.IO", "", "pypi.org", "x.herokuapp.com"})
	if len(av) != 2 {
		t.Fatalf("avisos = %d, quería 2: %q", len(av), av)
	}
	if AvisosAllowMultiinquilino(nil) != nil {
		t.Fatal("sin dominios no hay avisos")
	}
}
