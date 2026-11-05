package main

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// Las regexps de paquete se compilan en el primer uso (lazyre), así que un
// error de sintaxis ya no salta al arrancar: se prueban aquí las de este
// binario.
func TestRegexpsCompilan(t *testing.T) {
	if err := lazyre.CompileAll(); err != nil {
		t.Fatal(err)
	}
}
