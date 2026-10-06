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

// El nombre sale de la referencia y lleva un sufijo que cambia con la
// referencia entera y con el entorno (no con su orden), que depende de la
// clave y no revela claves ni valores.
func TestNombreParaRef(t *testing.T) {
	ref := func(s string) oci.ImageRef {
		r, err := oci.ParseImageRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	clave := []byte("clave de prueba de 32 bytes.....")
	r := ref("postgres:17-alpine")
	sin := nombreParaRef(r, nil, clave)
	a := nombreParaRef(r, []string{"POSTGRES_PASSWORD=secreta", "PGDATA=/x"}, clave)
	b := nombreParaRef(r, []string{"PGDATA=/x", "POSTGRES_PASSWORD=secreta"}, clave)
	c := nombreParaRef(r, []string{"POSTGRES_PASSWORD=otra"}, clave)
	if !strings.HasPrefix(sin, "postgres-17-alpine-") || len(sin) != len("postgres-17-alpine-")+8 {
		t.Errorf("sin entorno: %q", sin)
	}
	if a != b {
		t.Errorf("el orden del entorno cambia el nombre: %q != %q", a, b)
	}
	if a == c || a == sin {
		t.Errorf("entornos distintos con el mismo nombre: %q %q %q", a, c, sin)
	}
	for _, n := range []string{a, c} {
		for _, s := range []string{"secreta", "otra", "PASSWORD", "PGDATA"} {
			if strings.Contains(n, s) {
				t.Errorf("%q revela %q", n, s)
			}
		}
	}
	// Otra referencia con el mismo nombre corto, o la misma fijada por
	// digest, no reutiliza la imagen.
	d := "@sha256:" + strings.Repeat("ab", 32)
	for _, otra := range []string{"ghcr.io/evil/postgres:17-alpine", "postgres:17-alpine" + d} {
		if nombreParaRef(ref(otra), nil, clave) == sin {
			t.Errorf("%s se llama igual que postgres:17-alpine", otra)
		}
	}
	if nombreParaRef(r, nil, []byte("otra clave")) == sin {
		t.Error("el sufijo no depende de la clave")
	}
}
