package mcp

import (
	"encoding/json"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestIsolationDeLaAnotacion(t *testing.T) {
	con := func(v any) *api.Snapshot {
		b, _ := json.Marshal(v)
		return &api.Snapshot{Annotations: map[string]json.RawMessage{IsolationKey: b}}
	}
	for _, c := range []struct {
		s    *api.Snapshot
		want string
	}{
		{nil, IsolationService},
		{&api.Snapshot{}, IsolationService},
		{con("session"), IsolationSession},
		{con("service"), IsolationService},
		// Un valor que no se entiende no aísla a medias ni rompe: el de siempre.
		{con("Session"), IsolationService},
		{con(42), IsolationService},
	} {
		if got := Isolation(c.s); got != c.want {
			t.Errorf("Isolation(%v) = %q, quería %q", c.s, got, c.want)
		}
	}
}

// Un volumen de escritura tiene un solo escritor: en modo session la segunda
// sesión no podría arrancar. Uno de lectura sí se comparte.
func TestSessionIsolationConflictConVolumenes(t *testing.T) {
	if err := SessionIsolationConflict(nil); err != nil {
		t.Fatalf("sin volúmenes: %v", err)
	}
	if err := SessionIsolationConflict([]api.VolumeAttachment{{Name: "libs", ReadOnly: true}}); err != nil {
		t.Fatalf("solo lectura: %v", err)
	}
	if err := SessionIsolationConflict([]api.VolumeAttachment{
		{Name: "libs", ReadOnly: true}, {Name: "datos"}}); err == nil {
		t.Fatal("un volumen rw tiene que impedir el modo session")
	}
}
