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

// La incorporada `ai` tiene un manifiesto válido, un comando por cada
// subcomando del dispatcher, y cada uno con su bloque de ayuda: si alguien
// añade uno al dispatcher y no a la ayuda (o al revés), este test lo dice.
func TestBuiltinManifests(t *testing.T) {
	want := []string{"up", "serve", "ls", "test", "generate", "eval", "calibrate", "reload",
		"prime", "review", "feedback", "retrain", "rollback", "model", "chispa"}
	bs := builtinExtensions()
	if len(bs) != 1 || bs[0].Manifest.Name != "ai" {
		t.Fatalf("builtins: %+v", bs)
	}
	m := bs[0].Manifest
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	if m.HelpGroup != "SERVE" || m.Summary == "" {
		t.Errorf("group %q, summary %q", m.HelpGroup, m.Summary)
	}
	var names []string
	for _, c := range m.Commands {
		names = append(names, c.Name)
		if c.Usage == "" || c.Summary == "" || !strings.Contains(c.Usage, "  ai "+c.Name) {
			t.Errorf("%s: summary %q, usage %q", c.Name, c.Summary, c.Usage)
		}
		if bs[0].Commands[c.Name] == nil {
			t.Errorf("%s: declared but not implemented", c.Name)
		}
		if m.IsTopLevel(&c) {
			t.Errorf("%s: must live under `kling ai`", c.Name)
		}
		for _, s := range c.Subcommands {
			if !strings.Contains(c.Usage, "  ai "+c.Name+" "+s) {
				t.Errorf("ai %s: subcommand %q missing from its help", c.Name, s)
			}
		}
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("commands %v, want %v", names, want)
	}
}

// Descubierta junto a las externas, `ai` es un espacio de nombres: sirve
// `kling ai up` y aporta sus comandos al completado y a la ayuda.
func TestBuiltinsDiscovered(t *testing.T) {
	reg := plugin.Discover(context.Background(), plugin.Options{
		Core:     []string{"ps", "run"},
		Builtins: builtinExtensions(),
		Path:     []string{t.TempDir()},
	})
	p := reg.Lookup("ai")
	if p == nil || p.Builtin == nil || p.Err != nil || !reg.IsNamespace("ai") {
		t.Fatalf("ai: not served by a usable builtin: %+v", p)
	}
	if _, argv := reg.Resolve("ai", []string{"model", "ls"}); strings.Join(argv, " ") != "model ls" {
		t.Fatalf("resolve: %v", argv)
	}
	var help bytes.Buffer
	plugin.WriteNamespace(&help, "kling ai", *p.Manifest)
	for _, s := range []string{"kling ai — ", "GATEWAY\n", "  ai up ", "  ai chispa train ", "  ai model embed "} {
		if !strings.Contains(help.String(), s) {
			t.Errorf("help lacks %q:\n%s", s, help.String())
		}
	}
	cmds := reg.Commands()
	if len(cmds) != 1 || cmds[0].Name != "ai" || !contains(cmds[0].Subcommands, "model") {
		t.Fatalf("top level: %+v", cmds)
	}
}

// El árbol del núcleo: bloques por subcomando, alias y qué es una petición
// de ayuda.
func TestTree(t *testing.T) {
	hermetic(t)
	for _, name := range []string{"run", "save", "template", "image", "volume", "machine", "plugin", "help", "try"} {
		if coreCommand(name) == nil {
			t.Errorf("%s missing from the tree", name)
		}
	}
	if b := subBlock(coreCommand("template"), "rm"); !strings.HasPrefix(b, "  template rm ") || strings.Contains(b, "template ls") {
		t.Errorf("subBlock: %q", b)
	}
	for in, want := range map[string]string{
		"add x": "mcp add x", "gateway -listen :1": "mcp serve -listen :1", "models add m": "ai model add m",
		"chispa train": "ai chispa train", "commit a b": "save a b", "rmi x": "template rm x",
		"snapshots ls": "template ls", "plugins": "plugin", "info -json": "status -v -json",
		"mmds x": "machine secret x", "ps -a": "ps -a",
	} {
		f := strings.Fields(in)
		c, a := resolveAlias(f[0], f[1:])
		if got := strings.Join(append([]string{c}, a...), " "); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	for _, a := range []string{"commit", "snapshots", "plugins", "models"} {
		if !contains(coreCommands, a) {
			t.Errorf("alias %q must be reserved to the core", a)
		}
	}
	if contains(coreCommands, "add") {
		t.Error("`add` must stay available to a 0.13 kling-mcp")
	}
	for in, want := range map[string]bool{
		"run -h": true, "template rm -h": true, "ai model -h": true, "ai model add -h": true,
		"exec box -h": false, "exec box ls -h": false, "run -name x -h": false, "nope -h": false,
		"ai zzz -h": false, "help": false,
	} {
		f := strings.Fields(in)
		if _, ok := helpRequest(f[0], f[1:]); ok != want {
			t.Errorf("helpRequest(%q) = %v, want %v", in, ok, want)
		}
	}
	var b bytes.Buffer
	printUsage(&b)
	if !strings.Contains(b.String(), "START HERE") || !strings.Contains(b.String(), "kling help all") || strings.Contains(b.String(), "mmds") {
		t.Errorf("default screen:\n%s", b.String())
	}
	b.Reset()
	printUsageAll(&b)
	for _, s := range []string{"START HERE\n", "ADVANCED\n", "  machine resize ", "AI — ", "  ai model add ", "CONNECTION\n"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("help all lacks %q", s)
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
