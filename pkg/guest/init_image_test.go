package guest

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Con Init en el servicio de la imagen, el agente se lanza como hijo y el
// init de la imagen toma el PID 1.
func TestInitImageService(t *testing.T) {
	f := newFakeInit()
	f.files[api.GuestServiceSpec] = `{"argv":["/init","--x"],"init":true}`
	f.files["/init"] = "bin"
	if err := runInit(f, nil); err != nil {
		t.Fatal(err)
	}
	n := len(f.calls)
	if n < 2 || f.calls[n-2] != "spawn /usr/local/bin/kling-guest -listen :8080" || f.calls[n-1] != "exec /init" {
		t.Fatalf("last calls = %q", f.calls[max(0, n-2):])
	}
	if strings.Join(f.exec, " ") != "/init --x" {
		t.Errorf("argv = %q", f.exec)
	}

	// Sin Init, el agente es el PID 1 como siempre.
	g := newFakeInit()
	g.files[api.GuestServiceSpec] = `{"argv":["/srv"]}`
	if err := runInit(g, nil); err != nil {
		t.Fatal(err)
	}
	if last := g.calls[len(g.calls)-1]; last != "exec /usr/local/bin/kling-guest" {
		t.Errorf("last call = %q", last)
	}
}
