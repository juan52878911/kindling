package frontal

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una máquina con las etiquetas de una precalentada pero con kling.owner la
// creó un inquilino del daemon: el frontal no se la da a ningún cliente (el
// inquilino conservaría exec y ficheros) ni la cuenta como sandbox de nadie.
func TestPrecalentadaConDuenoNoSeReparte(t *testing.T) {
	mc := &api.Machine{
		ID:     "m1",
		State:  api.StateWarm,
		Egress: "none",
		OnTTL:  api.OnTTLFreeze,
		Labels: map[string]string{api.LabelKind: api.KindSandbox, LabelTemplate: "py"},
	}
	if !esPrecalentadaDe(mc, "py") {
		t.Fatal("la precalentada del frontal no se reconoce")
	}
	mc.Labels[api.LabelOwner] = "a"
	if esPrecalentadaDe(mc, "py") {
		t.Error("esPrecalentadaDe acepta una máquina con kling.owner=a")
	}
	mc.Labels[LabelTenant] = "b"
	if esSandboxDe(mc, &Tenant{Nombre: "b"}) {
		t.Error("esSandboxDe da al cliente b una máquina con kling.owner=a")
	}
}
