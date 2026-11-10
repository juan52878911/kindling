package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Tras cambiar el agente de una imagen (kling image refresh), la receta deja
// de decir el viejo, en ella y en sus capas, hasta que una máquina anuncie el
// nuevo; lo demás de la receta queda.
func TestOlvidarAgenteDeImagen(t *testing.T) {
	m := newTestManager(t)
	dir := filepath.Dir(m.recipePath("base"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	viejo := `"agent":{"agent":"kling-guest","version":"0.17.2","caps":["resync"]}`
	for nombre, receta := range map[string]string{
		"base":   `{"name":"base",` + viejo + `,"kling_version":"0.17.2"}`,
		"capa":   `{"name":"capa","base":"base",` + viejo + `}`,
		"otra":   `{"name":"otra",` + viejo + `}`,
		"suelta": `{"name":"suelta","kling_version":"0.17.2"}`,
	} {
		if err := os.WriteFile(m.recipePath(nombre), []byte(receta), 0o600); err != nil {
			t.Fatal(err)
		}
		ext := ".ext4"
		if nombre == "capa" {
			ext = ".layer.ext4"
		}
		if err := os.WriteFile(filepath.Join(dir, nombre+ext), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.OlvidarAgenteDeImagen("base")
	m.OlvidarAgenteDeImagen("suelta")
	for _, n := range []string{"base", "capa", "suelta"} {
		if got := m.AgenteDeImagen(n); got == nil || got.Agent != "kling-guest" || got.Version != "" || got.Caps != nil {
			t.Errorf("%s: sigue diciendo el agente viejo: %+v", n, got)
		}
	}
	if got := m.AgenteDeImagen("otra"); got == nil || got.Version != "0.17.2" {
		t.Errorf("otra: tocó una imagen que no era: %+v", got)
	}
	if b, _ := os.ReadFile(m.recipePath("base")); !json.Valid(b) || !strings.Contains(string(b), `"kling_version"`) {
		t.Errorf("perdió parte de la receta: %s", b)
	}
	// El agente nuevo, al anunciarse, se apunta.
	m.agenteEnReceta("base", &api.GuestAgent{Agent: "kling-guest", Version: "1.0.0", Caps: []string{api.GuestCapResync}})
	if got := m.AgenteDeImagen("base"); got.Version != "1.0.0" {
		t.Errorf("no apuntó el agente nuevo: %+v", got)
	}
}
