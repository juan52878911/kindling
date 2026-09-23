package api

import (
	"encoding/json"
	"testing"
)

// Un daemon anterior no envía "capabilities": el cliente nuevo debe leerlo como
// "no tiene ninguna", no fallar ni inventarse capacidades.
func TestInfoHasSinCapacidades(t *testing.T) {
	var i Info
	if err := json.Unmarshal([]byte(`{"version":"0.4.0","root":"/r","kvm":true,"machines":0}`), &i); err != nil {
		t.Fatal(err)
	}
	if i.Has("annotations") {
		t.Fatal("un daemon sin capabilities no puede anunciar annotations")
	}
}

func TestInfoHas(t *testing.T) {
	i := Info{Capabilities: []string{"annotations", "store"}}
	if !i.Has("store") || i.Has("exec") {
		t.Fatalf("Has mal: %+v", i.Capabilities)
	}
}
