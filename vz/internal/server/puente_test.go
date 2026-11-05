package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/grafo"
)

// PUT /kling/host-ports: solo con broker (cada conexión la marca el daemon),
// con las reglas de los puentes; los puertos llegan a la red como puertos de
// Graph y el DNS contesta host.kindling.internal con la pasarela. La lista
// vacía lo quita todo. La política de salida no cambia.
func TestKlingHostPorts(t *testing.T) {
	r := newRig(t)
	gw := netip.MustParseAddr("172.16.0.1")
	r.srv.d.Credentials = credproxy.New(credproxy.Options{})
	r.srv.d.CredIP = gw
	const puentes = `{"ports":[{"host":11434,"guest":11434},{"host":8080,"guest":18080}]}`

	r.mustFail("PUT", "/kling/host-ports", puentes, "no link broker")

	r.srv.d.Graph = grafo.NewConDial(func(context.Context) (*net.UnixConn, error) {
		return nil, errors.New("no daemon in this test")
	}, nil, nil)
	r.mustFail("PUT", "/kling/host-ports", `{"ports":[{"host":11434,"guest":53}]}`, "reserved")
	r.mustFail("PUT", "/kling/host-ports", `{"ports":[{"host":29000,"guest":9000}]}`, "forward range")
	modo := r.srv.d.Policy.Mode()
	r.must("PUT", "/kling/host-ports", puentes)
	if !r.srv.d.Graph.Link(11434) || !r.srv.d.Graph.Link(18080) || r.srv.d.Graph.Link(8080) {
		t.Fatal("the host ports did not reach the network by their guest port")
	}
	if ip, _, ok := r.srv.d.Policy.HostBridge("host.kindling.internal"); !ok || ip != gw {
		t.Fatal("host.kindling.internal is not answered with the gateway")
	}
	if r.srv.d.Policy.Mode() != modo {
		t.Fatal("host ports changed the egress policy")
	}
	r.must("PUT", "/kling/host-ports", `{"ports":[]}`)
	if r.srv.d.Graph.Link(11434) {
		t.Fatal("an empty list did not clear the host ports")
	}
	if _, _, ok := r.srv.d.Policy.HostBridge("host.kindling.internal"); ok {
		t.Fatal("host.kindling.internal still resolves without host ports")
	}
}
