package main

import (
	"flag"
	"io"
	"reflect"
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
