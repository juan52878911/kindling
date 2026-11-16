package main

import (
	"strings"
	"testing"
)

// S12: la interpolación va por escalar, después de parsear. Un valor con
// " #", ": " o un salto de línea llega entero a su variable y no añade
// claves; ni en un valor sin comillas.
func TestInterpolarTrasParsear(t *testing.T) {
	for _, valor := range []string{"s3cr #et", "a: b", "uno\nOTRA: dos", "x # y: z", "fin #"} {
		src := "services:\n  db:\n    image: postgres:17\n    environment:\n      DB_PASS: ${P}\n      USER: \"${P}\"\n"
		lookup := func(k string) (string, bool) { return valor, k == "P" }
		tr, err := traducirCompose([]byte(src), "dev", false, lookup)
		if err != nil {
			t.Fatalf("%q: %v", valor, err)
		}
		env := tr.vivos[0].env
		if len(env) != 2 || env[0] != [2]string{"DB_PASS", valor} || env[1] != [2]string{"USER", valor} {
			t.Errorf("%q: env = %q", valor, env)
		}
	}
}

// S12: un image: que no es una referencia de imagen (uno que empieza por
// "-" iría como bandera a kling image import) es un error de la traducción.
func TestImagenNoEsReferencia(t *testing.T) {
	for _, img := range []string{"-archive=/x", "--replace", "${IMG}"} {
		src := "services:\n  app:\n    image: \"" + img + "\"\n"
		lookup := func(k string) (string, bool) { return "-archive=/x", k == "IMG" }
		if _, err := traducirCompose([]byte(src), "dev", false, lookup); err == nil || !strings.Contains(err.Error(), "image") {
			t.Errorf("%q: err = %v", img, err)
		}
	}
	src := "services:\n  app:\n    image: ghcr.io/a/b:1.2@sha256:" + strings.Repeat("a", 64) + "\n"
	if _, err := traducirCompose([]byte(src), "dev", false, sinEntorno); err != nil {
		t.Fatalf("una referencia buena: %v", err)
	}
}
