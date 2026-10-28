package server

import (
	"strings"
	"testing"
)

// El nombre del backend sale en GET / y GET /kling/info: "vz" si no se da,
// el de Deps.Backend si se da (kling-hv dice "hv").
func TestBackendName(t *testing.T) {
	r := newRig(t)
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"backend":"vz"`) {
		t.Fatalf("default backend: %s", out)
	}
	r.srv.d.Backend = "hv"
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"backend":"hv"`) {
		t.Fatalf("hv backend: %s", out)
	}
	if out := r.must("GET", "/", ""); !strings.Contains(out, `"app_name":"kling-hv"`) {
		t.Fatalf("hv app_name: %s", out)
	}
}
