package server

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/reenvio"
)

// PUT /kling/credproxy: con el reenvío al proxy aparte, el VMM anuncia
// credproxy-split, recibe solo los dominios (los desvía a la pasarela) y
// enciende el reenvío; las claves que tuviera su proxy propio se borran. Un
// PUT /kling/credentials después lo apaga.
func TestKlingCredproxy(t *testing.T) {
	r := newRig(t)
	const split = `{"domains":["API.example.com","db.example.com"],"db":true}`
	r.mustFail("PUT", "/kling/credproxy", split, "separate credential proxy")

	gw := netip.MustParseAddr("172.16.0.1")
	r.srv.d.Credentials = credproxy.New(credproxy.Options{})
	r.srv.d.CredIP = gw
	if out := r.must("GET", "/kling/info", ""); strings.Contains(out, credproxy.CapSplit) {
		t.Fatalf("info without the forwarder = %s", out)
	}
	sp := reenvio.New("/dev/null/x.sock", nil)
	r.srv.d.Split = sp
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"`+credproxy.CapSplit+`"`) {
		t.Fatalf("info = %s", out)
	}
	r.mustFail("PUT", "/kling/credproxy", split, "need egress allowlist")
	r.mustFail("PUT", "/kling/credproxy", `{"domains":["db.graph"],"machines_only":true}`, "link broker")
	r.must("PUT", "/kling/network", `{"egress":"allowlist","allow_domains":["other.org"]}`)

	// Una clave en el proxy propio (un daemon anterior se la dio) se borra.
	r.must("PUT", "/kling/credentials", `{"credentials":[{"env":"K","domain":"old.example.com","placeholder":"kling-cred-old","secret":"sk-1-clave-de-prueba"}]}`)
	if out := r.must("PUT", "/kling/credproxy", split); !strings.Contains(out, `"domains":["api.example.com","db.example.com"]`) {
		t.Fatalf("PUT /kling/credproxy = %s", out)
	}
	if !sp.Activo() || !sp.DB() {
		t.Fatal("the forwarder is not on")
	}
	if ip, ok := r.srv.d.Policy.CredHost("api.example.com"); !ok || ip != gw {
		t.Fatal("the credential domain is not diverted to the gateway")
	}
	if _, ok := r.srv.d.Policy.CredHost("old.example.com"); ok {
		t.Fatal("the old domain is still diverted")
	}
	if r.srv.d.Credentials.DBActivo() || r.srv.d.Credentials.PGActivo() {
		t.Fatal("the VMM's own proxy kept credentials")
	}
	r.mustFail("PUT", "/kling/credproxy", `{"domains":["*.example.com"]}`, "exact host name")
	if !sp.Activo() {
		t.Fatal("a rejected set must keep the previous one")
	}
	r.must("PUT", "/kling/credproxy", `{"domains":[]}`)
	if sp.Activo() || sp.DB() {
		t.Fatal("an empty set must turn the forwarder off")
	}
	r.must("PUT", "/kling/credproxy", split)
	r.must("PUT", "/kling/credentials", `{"credentials":[{"env":"K","domain":"old.example.com","placeholder":"kling-cred-old","secret":"sk-1-clave-de-prueba"}]}`)
	if sp.Activo() {
		t.Fatal("PUT /kling/credentials must turn the forwarder off")
	}
}
