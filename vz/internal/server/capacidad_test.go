package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Un VMM que no puede crear otra VM porque el Mac no tiene sitio (kling-hv:
// HV_NO_RESOURCES en la VM 128) contesta 507 con su mensaje, como la falta de
// memoria, y no el 400 de una configuración mala. Un fallo cualquiera de
// Create sigue en 400.
func TestCreateSinCapacidadEs507(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("creating the VM: %w (hv_vm_create failed: 0xfae94005)", ErrSinCapacidad), http.StatusInsufficientStorage},
		{errors.New("creating the VM: hv_gic_create failed: 0xfae94001"), http.StatusBadRequest},
	} {
		r := newRig(t)
		r.configure(t.TempDir())
		r.f.errNext = c.err
		code, out := r.call("PUT", "/actions", `{"action_type":"InstanceStart"}`)
		if code != c.code {
			t.Fatalf("%v -> %d %s, want %d", c.err, code, out, c.code)
		}
		var f struct {
			FaultMessage string `json:"fault_message"`
		}
		if err := json.Unmarshal([]byte(out), &f); err != nil || !strings.Contains(f.FaultMessage, "creating the VM") {
			t.Fatalf("fault body %q does not carry the VMM error", out)
		}
	}
}
