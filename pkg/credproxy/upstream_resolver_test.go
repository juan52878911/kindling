package credproxy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
)

// ResolverUpstream (macOS: el daemon resuelve por kling-vz) aplica lo mismo
// que dialFijado: una IP prohibida o del rango de reenvíos entre las
// respuestas basta para no fijar ninguna; una IP o localhost no se resuelven.
func TestResolverUpstream(t *testing.T) {
	respuestas := map[string][]netip.Addr{
		"db.lan":      {netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("10.0.0.6")},
		"v6.lan":      {netip.MustParseAddr("fd12::5")},
		"mapeada.lan": {netip.MustParseAddr("::ffff:10.0.0.9")},
		"metadatos":   {netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("169.254.169.254")},
		"interno":     {netip.MustParseAddr("172.30.0.2")},
		"bucle.lan":   {netip.MustParseAddr("127.0.0.1")},
		"vacio.lan":   {},
	}
	preguntados := map[string]int{}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		preguntados[host]++
		ips, ok := respuestas[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return ips, nil
	}
	ctx := context.Background()
	for u, want := range map[string]string{
		"db.lan:5432":      "10.0.0.5:5432",
		"DB.lan.:5432":     "10.0.0.5:5432",
		"v6.lan:5432":      "[fd12::5]:5432",
		"mapeada.lan:5432": "10.0.0.9:5432",
		"bucle.lan:5432":   "127.0.0.1:5432",
		"10.0.0.7:5432":    "10.0.0.7:5432",
		"localhost:5432":   "localhost:5432",
		"[::1]:5432":       "[::1]:5432",
	} {
		got, err := ResolverUpstream(ctx, lookup, u)
		if err != nil || got != want {
			t.Errorf("ResolverUpstream(%q) = %q, %v; quería %q", u, got, err, want)
		}
	}
	if preguntados["10.0.0.7"]+preguntados["localhost"]+preguntados["::1"] != 0 {
		t.Errorf("se resolvió una IP o localhost: %v", preguntados)
	}
	for _, u := range []string{"metadatos:5432", "interno:5432"} {
		if _, err := ResolverUpstream(ctx, lookup, u); !errors.Is(err, errUpstreamProhibido) {
			t.Errorf("%s: %v, quería prohibido", u, err)
		}
	}
	// Un nombre que resuelve al loopback no puede ir al rango de reenvíos.
	if _, err := ResolverUpstream(ctx, lookup, fmt.Sprintf("bucle.lan:%d", ForwardPortMin)); !errors.Is(err, errUpstreamProhibido) {
		t.Errorf("rango de reenvíos: %v", err)
	}
	for _, u := range []string{"vacio.lan:5432", "falla.lan:5432"} {
		if got, err := ResolverUpstream(ctx, lookup, u); err == nil {
			t.Errorf("%s: fijado a %q sin resolver", u, got)
		}
	}
}
