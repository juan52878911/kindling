package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
	"github.com/juan52878911/kindling/pkg/aigw"
)

// ai.json es el registro de ejemplo: el gateway lo acepta, tiene las tres
// tareas con los nombres por defecto y el esquema de VON enumera exactamente
// las categorías que valida el ejemplo.
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
	o := triage.DefaultOptions
	for _, n := range []string{o.LinesTask, o.CategoryTask, o.SummaryTask} {
		if cfg.Tasks[n] == nil {
			t.Fatalf("ai.json has no task %q", n)
		}
	}
	var schema struct {
		Properties struct {
			Category struct {
				Enum []string `json:"enum"`
			} `json:"category"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(cfg.Tasks[o.SummaryTask].JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Properties.Category.Enum, triage.Categories) {
		t.Fatalf("schema enum %v, want %v", schema.Properties.Category.Enum, triage.Categories)
	}
	if !strings.Contains(cfg.Tasks[o.SummaryTask].Prompt, "{hint}") || !strings.Contains(cfg.Tasks[o.SummaryTask].Prompt, "{input}") {
		t.Fatal("the summary prompt must carry {hint} and {input}")
	}
}

// La página solo contesta a un Host de loopback y solo acepta POST con JSON o
// con su cabecera propia (un formulario de otra web no puede mandarlos).
func TestServeGuards(t *testing.T) {
	s := &server{busy: make(chan struct{}, 1)}
	h := s.handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example:8089"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/analyze", strings.NewReader("log"))
	req.Host = "127.0.0.1:8089"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:8089"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "CI triage") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("page: %d", w.Code)
	}
}

// export deja una etiqueta por log (gana la última confirmación) y fuera lo
// que no es del formato o es flaky.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/fb.jsonl"
	lines := []string{
		`{"schema":"ci-triage.feedback/v1","text":"FAIL a","label":"test","log_sha256":"h1"}`,
		`{"schema":"ci-triage.feedback/v1","text":"FAIL a","label":"infra","log_sha256":"h1","fields":{"sec":"script"}}`,
		`{"schema":"ci-triage.feedback/v1","text":"FAIL b","label":"flaky","log_sha256":"h2"}`,
		`{"schema":"other","text":"x","label":"test"}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := cmdExport([]string{p})
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	want := `{"text":"FAIL a","fields":{"sec":"script"},"label":"infra","by":"ci-triage"}` + "\n"
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}
