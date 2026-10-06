package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
)

// Un nombre de kindling nunca es una referencia; una referencia lleva
// etiqueta, registro o digest.
func TestEsRefDocker(t *testing.T) {
	for in, want := range map[string]bool{
		"hs-full2":                       false,
		"default":                        false,
		"redis:7-alpine":                 true,
		"postgres:17-alpine":             true,
		"ghcr.io/vectorize-io/hindsight": true,
		"docker.io/library/nginx":        true,
		"nginx@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": true,
	} {
		if got := esRefDocker(in); got != want {
			t.Errorf("%q: %v, quería %v", in, got, want)
		}
	}
}

// El nombre sale de la referencia y, con entorno, lleva un sufijo que cambia
// con el entorno y no con su orden, y no revela claves ni valores.
func TestNombreParaRef(t *testing.T) {
	r, err := oci.ParseImageRef("postgres:17-alpine")
	if err != nil {
		t.Fatal(err)
	}
	sin := nombreParaRef(r, nil)
	a := nombreParaRef(r, []string{"POSTGRES_PASSWORD=secreta", "PGDATA=/x"})
	b := nombreParaRef(r, []string{"PGDATA=/x", "POSTGRES_PASSWORD=secreta"})
	c := nombreParaRef(r, []string{"POSTGRES_PASSWORD=otra"})
	if sin != "postgres-17-alpine" {
		t.Errorf("sin entorno: %q", sin)
	}
	if a != b {
		t.Errorf("el orden del entorno cambia el nombre: %q != %q", a, b)
	}
	if a == c || a == sin {
		t.Errorf("entornos distintos con el mismo nombre: %q %q %q", a, c, sin)
	}
	for _, n := range []string{a, c} {
		if len(n) != len(sin)+7 {
			t.Errorf("sufijo de %q", n)
		}
		for _, s := range []string{"secreta", "otra", "PASSWORD", "PGDATA"} {
			if strings.Contains(n, s) {
				t.Errorf("%q revela %q", n, s)
			}
		}
	}
}
