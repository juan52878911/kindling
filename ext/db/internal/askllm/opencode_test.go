package askllm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/sqlguard"
)

func fakeOC(t *testing.T, mode string) (*OpenCode, string) {
	t.Helper()
	bin, err := filepath.Abs("testdata/fake-opencode.sh")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_OC_MODE", mode)
	t.Setenv("FAKE_OC_LOG", log)
	return NewOpenCode(bin, "", 10*time.Second), log
}

func TestOpenCodeBloqueDeCodigo(t *testing.T) {
	o, log := fakeOC(t, "block")
	out, err := o.Complete(context.Background(), "SYS", "PROMPT-Q")
	if err != nil {
		t.Fatal(err)
	}
	sql, err := sqlguard.Extract(out)
	if err != nil || sql != "SELECT count(*) FROM clientes" {
		t.Fatalf("Extract(%q) = %q, %v", out, sql, err)
	}
	b, _ := os.ReadFile(log)
	l := string(b)
	for _, want := range []string{"run --pure -m " + DefaultOpenCodeModel, "--format json", "STDIN: SYS  PROMPT-Q"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	// El prompt (esquema y pregunta) no va en el argv: lo vería cualquiera con ps.
	if args := strings.SplitN(l, "\n", 2)[0]; strings.Contains(args, "SYS") || strings.Contains(args, "PROMPT-Q") {
		t.Errorf("the prompt went through argv: %s", args)
	}
	if strings.Contains(strings.SplitN(l, "\n", 2)[0], "--auto") || !strings.Contains(l, "LS: \n") {
		t.Errorf("unsafe args or non-empty dir:\n%s", l)
	}
	// El directorio temporal se borra.
	for _, line := range strings.Split(l, "\n") {
		if p, ok := strings.CutPrefix(line, "PWD: "); ok {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("scratch dir %s still exists", p)
			}
		}
	}
}

// Un esquema grande (más que un argumento de Linux) también va por stdin, sin
// fichero adjunto en el directorio.
func TestOpenCodePromptGrandePorStdin(t *testing.T) {
	o, log := fakeOC(t, "block")
	if _, err := o.Complete(context.Background(), "SYS", strings.Repeat("x", 200<<10)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	l := string(b)
	if !strings.Contains(l, "STDIN: SYS  xxx") || !strings.Contains(l, "LS: \n") || strings.Contains(l, "-f ") {
		t.Errorf("big prompt not sent by stdin:\n%.300s", l)
	}
}

func TestOpenCodeTextoAlrededor(t *testing.T) {
	o, _ := fakeOC(t, "around")
	out, err := o.Complete(context.Background(), "", "q")
	if err != nil {
		t.Fatal(err)
	}
	if sql, err := sqlguard.Extract(out); err != nil || sql != "SELECT 1" {
		t.Fatalf("Extract(%q) = %q, %v", out, sql, err)
	}
}

func TestOpenCodeHerramientaAborta(t *testing.T) {
	o, _ := fakeOC(t, "tool")
	_, err := o.Complete(context.Background(), "", "q")
	if err == nil || !strings.Contains(err.Error(), "tool_use") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeMalasSalidas(t *testing.T) {
	for mode, want := range map[string]string{
		"notjson":  "not the expected JSON",
		"nofinish": "without a finished step",
		"length":   "reason",
		"fail":     "bad credentials",
		"":         "without a finished step", // no emite nada
	} {
		o, _ := fakeOC(t, mode)
		_, err := o.Complete(context.Background(), "", "q")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", mode, err, want)
		}
	}
}

func TestOpenCodeTimeoutMataElGrupo(t *testing.T) {
	o, _ := fakeOC(t, "hang")
	pidfile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_OC_PIDFILE", pidfile)
	o.Timeout = 500 * time.Millisecond
	t0 := time.Now()
	_, err := o.Complete(context.Background(), "", "q")
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(t0); d > 8*time.Second {
		t.Fatalf("took %s", d)
	}
	b, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("no grandchild pid")
	}
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the grandchild survived the timeout")
}

