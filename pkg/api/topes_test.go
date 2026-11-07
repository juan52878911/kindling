package api

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateLabels(t *testing.T) {
	muchas := map[string]string{}
	for i := 0; i <= MaxLabels; i++ {
		muchas[fmt.Sprintf("k%d", i)] = "v"
	}
	for _, c := range []struct {
		nombre string
		labels map[string]string
		vale   bool
	}{
		{"nil", nil, true},
		{"normales", map[string]string{"service": "web", "env": "dev"}, true},
		{"demasiadas", muchas, false},
		{"valor enorme", map[string]string{"a": strings.Repeat("x", MaxLabelsBytes)}, false},
		{"clave enorme", map[string]string{strings.Repeat("k", MaxLabelsBytes+1): ""}, false},
		{"justo el tope", map[string]string{"a": strings.Repeat("x", MaxLabelsBytes-1)}, true},
	} {
		if err := ValidateLabels(c.labels); (err == nil) != c.vale {
			t.Errorf("%s: err = %v, quería válido=%v", c.nombre, err, c.vale)
		}
	}
}

func TestValidateAllowDomains(t *testing.T) {
	if err := ValidateAllowDomains(make([]string, MaxAllowDomains)); err != nil {
		t.Fatalf("en el tope: %v", err)
	}
	if err := ValidateAllowDomains(make([]string, MaxAllowDomains+1)); err == nil {
		t.Fatal("se admite una lista por encima del tope")
	}
}
