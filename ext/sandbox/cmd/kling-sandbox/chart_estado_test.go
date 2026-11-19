package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sin helm en CI de Go, esto fija lo mínimo del chart: que monta el directorio
// de estado con la bandera que el gateway tiene de verdad, y que hay valores de
// ci/ que lo ejercitan (scripts/helm-check.sh los renderiza).
func TestChartMontaElDirectorioDeEstado(t *testing.T) {
	chart := filepath.Join("..", "..", "..", "..", "deploy", "helm", "kindling-sandbox")
	leer := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{chart}, rel...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	gw, err := os.ReadFile("gateway.go")
	if err != nil || !strings.Contains(string(gw), `fs.String("state-dir"`) {
		t.Fatalf("el gateway ya no tiene la bandera -state-dir (%v): actualiza el chart", err)
	}
	dep := leer("templates", "gateway-deployment.yaml")
	for _, quiero := range []string{"- -state-dir", "name: state", "persistentVolumeClaim", "emptyDir"} {
		if !strings.Contains(dep, quiero) {
			t.Errorf("gateway-deployment.yaml no contiene %q", quiero)
		}
	}
	if !strings.Contains(leer("values.yaml"), "stateDir:") {
		t.Error("values.yaml no declara gateway.stateDir")
	}
	if !strings.Contains(leer("templates", "gateway-pvc.yaml"), "kind: PersistentVolumeClaim") {
		t.Error("falta el PVC opcional")
	}
	for _, f := range []string{"gateway-estado.yaml", "gateway-estado-claim.yaml"} {
		if !strings.Contains(leer("ci", f), "stateDir:") {
			t.Errorf("ci/%s no ejercita gateway.stateDir", f)
		}
	}
}