func TestOpenCodeNoExiste(t *testing.T) {
	o := NewOpenCode("/nonexistent/opencode", "", 0)
	if _, err := o.Complete(context.Background(), "", "q"); err == nil {
		t.Fatal("want error")
	}
	if o.Timeout != DefaultLLMTimeout || o.Model != DefaultOpenCodeModel {
		t.Fatalf("defaults %+v", o)
	}
}

func TestSelect(t *testing.T) {
	t.Setenv(EnvFake, "")
	t.Setenv(EnvProvider, "")
	t.Setenv(EnvKey, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if _, err := Select("", "", 0); err != ErrNoProvider {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
	if _, err := Select("opencode", "", 0); err == nil {
		t.Fatal("opencode not installed: want error")
	}
	if _, err := Select("gpt", "", 0); err == nil {
		t.Fatal("unknown provider accepted")
	}
	// ~/.opencode/bin/opencode
	bin := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Select("", "", 0)
	if err != nil || !strings.HasPrefix(p.Name(), "opencode (") {
		t.Fatalf("Select = %v, %v", p, err)
	}
	// Con clave, anthropic por defecto; -provider opencode manda.
	t.Setenv(EnvKey, testKey)
	if p, err := Select("", "", 0); err != nil || !strings.HasPrefix(p.Name(), "Anthropic") {
		t.Fatalf("Select = %v, %v", p, err)
	}
	if p, _ := Select("opencode", "m", 0); !strings.Contains(p.Name(), "(m)") {
		t.Fatalf("Select = %v", p)
	}
	t.Setenv(EnvProvider, "opencode")
	if p, _ := Select("", "", 0); !strings.HasPrefix(p.Name(), "opencode") {
		t.Fatalf("env provider ignored: %v", p)
	}
	if p, _ := Select("anthropic", "", 0); !strings.HasPrefix(p.Name(), "Anthropic") {
		t.Fatalf("flag must win: %v", p)
	}
}

func TestFake(t *testing.T) {
	f := filepath.Join(t.TempDir(), "sql")
	if err := os.WriteFile(f, []byte("```sql\nSELECT 2\n```"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvFake, f)
	p, err := Select("anthropic", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := p.Complete(context.Background(), "", "")
	if sql, _ := sqlguard.Extract(out); sql != "SELECT 2" || !strings.Contains(p.Name(), "test") {
		t.Fatalf("%q %s", out, p.Name())
	}
	t.Setenv(EnvFake, filepath.Join(t.TempDir(), "nada"))
	if _, err := Select("", "", 0); err == nil {
		t.Fatal("missing fake file accepted")
	}
}

// Un fallo del proveedor se informa con su mensaje (sin caracteres de control)
// y, tras un reintento que también falla, se devuelve.
func TestOpenCodeFalloDelProveedor(t *testing.T) {
	o, _ := fakeOC(t, "provider")
	_, err := o.Complete(context.Background(), "sys", "q")
	var pe *providerError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, quería un providerError", err)
	}
	if !strings.Contains(err.Error(), "upstream") || !strings.Contains(err.Error(), "overloaded") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("mensaje = %q", err)
	}
}

// Un fallo intermitente del proveedor se arregla con el reintento.
func TestOpenCodeReintentaUnaVez(t *testing.T) {
	o, _ := fakeOC(t, "provideronce")
	got, err := o.Complete(context.Background(), "sys", "q")
	if err != nil || strings.TrimSpace(got) != "SELECT 2" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// Una herramienta NO se reintenta: aborta a la primera.
func TestOpenCodeHerramientaNoSeReintenta(t *testing.T) {
	o, _ := fakeOC(t, "tool")
	_, err := o.Complete(context.Background(), "sys", "q")
	var pe *providerError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("err = %v, quería un aborto que no sea providerError", err)
	}
}
