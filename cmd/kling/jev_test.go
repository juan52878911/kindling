package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Prueba de extremo a extremo de `kling jev`: compila el binario de verdad y
// recorre train → eval → predict → inspect sobre un conjunto diminuto.

func writeJevData(t *testing.T, path string, n, seed int) {
	t.Helper()
	vocab := map[string][]string{
		"bug":  {"crash", "panic", "broken", "segfault", "regression"},
		"feat": {"add", "support", "introduce", "implement", "option"},
		"docs": {"readme", "typo", "guide", "documentation", "example"},
	}
	labels := []string{"bug", "docs", "feat"}
	noise := []string{"the", "parser", "cache", "server", "in", "for", "when"}
	var b bytes.Buffer
	s := uint64(seed)
	next := func() int {
		s = s*6364136223846793005 + 1442695040888963407
		return int(s >> 33)
	}
	for i := 0; i < n; i++ {
		l := labels[next()%3]
		var words []string
		for j := 0; j < 3; j++ {
			words = append(words, vocab[l][next()%5], noise[next()%7])
		}
		ext := ".go"
		if l == "docs" {
			ext = ".md"
		}
		line, _ := json.Marshal(map[string]any{"text": strings.Join(words, " "), "label": l,
			"fields": map[string]any{"ext": ext, "files": next() % 9}})
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJevCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the kling binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	dir := t.TempDir()
	kling := filepath.Join(dir, "kling")
	if out, err := exec.Command(goBin, "build", "-o", kling, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	p := func(name string) string { return filepath.Join(dir, name) }
	writeJevData(t, p("train.jsonl"), 400, 1)
	writeJevData(t, p("valid.jsonl"), 150, 2)
	writeJevData(t, p("test.jsonl"), 150, 3)

	run := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command(kling, args...)
		cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=0", "KLING_PLUGIN_PATH="+dir)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mustRun := func(stdin string, args ...string) string {
		t.Helper()
		out, err := run(stdin, args...)
		if err != nil {
			t.Fatalf("kling %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	trainArgs := []string{"jev", "train", "-data", p("train.jsonl"), "-valid", p("valid.jsonl"),
		"-buckets", "12", "-min-support", "5"}
	out := mustRun("", append(trainArgs, "-test", p("test.jsonl"), "-o", p("a.jev"))...)
	for _, want := range []string{"trained", "3 labels", "int16 vs float agreement (test)", "confusion"} {
		if !strings.Contains(out, want) {
			t.Errorf("train output lacks %q:\n%s", want, out)
		}
	}
	// Misma semilla y SOURCE_DATE_EPOCH: el fichero sale idéntico byte a byte.
	mustRun("", append(trainArgs, "-o", p("b.jev"))...)
	a, _ := os.ReadFile(p("a.jev"))
	b, _ := os.ReadFile(p("b.jev"))
	if !bytes.Equal(a, b) {
		t.Error("two trainings with the same seed produced different files")
	}

	out = mustRun("", "jev", "eval", "-model", p("a.jev"), "-data", p("test.jsonl"))
	for _, want := range []string{"accuracy:", "macro-F1:", "ECE:", "confident:", "precision on confident", "confusion"} {
		if !strings.Contains(out, want) {
			t.Errorf("eval output lacks %q:\n%s", want, out)
		}
	}
	var rep struct {
		N        int     `json:"n"`
		Accuracy float64 `json:"accuracy"`
		Coverage float64 `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(mustRun("", "jev", "eval", "-json", "-model", p("a.jev"), "-data", p("test.jsonl"))), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.N != 150 || rep.Accuracy < 0.9 {
		t.Errorf("eval -json: %+v", rep)
	}

	var pred struct {
		Label     string  `json:"label"`
		Prob      float64 `json:"prob"`
		Threshold float64 `json:"threshold"`
		Decision  string  `json:"decision"`
		Evidence  []any   `json:"evidence"`
		Probs     []any   `json:"probs"`
		Confident *bool   `json:"confident"`
	}
	out = mustRun("", "jev", "predict", "-model", p("a.jev"), "-text", "segfault crash in the parser", "-json", "-top", "3")
	if err := json.Unmarshal([]byte(out), &pred); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if pred.Label != "bug" || pred.Confident == nil || len(pred.Evidence) == 0 || len(pred.Probs) != 3 ||
		(pred.Decision != "confident" && pred.Decision != "escalate") || pred.Threshold <= 0 {
		t.Errorf("predict -json: %s", out)
	}
	out = mustRun("", "jev", "predict", "-model", p("a.jev"), "-text", "readme typo", "-fields", `{"ext":".md"}`)
	if !strings.HasPrefix(out, "docs") || !strings.Contains(out, "evidence:") {
		t.Errorf("predict text output:\n%s", out)
	}
	out = mustRun("{\"text\":\"add support for option\"}\n\n{\"text\":\"panic\",\"fields\":{\"ext\":\".go\"}}\n",
		"jev", "predict", "-model", p("a.jev"))
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.Contains(lines[0], `"label":"feat"`) {
		t.Errorf("predict from stdin:\n%s", out)
	}

	out = mustRun("", "jev", "inspect", p("a.jev"))
	for _, want := range []string{"spec hash:", "2^12 buckets", "multinomial", "adagrad", "LABEL"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output lacks %q:\n%s", want, out)
		}
	}
	out = mustRun("", "jev", "inspect", "-json", p("a.jev"))
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil || info["spec_hash"] == nil {
		t.Errorf("inspect -json: %v\n%s", err, out)
	}

	// Binario uno-contra-resto a partir de los mismos datos multiclase.
	out = mustRun("", "jev", "train", "-data", p("train.jsonl"), "-valid", p("valid.jsonl"), "-buckets", "12",
		"-one-vs-rest", "bug", "-o", p("bug.jev"), "-test", p("test.jsonl"))
	if !strings.Contains(out, "2 labels") || !strings.Contains(out, "not-bug") {
		t.Errorf("one-vs-rest:\n%s", out)
	}

	// Errores: un modelo corrupto y una línea de datos rota se rechazan con un
	// mensaje útil, no con un pánico.
	bad := append([]byte(nil), a...)
	bad[len(bad)/2] ^= 0xff
	os.WriteFile(p("bad.jev"), bad, 0o644)
	if out, err := run("", "jev", "eval", "-model", p("bad.jev"), "-data", p("test.jsonl")); err == nil || !strings.Contains(out, "checksum") {
		t.Errorf("corrupt model: err=%v\n%s", err, out)
	}
	os.WriteFile(p("broken.jsonl"), []byte("{\"text\":\"a\",\"label\":\"x\"}\n{nope\n"), 0o644)
	if out, err := run("", "jev", "eval", "-model", p("a.jev"), "-data", p("broken.jsonl")); err == nil || !strings.Contains(out, "line 2") {
		t.Errorf("broken JSONL: err=%v\n%s", err, out)
	}
	if out, err := run("", "jev", "bogus"); err == nil || !strings.Contains(out, "unknown jev command") {
		t.Errorf("unknown subcommand: %v\n%s", err, out)
	}
}
