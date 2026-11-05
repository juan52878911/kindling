package machine

import (
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// Un puente al host: el broker marca 127.0.0.1:PUERTO solo para la máquina
// que lo tiene en Machine.HostPorts (fijado al crearla), ni para otra ni para
// un puerto que no pidió. Sin -host-port, el loopback del Mac no se alcanza.
// El 8080 vale (el de llama-server): en el loopback no hay agente de ningún
// invitado.
func TestBrokerPuenteHost(t *testing.T) {
	m := escenaModeloA(t)
	eco := nuevoEcoBroker(t, m)
	req := linkbroker.Request{Kind: linkbroker.KindLoopback, Port: 8080}
	if _, _, _, err := pedirBroker(t, m, idAgente, req); err == nil {
		t.Fatal("sin -host-port, el broker abrió el loopback del host")
	}
	m.mu.Lock()
	m.byID[idAgente].HostPorts = []api.HostPort{{Host: 8080, Guest: 8080}, {Host: 11434, Guest: 11434}}
	m.mu.Unlock()
	for _, mal := range []struct {
		origen string
		port   int
	}{{idOtra, 8080}, {idAgente, 11435}, {idAgente, credproxy.ForwardPortMin}} {
		if _, _, _, err := pedirBroker(t, m, mal.origen, linkbroker.Request{Kind: linkbroker.KindLoopback, Port: mal.port}); err == nil {
			t.Fatalf("aceptada: %+v", mal)
		}
	}
	// Una arista hacia otra máquina sigue sin poder llegar al agente.
	if _, _, _, err := pedirBroker(t, m, idAgente, linkbroker.Request{Kind: linkbroker.KindMachine, Machine: idOtra, Port: api.GuestPort}); err == nil {
		t.Fatal("el puente abrió el agente de otra máquina")
	}
	tc, _, _, err := pedirBroker(t, m, idAgente, req)
	if err != nil {
		t.Fatal(err)
	}
	if l := eco.lista(); l[len(l)-1] != "127.0.0.1:8080" {
		t.Fatalf("marcó a %v", l)
	}
	ecoPor(t, tc)
}

// RunRequest.HostPorts: los mismos límites que el CLI, más el rango de los
// reenvíos en el lado del host; y en Linux, un error claro.
func TestValidarPuentesHost(t *testing.T) {
	if err := validarPuentesHost(nil); err != nil {
		t.Fatal(err)
	}
	err := validarPuentesHost([]api.HostPort{{Host: 11434, Guest: 11434}})
	if !puenteHostSoportado {
		if err == nil || !errors.Is(err, ErrRunRequest) || !strings.Contains(err.Error(), "only supported on macOS") {
			t.Fatalf("Linux: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, mal := range [][]api.HostPort{
		{{Host: credproxy.ForwardPortMin, Guest: 9000}},
		{{Host: 11434, Guest: 53}},
		{{Host: 1, Guest: 9000}, {Host: 2, Guest: 9000}},
		{{Host: 70000, Guest: 1}},
	} {
		if err := validarPuentesHost(mal); err == nil || !errors.Is(err, ErrRunRequest) {
			t.Errorf("%+v: %v", mal, err)
		}
	}
}
