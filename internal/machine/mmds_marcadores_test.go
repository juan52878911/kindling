package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// PutMMDS sustituye el almacén entero: en una máquina con credenciales, los
// marcadores tienen que seguir en "env" después, junto a lo que se inyectó, y
// un "env" propio del documento no los pisa.
func TestPutMMDSConservaLosMarcadoresDeCredenciales(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "abcdef0123456790"
	mc := m.addForTest(id)
	falso := nuevoFcFalso(t)
	m.mu.Lock()
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := []credproxy.Credential{{Env: "API_KEY", Domain: "api.example.com",
		Placeholder: "kling-cred-marcador", Secret: credSecreto}}
	if err := m.guardarCredenciales(id, creds); err != nil {
		t.Fatal(err)
	}

	casos := []string{
		`{"sessions":{"s1":{"TOKEN":"t"}}}`,
		`{"env":{"OTRA":"x","API_KEY":"intento de pisarlo"}}`,
		`{}`,
		`null`,
	}
	for i, doc := range casos {
		if _, err := m.PutMMDS(context.Background(), mc.ID, json.RawMessage(doc)); err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		puts := falso.llamadasA(http.MethodPut, "/mmds")
		if len(puts) != i+1 {
			t.Fatalf("%s: %d PUT /mmds", doc, len(puts))
		}
		var got struct {
			Env map[string]any `json:"env"`
		}
		if err := json.Unmarshal(puts[i].Cuerpo, &got); err != nil {
			t.Fatalf("%s: cuerpo %q: %v", doc, puts[i].Cuerpo, err)
		}
		if got.Env["API_KEY"] != "kling-cred-marcador" {
			t.Errorf("%s: tras el PUT el marcador no está: %s", doc, puts[i].Cuerpo)
		}
	}
	// Lo del documento se conserva.
	var ultimo map[string]any
	_ = json.Unmarshal(falso.llamadasA(http.MethodPut, "/mmds")[1].Cuerpo, &ultimo)
	if env, _ := ultimo["env"].(map[string]any); env["OTRA"] != "x" {
		t.Errorf("se perdió la variable propia del documento: %v", ultimo)
	}
	// {} sigue contando como vaciar a efectos de la marca de secretos.
	if !almacenVacio(json.RawMessage(`{}`)) {
		t.Fatal("almacenVacio cambió")
	}
}
