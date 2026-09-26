package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// Cada incorporada tiene un manifiesto válido, un solo comando con su nombre en
// el grupo AI, y cada subcomando del completado aparece en su ayuda: si alguien
// añade uno al dispatcher y no a la ayuda (o al revés), este test lo dice.
func TestBuiltinManifests(t *testing.T) {
	want := map[string][]string{
		"ai":     {"up", "serve", "ls", "test", "generate", "eval", "calibrate", "reload", "prime", "review", "feedback", "retrain", "rollback"},
		"chispa": {"train", "eval", "predict", "inspect", "deploy", "ls", "rm"},
		"models": {"ls", "add", "ask", "embed", "rm"},
	}
	bs := builtinExtensions()
	if len(bs) != len(want) {
		t.Fatalf("%d builtins, want %d", len(bs), len(want))
	}
	for _, b := range bs {
		m := b.Manifest
		if err := m.Validate(); err != nil {
			t.Errorf("%s: invalid manifest: %v", m.Name, err)
		}
		subs, ok := want[m.Name]
		if !ok {
			t.Errorf("unexpected builtin %q", m.Name)
			continue
		}
		if len(m.Commands) != 1 || m.Commands[0].Name != m.Name {
			t.Fatalf("%s: want one command named like the extension, got %+v", m.Name, m.Commands)
		}
		c := m.Commands[0]
		if c.Group != "AI" || c.Summary == "" || c.Usage == "" {
			t.Errorf("%s: group %q, summary %q, usage empty %v", m.Name, c.Group, c.Summary, c.Usage == "")
		}
		if strings.Join(c.Subcommands, " ") != strings.Join(subs, " ") {
			t.Errorf("%s: subcommands %v, want %v", m.Name, c.Subcommands, subs)
		}
		for _, s := range c.Subcommands {
			if !strings.Contains(c.Usage, "  "+m.Name+" "+s) && !strings.Contains(c.Usage, "| "+s+" ") {
				t.Errorf("%s: subcommand %q missing from its help", m.Name, s)
			}
		}
		if b.Commands[m.Name] == nil {
			t.Errorf("%s: declares its command but does not implement it", m.Name)
		}
	}
}

// Descubiertas junto a las externas, las tres salen como incorporadas, sirven
// su comando y aportan su subcomando al completado.
func TestBuiltinsDiscovered(t *testing.T) {
	reg := plugin.Discover(context.Background(), plugin.Options{
		Core:     []string{"ps", "run"},
		Builtins: builtinExtensions(),
		Path:     []string{t.TempDir()},
	})
	for _, n := range []string{"ai", "chispa", "models"} {
		p := reg.Lookup(n)
		if p == nil || p.Builtin == nil || p.Err != nil {
			t.Fatalf("%s: not served by a usable builtin: %+v", n, p)
		}
	}
	var help bytes.Buffer
	plugin.WriteHelp(&help, reg.Commands())
	for _, s := range []string{"AI\n", "  ai up ", "  chispa train ", "  models embed "} {
		if !strings.Contains(help.String(), s) {
			t.Errorf("help lacks %q:\n%s", s, help.String())
		}
	}
}

// `kling help ai` llega como `ai -h`: imprime la ayuda del manifiesto sin
// tocar el daemon.
func TestBuiltinHelp(t *testing.T) {
	var buf bytes.Buffer
	writeBuiltinHelp(&buf, "ai", "sum", aiHelp)
	if !strings.HasPrefix(buf.String(), "kling ai - sum\n\nUSAGE\n  ai up ") {
		t.Fatalf("help:\n%s", buf.String())
	}
	for _, a := range []string{"-h", "--help", "help"} {
		if !isHelpArg(a) {
			t.Errorf("%q is not taken as help", a)
		}
	}
}

func TestAIUpConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("KLING_CONFIG", filepath.Join(cfgDir, "config.json"))
	work := t.TempDir()
	t.Chdir(work)

	// Sin ninguno: error que dice dónde se buscó.
	if _, err := aiUpConfig(""); err == nil || !strings.Contains(err.Error(), filepath.Join(cfgDir, "ai.json")) {
		t.Fatalf("no registry: %v", err)
	}
	// El de junto a config.json.
	home := filepath.Join(cfgDir, "ai.json")
	if err := os.WriteFile(home, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := aiUpConfig(""); err != nil || got != home {
		t.Fatalf("got %q, %v; want %q", got, err, home)
	}
	// El del directorio actual gana.
	if err := os.WriteFile(filepath.Join(work, "ai.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := aiUpConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(got); filepath.Base(got) != "ai.json" || filepath.Dir(real) != mustEval(t, work) {
		t.Fatalf("got %q, want ./ai.json", got)
	}
	// -config manda.
	if got, _ := aiUpConfig("/x/y.json"); got != "/x/y.json" {
		t.Fatalf("explicit: %q", got)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAIUpURLAndBanner(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		":9000":          "http://127.0.0.1:9000",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"[::1]:8080":     "http://[::1]:8080",
	} {
		if got := aiUpURL(in); got != want {
			t.Errorf("aiUpURL(%q) = %q, want %q", in, got, want)
		}
	}
	var buf bytes.Buffer
	printAIUpBanner(&buf, "/c/ai.json", aiUpListen, "/c/ai.token", false)
	out := buf.String()
	for _, s := range []string{"http://127.0.0.1:8080", "/c/ai.json", `$(cat /c/ai.token)`, "http://127.0.0.1:8080/v1/models"} {
		if !strings.Contains(out, s) {
			t.Errorf("banner lacks %q:\n%s", s, out)
		}
	}
	buf.Reset()
	printAIUpBanner(&buf, "/c/ai.json", aiUpListen, "/c/ai.token", true)
	if !strings.Contains(buf.String(), "$KLING_AI_TOKEN") {
		t.Errorf("banner with env token:\n%s", buf.String())
	}
}
