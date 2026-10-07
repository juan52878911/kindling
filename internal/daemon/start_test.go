package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// POST /machines/{ref}/start existe, se anuncia, acepta el cuerpo vacío y
// valida el entorno antes de tocar nada (el error nombra la clave, nunca el
// valor).
func TestStartAPI(t *testing.T) {
	_, h := testServer(t)

	var info api.Info
	if err := json.Unmarshal(call(t, h, "GET", "/info", "").Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityStart) {
		t.Fatalf("sin la capacidad %s: %v", api.CapabilityStart, info.Capabilities)
	}
	rr := call(t, h, "POST", "/machines/nada/start", "")
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "doesn't exist") {
		t.Fatalf("start de una que no existe = %d %s", rr.Code, rr.Body)
	}
	rr = call(t, h, "POST", "/machines/nada/start", `{"env":["no vale=secreto"]}`)
	if rr.Code != 404 || strings.Contains(rr.Body.String(), "secreto") {
		t.Fatalf("start con un entorno inválido = %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/machines/nada/start", `{`); rr.Code != 400 {
		t.Fatalf("start con JSON roto = %d %s", rr.Code, rr.Body)
	}
}
