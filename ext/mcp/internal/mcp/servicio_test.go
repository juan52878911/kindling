package mcp

import (
	"context"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// En /mcp/_all una herramienta es servicio.herramienta y se parte por el
// primer punto: un servicio con punto en el nombre lo publicaría otro (a
// con la herramienta b.c sería a.b.c, la c de a.b). No se admite.
func TestNombreDeServicioSinPunto(t *testing.T) {
	for _, ok := range []string{"files", "engram", "mcp-server_2", "A1"} {
		if err := ValidServiceName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, mal := range []string{"", "a.b", "files.", ".x", "-x", "a/b", "con espacio",
		"x123456789012345678901234567890123456789012345678901234567890abcd"} {
		if ValidServiceName(mal) == nil {
			t.Errorf("%q aceptado", mal)
		}
	}

	ctx := context.Background()
	_, c := newFakeDaemon(t, true)
	if _, err := SetLink(ctx, c, &Link{Name: "a.b", URL: "http://mac:9100/mcp"}); err == nil {
		t.Error("enlace a.b aceptado")
	}
	if _, err := SetLink(ctx, c, &Link{Name: "mem", URL: "http://mac:9100/mcp",
		Labels: map[string]string{api.LabelService: "a.b"}}); err == nil {
		t.Error("enlace con service=a.b aceptado")
	}
	if _, err := SetLink(ctx, c, &Link{Name: "mem.local", URL: "http://mac:9100/mcp",
		Labels: map[string]string{api.LabelService: "mem"}}); err != nil {
		t.Errorf("el nombre del enlace puede llevar punto si su servicio no: %v", err)
	}
}
