package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/esquema"
)

// Save escribe "schema", deja la copia de la config sin versión y conserva
// las claves que este binario no conoce, arriba y dentro de las secciones,
// sin resucitar las conocidas que se borran.
func TestSaveConservaDesconocidasYVersiona(t *testing.T) {
	p := enUnaConfigTemporal(t)
	v0 := `{"current_context":"lab","daemon":{"vmm":"vz","futuro_daemon":{"x":1}},` +
		`"futuro":"sí","gateway":{"listen":":1"},"contexts":{"lab":{"host":"tcp://h:1"}}}`
	if err := os.WriteFile(p, []byte(v0), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Daemon.VMM = "" // `kling config unset daemon.vmm`
	c.Gateway.Listen = ":2"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(esquema.RutaRespaldo(p, 0)); err != nil || string(b) != v0 {
		t.Fatalf("copia v0 = %q, %v", b, err)
	}
	b, _ := os.ReadFile(p)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["schema"] != float64(Version) || got["futuro"] != "sí" {
		t.Fatalf("schema/futuro: %s", b)
	}
	d := got["daemon"].(map[string]any)
	if _, ok := d["vmm"]; ok {
		t.Fatalf("resucitó daemon.vmm: %s", b)
	}
	if fd, ok := d["futuro_daemon"].(map[string]any); !ok || fd["x"] != float64(1) {
		t.Fatalf("perdió daemon.futuro_daemon: %s", b)
	}
	if g := got["gateway"].(map[string]any); g["listen"] != ":2" {
		t.Fatalf("gateway.listen: %s", b)
	}
	// Se relee y se reescribe igual (idempotente).
	c2, err := Load()
	if err != nil || c2.Host("") != "tcp://h:1" {
		t.Fatalf("Load = %v, host %q", err, c2.Host(""))
	}
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(p); string(b2) != string(b) {
		t.Fatalf("no es idempotente:\n%s\n%s", b, b2)
	}
	// Borrar un contexto no lo resucita (los mapas no se mezclan).
	delete(c2.Contexts, "lab")
	c2.CurrentContext = ""
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}
	if b3, _ := os.ReadFile(p); strings.Contains(string(b3), "tcp://h:1") {
		t.Fatalf("resucitó el contexto: %s", b3)
	}
}

// Una config de un kling más nuevo se lee (para poder hablar con el daemon y
// actualizar), pero no se escribe encima.
func TestConfigDelFuturo(t *testing.T) {
	p := enUnaConfigTemporal(t)
	futuro := `{"schema":99,"current_context":"lab","contexts":{"lab":{"host":"tcp://h:1"}}}`
	if err := os.WriteFile(p, []byte(futuro), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Host("") != "tcp://h:1" {
		t.Fatalf("Load = %v, host %q", err, c.Host(""))
	}
	err = c.Save()
	if !esquema.EsMasNuevo(err) || !strings.Contains(err.Error(), "update kling") {
		t.Fatalf("Save = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != futuro {
		t.Fatalf("la pisó: %s", b)
	}
}
