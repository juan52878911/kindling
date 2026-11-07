package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas y allow_domains sin tope eran memoria y disco (machines.json) a
// gusto de quien llama: run, sandbox, fork y PUT labels las rechazan con 400
// antes de tocar nada.
func TestTopesDeEntrada(t *testing.T) {
	_, h := testServer(t)
	muchas := map[string]string{}
	for i := 0; i <= api.MaxLabels; i++ {
		muchas[fmt.Sprintf("k%d", i)] = "v"
	}
	enorme := map[string]string{"service": strings.Repeat("x", api.MaxLabelsBytes)}
	dominios := make([]string, api.MaxAllowDomains+1)
	for i := range dominios {
		dominios[i] = fmt.Sprintf("d%d.example.com", i)
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, c := range []struct {
		nombre, method, path, body, dice string
	}{
		{"run muchas", "POST", "/machines", js(map[string]any{"image": "x", "labels": muchas}), "labels"},
		{"run enorme", "POST", "/machines", js(map[string]any{"image": "x", "labels": enorme}), "labels"},
		{"run dominios", "POST", "/machines", js(map[string]any{"image": "x", "egress": "allowlist", "allow_domains": dominios}), "allow_domains"},
		{"sandbox enorme", "POST", "/sandboxes", js(map[string]any{"labels": enorme}), "labels"},
		{"sandbox dominios", "POST", "/sandboxes", js(map[string]any{"egress": "allowlist", "allow_domains": dominios}), "allow_domains"},
		{"labels muchas", "PUT", "/machines/nada/labels", js(muchas), "labels"},
		{"labels enorme", "PUT", "/machines/nada/labels", js(enorme), "labels"},
	} {
		rr := call(t, h, c.method, c.path, c.body)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), c.dice) {
			t.Errorf("%s = %d %s, quería 400 que hable de %s", c.nombre, rr.Code, strings.TrimSpace(rr.Body.String()), c.dice)
		}
	}
}
