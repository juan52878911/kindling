package main

import (
	"os"
	"testing"

	"github.com/juan52878911/kindling/pkg/guest"
)

// Si el Chromium compartido muere, las sesiones conectadas a él se cierran: su
// CDP apunta a un puerto muerto. Antes seguían vivas, con cada herramienta
// fallando, hasta caducar por ociosas o un /reset.
func TestNavegadorMuertoCierraSusSesiones(t *testing.T) {
	var store *guest.MMDSStore
	conMMDS(t, &store)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := direccionLibre(t)
	b := testBridge()
	// El Chromium falso es el propio binario en modo HTTP; la variable va solo
	// en su entorno, no en el de las sesiones, que son el servidor MCP falso.
	b.browser = &browserSpec{
		Sidecar:  []string{"/usr/bin/env", envHTTPFalso + "=" + addr, exe},
		ReadyURL: "http://" + addr + "/"}
	t.Cleanup(b.stopBrowser)

	code, sid := postJSON(b, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if code != 200 || sid == "" {
		t.Fatalf("initialize: %d, sesión %q", code, sid)
	}
	b.mu.Lock()
	s := b.sessions[sid]
	b.mu.Unlock()
	if s == nil || s.navegador == nil {
		t.Fatal("la sesión no quedó atada al navegador compartido")
	}

	h := b.navegadorActual()
	_ = h.cmd.Process.Kill()
	esperar(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, sigue := b.sessions[sid]
		return !sigue
	})
	esperar(t, s.isClosed)

	// La siguiente conversación levanta otro navegador y funciona.
	code, sid2 := postJSON(b, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if code != 200 || sid2 == "" || sid2 == sid {
		t.Fatalf("initialize tras la caída: %d, sesión %q", code, sid2)
	}
	if n := b.navegadorActual(); n == h || !n.vivo() {
		t.Fatal("la sesión nueva no recibió un navegador nuevo")
	}
}
