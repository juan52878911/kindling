package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
)

// ai.json es el registro de ejemplo del gateway. Su tarea de la capa 4 tiene
// que llevar exactamente el prompt y el esquema que valida la demo (y el
// esquema en su orden: el LLM genera en ese orden).
func TestAIConfig(t *testing.T) {
	f, err := os.Open("ai.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := aigw.ParseConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.Tasks["room-llm"]
	if tc == nil || tc.System != domotica.LLMSystemPrompt || tc.Prompt != "{input}" || tc.Temperature == nil || *tc.Temperature != 0 {
		t.Fatal("room-llm must carry the domotica prompt, {input} and temperature 0 (regenerate ai.json)")
	}
	var a, b bytes.Buffer
	_ = json.Compact(&a, tc.JSONSchema)
	_ = json.Compact(&b, domotica.LLMSchema)
	if a.String() != b.String() {
		t.Fatal("room-llm json_schema differs from domotica.LLMSchema (regenerate ai.json)")
	}
	if d := cfg.Tasks["room"].Intent; d == nil || d.Model == "" || d.Encoder == "" || d.Domain != DomainName {
		t.Fatalf("room must be an intent task of the %s domain with its encoder", DomainName)
	}
}

func TestLayerOf(t *testing.T) {
	cases := map[string]*api.Machine{
		domotica.LayerEncoder: {Name: "gw-enc-1", Labels: map[string]string{"von.kind": "embed", "von.model": "e5"}},
		domotica.LayerVON:     {Name: "gw-llm-1", Labels: map[string]string{"von.model": "qwen"}},
		domotica.LayerChispa:  {Name: "chispa-room-1", From: "chispa-room"},
		"":                    {Name: "other"},
	}
	for want, m := range cases {
		if got := layerOf(m); got != want {
			t.Errorf("%s: got %q, want %q", m.Name, got, want)
		}
	}
}

func TestBacked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	f := flags{record: p, llmTask: "room-llm", decideTask: "room"}
	if sc, _ := backed(f, "gateway:room"); sc != "" {
		t.Fatal("no record, no layer 4")
	}
	write := func(r map[string]any) {
		b, _ := json.Marshal(r)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := map[string]any{"id": "room-llm", "prompt_id": domotica.PromptID(), "fast_id": "gateway:room",
		"scopes": map[string]any{"all": map[string]any{"pass": false, "why": "no"}, "uncertain": map[string]any{"pass": true, "why": "yes"}}}
	write(rec)
	if sc, _ := backed(f, "gateway:room"); sc != domotica.ScopeUncertain {
		t.Fatalf("scope %q", sc)
	}
	if sc, _ := backed(f, "gateway:other"); sc != "" {
		t.Fatal("a record for other fast layers must not back it")
	}
	rec["prompt_id"] = "x"
	write(rec)
	if sc, _ := backed(f, "gateway:room"); sc != "" {
		t.Fatal("a record for another prompt must not back it")
	}
}

func TestToken(t *testing.T) {
	t.Setenv("KLING_AI_TOKEN", "")
	p := filepath.Join(t.TempDir(), "tok")
	if tok, err := token(p); err != nil || tok != "" {
		t.Fatal("no file, no token")
	}
	if err := os.WriteFile(p, []byte("secret-token-123456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := token(p); err == nil {
		t.Fatal("a token readable by others must be refused")
	}
	_ = os.Chmod(p, 0o600)
	if tok, err := token(p); err != nil || tok != "secret-token-123456" {
		t.Fatal(tok, err)
	}
}
