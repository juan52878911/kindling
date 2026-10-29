package daemon

import (
	"flag"
	"testing"

	"github.com/juan52878911/kindling/internal/contrato"
	"github.com/juan52878911/kindling/pkg/api"
)

var actualizar = flag.Bool("update", false, "regenerate the goldens of testdata/contrato")

// La tabla de rutas es contrato: método, patrón y acción de autorización de
// cada ruta de rutas(), comparados con testdata/contrato/rutas.txt. Quitar o
// cambiar una (también su acción: cambia quién puede usarla) exige subir
// api.APIVersion; añadir una pide -update (y su capacidad en docs/api.md).
func TestContratoRutas(t *testing.T) {
	s, _ := testServer(t)
	var lineas []string
	vistas := map[string]bool{}
	for _, rt := range s.rutas() {
		if vistas[rt.patron] {
			t.Errorf("ruta repetida: %s", rt.patron)
		}
		vistas[rt.patron] = true
		lineas = append(lineas, rt.patron+"  "+string(rt.accion))
	}
	cab := []string{
		"Rutas del daemon: <MÉTODO> <patrón>  <acción de authz>.",
		"Lo genera TestContratoRutas (internal/daemon/contrato_rutas_test.go):",
		"  go test ./internal/daemon -run TestContratoRutas -update",
		"Quitar o cambiar una línea exige subir api.APIVersion (internal/contrato).",
	}
	if err := contrato.Comparar("testdata/contrato/rutas.txt", api.APIVersion, cab, lineas, *actualizar); err != nil {
		t.Fatal(err)
	}
}
