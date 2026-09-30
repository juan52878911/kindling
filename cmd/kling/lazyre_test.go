package main

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// Las regexps de paquete se compilan en el primer uso (lazyre): este binario
// enlaza todos los paquetes de kling, así que aquí se prueban todas a la vez
// en lugar de esperar al pánico en producción.
func TestRegexpsCompilan(t *testing.T) {
	if err := lazyre.CompileAll(); err != nil {
		t.Fatal(err)
	}
}
