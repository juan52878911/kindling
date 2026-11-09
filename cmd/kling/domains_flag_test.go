package main

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

// -allow repetido suma dominios en vez de quedarse con el último (visto al
// probar RDS: -allow dl-cdn... -allow <rds> dejaba sin resolver el primero).
func TestAllowRepetidoSeSuma(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var allow domainsFlag
	fs.Var(&allow, "allow", "")
	if err := fs.Parse([]string{"-allow", "a.example.com", "-allow", "b.example.com,c.example.com", "-allow", " "}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a.example.com", "b.example.com", "c.example.com"}
	if !reflect.DeepEqual([]string(allow), want) {
		t.Fatalf("got %v, want %v", allow, want)
	}
	if got := splitDomains(allow.String()); !reflect.DeepEqual(got, want) {
		t.Fatalf("splitDomains(%q) = %v", allow.String(), got)
	}
}

// -allow github.io avisa en stderr de que es un sufijo multiinquilino (la
// allowlist filtra por IP); -allow github.com no dice nada.
func TestAvisarAllow(t *testing.T) {
	var b strings.Builder
	avisarAllow(&b, splitDomains("github.com,github.io"))
	if got := b.String(); strings.Count(got, "warning:") != 1 || !strings.Contains(got, `"github.io" is a shared hosting suffix`) {
		t.Fatalf("aviso = %q", got)
	}
	b.Reset()
	avisarAllow(&b, splitDomains("github.com, pypi.org"))
	if b.Len() != 0 {
		t.Fatalf("dominios normales no avisan: %q", b.String())
	}
}
