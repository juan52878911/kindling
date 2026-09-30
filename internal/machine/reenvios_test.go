package machine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Lo que abre kling-vz tiene que caer en el rango reservado de 127.0.0.1: un
// reenvío fuera (un kling-vz anterior, o en otra IP) se rechaza.
func TestValidarReenvios(t *testing.T) {
	bueno := map[string]string{"8080": "127.0.0.1:29000", "9000": "127.0.0.1:29999"}
	if err := validarReenvios(bueno); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1:61234", "127.0.0.1:28999", "127.0.0.1:30000", "0.0.0.0:29001",
		"127.0.0.2:29001", "[::1]:29001", "basura"} {
		if err := validarReenvios(map[string]string{"8080": addr}); err == nil || !strings.Contains(err.Error(), "rebuild kling-vz") {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// Una credencial cuyo upstream del loopback es el reenvío vivo de otra
// máquina se rechaza al entregarla a una máquina y al atarla a una
// plantilla, sin llegar al proxy; el mismo puerto en la LAN, o el loopback en
// otro puerto, sí valen.
func TestUpstreamEnReenvioDeOtraMaquina(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	mc := m.addForTest("m1")
	otra := m.addForTest("victima")
	m.mu.Lock()
	mc.Egress = string(knet.EgressAllowlist)
	m.socket["m1"] = falso.Sock
	// Un reenvío fuera del rango (kling-vz anterior): el rango no lo cubre y
	// es la lista de reenvíos vivos la que lo para.
	otra.Forwards = map[string]string{"5432": "127.0.0.1:61234"}
	m.mu.Unlock()
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pg := func(up string) []api.CredentialSpec {
		return []api.CredentialSpec{{Domain: "db.example.com", Env: "PGPASSWORD", Secret: "pw-clave-de-prueba", Type: "postgres",
			User: "app", Database: "appdb", Upstream: up, UpstreamTLS: "disable"}}
	}
	for _, up := range []string{"127.0.0.1:61234", "localhost:61234", "[::1]:61234"} {
		if _, err := m.SetCredentials(ctx, "m1", pg(up)); err == nil || !strings.Contains(err.Error(), "victima") {
			t.Errorf("%s: %v", up, err)
		}
	}
	// El rango reservado, aunque ninguna máquina lo use ahora mismo.
	if _, err := m.SetCredentials(ctx, "m1", pg("127.0.0.1:29123")); err == nil || !strings.Contains(err.Error(), "reserved forward range") {
		t.Errorf("rango reservado: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("un rechazo no debe llegar al proxy: %+v", *got)
	}
	for _, up := range []string{"10.0.0.5:61234", "127.0.0.1:5432"} {
		if _, err := m.SetCredentials(ctx, "m1", pg(up)); err != nil {
			t.Errorf("%s: %v", up, err)
		}
	}

	escribirSnapshot(t, m, "svc", api.Snapshot{Egress: "allowlist"})
	if _, err := m.SetSnapshotCredentials("svc", pg("127.0.0.1:61234"), false); err == nil || !strings.Contains(err.Error(), "victima") {
		t.Errorf("plantilla: %v", err)
	}

	// Al reentregar (thaw) se vuelve a mirar: el reenvío pudo abrirse después.
	creds := []credproxy.Credential{{Env: "PGPASSWORD", Domain: "db.example.com", Placeholder: credproxy.PlaceholderPrefix + "aa",
		Secret: "pw-clave-de-prueba", Kind: credproxy.KindPostgres, Port: 5432, User: "app", Database: "appdb", Upstream: "127.0.0.1:61234", UpstreamTLS: "disable"}}
	if err := m.guardarCredenciales("m1", creds); err != nil {
		t.Fatal(err)
	}
	antes := len(*got)
	if _, err := m.reentregarCredenciales(ctx, mc, falso.cliente()); err == nil || !strings.Contains(err.Error(), "victima") {
		t.Errorf("reentrega: %v", err)
	}
	if len(*got) != antes {
		t.Error("una reentrega rechazada llegó al proxy")
	}
}
