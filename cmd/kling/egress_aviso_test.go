package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

type fakeEgress struct {
	e   *api.EgressBlocked
	err error
}

func (f fakeEgress) EgressBlocked(context.Context, string) (*api.EgressBlocked, error) {
	return f.e, f.err
}

func TestAvisoEgress(t *testing.T) {
	ctx := context.Background()
	e := &api.EgressBlocked{Egress: "none", Total: 2, Destinations: []api.BlockedDest{{Dest: "example.com", Kind: "dns", Count: 2}}}
	if a := avisoEgress(ctx, fakeEgress{e: e}, "m"); !strings.Contains(a, "-egress allowlist -allow example.com") {
		t.Fatalf("aviso: %q", a)
	}
	// Un fallo, un daemon viejo o nada cortado: sin aviso y sin error.
	if a := avisoEgress(ctx, fakeEgress{err: errors.New("404")}, "m"); a != "" {
		t.Fatalf("error: %q", a)
	}
	if a := avisoEgress(ctx, fakeEgress{e: &api.EgressBlocked{Egress: "none"}}, "m"); a != "" {
		t.Fatalf("vacío: %q", a)
	}
	if a := avisoEgress(ctx, struct{}{}, "m"); a != "" {
		t.Fatalf("sin método: %q", a)
	}
}
