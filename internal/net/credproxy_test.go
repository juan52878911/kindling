package net

import (
	"encoding/binary"
	"io"
	stdnet "net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Tests de la parte Linux del proxy de credenciales: el resolver y el
// listener por netns. La lógica del proxy se prueba en pkg/credproxy.

// El resolver contesta un dominio con credencial con la IP del proxy, sin
// reenviar al upstream (que aquí no existe) ni sembrar nada; AAAA sale vacío.
func TestResolverDesviaElDominioConCredencial(t *testing.T) {
	r := newTestResolver("127.0.0.1:1") // un upstream que no contesta: no debe usarse
	var calls int32
	defer fakeEjecutar(&calls)()
	r.setCredHosts([]string{"API.Stripe.com"}, stdnet.ParseIP("172.30.0.1"))

	resp := r.process(buildQuery(7, "api.stripe.com", 1), false)
	if len(resp) < 12 || binary.BigEndian.Uint16(resp[6:8]) != 1 || resp[3]&0x0F != 0 {
		t.Fatalf("respuesta A inválida: % x", resp)
	}
	if got := stdnet.IP(resp[len(resp)-4:]).String(); got != "172.30.0.1" {
		t.Errorf("A = %s, quería la IP del proxy", got)
	}
	if !responseMatches(buildQuery(7, "api.stripe.com", 1), resp) {
		t.Error("la respuesta sintética no casa con su consulta")
	}
	aaaa := r.process(buildQuery(8, "api.stripe.com", 28), false)
	if binary.BigEndian.Uint16(aaaa[6:8]) != 0 || aaaa[3]&0x0F != 0 {
		t.Errorf("AAAA debería ser NOERROR vacío: % x", aaaa)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("se sembró el ipset %d veces para un dominio con credencial", calls)
	}
	// Un dominio con credencial no hace permitido a su padre ni a sus hermanos.
	if r.isCredHost("stripe.com") || r.isCredHost("files.stripe.com") {
		t.Error("isCredHost casa más de lo pedido")
	}
}

// pkg/credproxy lleva su propia copia de los rangos bloqueados (vz/ no puede
// importar este paquete). Si una cambia sin la otra, el proxy podría llevar la
// clave a un rango que el firewall sí corta.
func TestCredproxyBloqueaLoMismoQueElFirewall(t *testing.T) {
	if got := credproxy.BlockedCIDRs(); !reflect.DeepEqual(got, blocked) {
		t.Fatalf("pkg/credproxy bloquea %v y el firewall %v", got, blocked)
	}
}

// Sin resolver propio (la máquina no está en allowlist) no hay a quién avisar
// de los dominios desviados: error y ningún proxy escuchando.
func TestSetCredentialsSinResolverFalla(t *testing.T) {
	n := &Net{NS: "kl-test-sinres", HostIP: "127.0.0.1"}
	creds := []credproxy.Credential{{Domain: "example.com", Placeholder: credproxy.PlaceholderPrefix + "x", Secret: "s"}}
	if err := SetCredentials(n, creds, ""); err == nil {
		t.Fatal("debería fallar sin resolver")
	}
	credMu.Lock()
	defer credMu.Unlock()
	if credProxies[n.NS] != nil {
		t.Error("arrancó un proxy para una máquina sin resolver")
	}
}

// Con resolver, SetCredentials arranca el proxy en HostIP:credPort, avisa al
// resolver con los dominios normalizados y stopCredProxy lo cierra, dejando
// escrito el registro de auditoría.
func TestSetCredentialsArrancaElProxyYAvisaAlResolver(t *testing.T) {
	const ns = "kl-test-cred"
	r := newTestResolver("127.0.0.1:1")
	var calls int32
	defer fakeEjecutar(&calls)()
	resolversMu.Lock()
	resolvers[ns] = r
	resolversMu.Unlock()
	defer func() {
		resolversMu.Lock()
		delete(resolvers, ns)
		resolversMu.Unlock()
	}()
	n := &Net{NS: ns, HostIP: "127.0.0.1"}
	creds := []credproxy.Credential{{Domain: "API.Example.com", Placeholder: credproxy.PlaceholderPrefix + "x", Secret: "s"}}
	audit := filepath.Join(t.TempDir(), credproxy.AuditFile)
	if err := SetCredentials(n, creds, audit); err != nil {
		t.Skipf("no se pudo escuchar en 127.0.0.1:%d: %v", credPort, err)
	}
	defer stopCredProxy(ns)
	if creds[0].Domain != "api.example.com" || !r.isCredHost("api.example.com") {
		t.Fatalf("dominio %q; el resolver debería desviarlo", creds[0].Domain)
	}
	// Un host sin credencial: el proxy contesta 403 sin salir a ningún sitio.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(credPort)+"/", nil)
	req.Host = "otro.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, quería 403", resp.StatusCode)
	}
	stopCredProxy(ns)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Error("el proxy sigue escuchando tras stopCredProxy")
	}
	// stopCredProxy cierra el registro: la línea del 403 ya está en disco.
	b, err := os.ReadFile(audit)
	if err != nil || !strings.Contains(string(b), `"reason":"no_credential"`) || !strings.Contains(string(b), `"denied":true`) {
		t.Fatalf("registro tras parar el proxy: %q (%v)", b, err)
	}
}
