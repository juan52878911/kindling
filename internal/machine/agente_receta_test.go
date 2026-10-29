package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El agente que anuncia una máquina se apunta en la receta de su imagen si
// no lo tenía (o solo la versión), sin perder el resto de la receta; y
// AgenteDeImagen lo devuelve, o la versión que la construyó si es de antes.
func TestAgenteEnReceta(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Dir(m.recipePath("eco")), 0o755); err != nil {
		t.Fatal(err)
	}
	vieja := `{"name":"eco","base":"min","cmd":["x"],"kling_version":"0.19.1","clave_de_otro":{"a":1}}`
	if err := os.WriteFile(m.recipePath("eco"), []byte(vieja), 0o600); err != nil {
		t.Fatal(err)
	}
	if ag := m.AgenteDeImagen("eco"); ag == nil || ag.Version != "0.19.1" || ag.Caps != nil {
		t.Fatalf("receta de antes: %+v", ag)
	}
	if m.AgenteDeImagen("nada") != nil {
		t.Fatal("agente de una imagen sin receta")
	}

	ag := &api.GuestAgent{Agent: "kling-guest", Version: "0.21.0", Caps: []string{api.GuestCapResync}}
	m.agenteEnReceta("eco", ag)
	b, _ := os.ReadFile(m.recipePath("eco"))
	var claves map[string]json.RawMessage
	if err := json.Unmarshal(b, &claves); err != nil || string(claves["clave_de_otro"]) == "" || string(claves["kling_version"]) != `"0.19.1"` {
		t.Fatalf("perdió parte de la receta: %s", b)
	}
	if got := m.AgenteDeImagen("eco"); got == nil || got.Version != "0.21.0" || !slices.Contains(got.Caps, api.GuestCapResync) {
		t.Fatalf("tras apuntarlo: %+v", got)
	}
	if fi, _ := os.Stat(m.recipePath("eco")); fi.Mode().Perm() != 0o600 {
		t.Errorf("cambió los permisos: %v", fi.Mode().Perm())
	}
	// Con capacidades ya apuntadas no se reescribe.
	m.agenteEnReceta("eco", &api.GuestAgent{Agent: "kling-guest", Version: "9.9.9", Caps: []string{}})
	if got := m.AgenteDeImagen("eco"); got.Version != "0.21.0" {
		t.Fatalf("la pisó: %+v", got)
	}
	// La del constructor (solo versión) sí se completa con las capacidades.
	os.WriteFile(m.recipePath("eco"), []byte(`{"name":"eco","agent":{"agent":"kling-guest","version":"0.21.0"}}`), 0o600)
	m.agenteEnReceta("eco", ag)
	if got := m.AgenteDeImagen("eco"); got == nil || got.Caps == nil {
		t.Fatalf("no la completó: %+v", got)
	}
}
