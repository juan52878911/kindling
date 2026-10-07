package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// El plazo de cada ejecución de la sonda no puede llamarse como la espera
// total a "listo" de commit, fork y run (ready_timeout_seconds): son cosas
// distintas y un mismo nombre invitaba a confundirlas.
func TestServiceSpecProbeTimeoutJSON(t *testing.T) {
	b, err := json.Marshal(ServiceSpec{Argv: []string{"x"}, ProbeTimeoutSeconds: 3, ReadyStartPeriodSeconds: 40})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"probe_timeout_seconds":3`) || strings.Contains(s, "ready_timeout_seconds") {
		t.Fatalf("service.json: %s", s)
	}
	var back ServiceSpec
	if err := json.Unmarshal([]byte(`{"argv":["x"],"probe_timeout_seconds":7}`), &back); err != nil || back.ProbeTimeoutSeconds != 7 {
		t.Fatalf("decode: %+v %v", back, err)
	}
}
