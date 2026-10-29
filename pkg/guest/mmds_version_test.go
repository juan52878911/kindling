package guest

import (
	"encoding/json"
	"testing"
)

// ConVersionMMDS pone kling_mmds en un objeto (decodificado o en crudo) sin
// tocar el original, y deja lo que no es un objeto como está.
func TestConVersionMMDS(t *testing.T) {
	orig := map[string]any{"env": map[string]any{"A": "1"}}
	got, ok := ConVersionMMDS(orig).(map[string]any)
	if !ok || got[MMDSVersionKey] != MMDSVersion || got["env"] == nil {
		t.Fatalf("objeto = %v", got)
	}
	if _, ok := orig[MMDSVersionKey]; ok {
		t.Fatal("tocó el mapa del llamante")
	}
	crudo, ok := ConVersionMMDS(json.RawMessage(`{"sessions":{}}`)).(map[string]any)
	if !ok || crudo[MMDSVersionKey] != MMDSVersion || crudo["sessions"] == nil {
		t.Fatalf("crudo = %v", crudo)
	}
	if arr, ok := ConVersionMMDS(json.RawMessage(`[1]`)).(json.RawMessage); !ok || string(arr) != `[1]` {
		t.Fatalf("array = %v", arr)
	}
	// Y el agente la lee; una mayor que la suya no estorba al resto.
	var s MMDSStore
	if err := json.Unmarshal([]byte(`{"kling_mmds":7,"env":{"A":"1"},"futuro":true}`), &s); err != nil || s.Version != 7 || s.Env["A"] != "1" {
		t.Fatalf("store = %+v, %v", s, err)
	}
}
