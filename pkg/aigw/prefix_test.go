package aigw

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/von"
)

func TestPrefixes(t *testing.T) {
	c := &Config{
		Models: map[string]*ModelConfig{
			"j": {Kind: KindJEV, Path: "m.jev"},
			"q": {Kind: KindVON, Snapshot: "von-q"},
			"s": {Kind: KindVON, Snapshot: "von-s"},
		},
		Tasks: map[string]*TaskConfig{
			"home":  {VON: "q", System: "You control a smart home.", Prompt: "Request: {input}"},
			"home2": {VON: "q", System: "You control a smart home.", Prompt: "Request: {input}"}, // repetido
			"bare":  {VON: "q"},                                                                  // nada fijo
			"kind":  {JEV: "j", EscalateTo: "q"},
			"other": {VON: "s", System: "Other model."},
		},
	}
	got := c.Prefixes("q")
	want := []von.Prefix{
		{System: "You control a smart home.", User: "Request: "},
		{System: defaultSystem, User: "Labels: "},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("prefixes = %+v", got)
	}
	if p := c.Prefixes("s"); len(p) != 1 || p[0].System != "Other model." || p[0].User != "" {
		t.Fatalf("model s: %+v", p)
	}
	if p := c.Prefixes("nope"); len(p) != 0 {
		t.Fatalf("unknown model: %+v", p)
	}
}

func TestJSONSchema(t *testing.T) {
	for _, b := range []string{
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","json_schema":[1]}}}`,
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","json_schema":"x"}}}`,
		`{"models":{"j":{"kind":"jev","path":"x"},"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"jev":"j","json_schema":{}}}}`,
	} {
		if _, err := ParseConfig(strings.NewReader(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	g, ll, _ := newTestGateway(t, func(c *Config) {
		c.Tasks["js"] = &TaskConfig{VON: "smol", System: "Answer in JSON.", JSONSchema: schema}
	})
	h := g.Handler("")
	ll.set(`{"ok": true}`)
	rec := do(t, h, "POST", "/v1/generate", "", map[string]any{"task": "js", "input": "x"})
	if rec.Code != 200 || decode[GenerateResponse](t, rec).Output != `{"ok": true}` {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	// El esquema viaja a llama-server tal cual, en su campo json_schema.
	if b, _ := json.Marshal(ll.last()["json_schema"]); !strings.Contains(string(b), `"required":["ok"]`) {
		t.Fatalf("json_schema sent = %s", b)
	}
	// Una respuesta que no es JSON (invitado roto, o cortada por max_tokens)
	// es un 502 que lo dice, no un 200 ilegible.
	ll.set(`{"ok": tr`)
	if rec := do(t, h, "POST", "/v1/generate", "", map[string]any{"task": "js", "input": "x"}); rec.Code != 502 ||
		!strings.Contains(rec.Body.String(), "valid JSON") {
		t.Fatalf("invalid JSON: %d %s", rec.Code, rec.Body)
	}
	// Sin esquema no se comprueba nada ni se manda el campo.
	g2, ll2, _ := newTestGateway(t, func(c *Config) { c.Tasks["free"] = &TaskConfig{VON: "smol"} })
	ll2.set("not json")
	if rec := do(t, g2.Handler(""), "POST", "/v1/generate", "", map[string]any{"task": "free", "input": "x"}); rec.Code != 200 {
		t.Fatalf("free: %d", rec.Code)
	}
	if _, ok := ll2.last()["json_schema"]; ok {
		t.Fatal("json_schema sent without one in the task")
	}
}
