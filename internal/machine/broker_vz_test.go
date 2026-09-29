//go:build darwin

package machine

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// El broker de verdad en macOS: escucha en su socket del directorio privado,
// sabe quién pregunta por el PID del otro extremo (aquí, este proceso hace de
// kling-vz de web) y entrega por él un socket conectado al reenvío de api.
// Un proceso que no es el VMM de ninguna máquina no obtiene nada.
func TestVZBrokerPorSocket(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	eco := nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	previo := plazoIdentificar
	plazoIdentificar = 200 * time.Millisecond
	t.Cleanup(func() { plazoIdentificar = previo })

	e.m.iniciarBroker()
	t.Cleanup(e.m.cerrarBroker)
	if e.m.brokerRuta == "" {
		t.Fatal("el broker no escucha")
	}
	if !strings.HasPrefix(e.m.brokerRuta, "/tmp/kling-") || len(e.m.brokerRuta) >= 104 {
		t.Fatalf("ruta del broker %q", e.m.brokerRuta)
	}
	fi, err := os.Stat(e.m.brokerRuta)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("permisos del socket: %v %v", fi.Mode(), err)
	}
	if env := strings.Join(e.m.entornoVMM(), " "); !strings.Contains(env, "KLING_VZ_BROKER="+e.m.brokerRuta) {
		t.Fatalf("kling-vz no recibe el broker: %s", env)
	}

	pedir := func() (*net.TCPConn, linkbroker.Response, error) {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: e.m.brokerRuta, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		tc, resp, err := linkbroker.Pedir(c, linkbroker.Request{V: linkbroker.Version, Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081}, 5*time.Second)
		if err == nil {
			t.Cleanup(func() { tc.Close(); c.Close() })
		}
		return tc, resp, err
	}

	// Este proceso no es el VMM de ninguna máquina.
	if _, _, err := pedir(); err == nil {
		t.Fatal("un proceso cualquiera obtuvo una conexión")
	}
	if len(eco.lista()) != 0 {
		t.Fatal("se marcó para un desconocido")
	}

	// Ahora es el kling-vz de web.
	e.m.mu.Lock()
	e.m.byID[web].PID = os.Getpid()
	e.m.mu.Unlock()
	tc, resp, err := pedir()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Machine != apiID {
		t.Fatalf("máquina %s, quería %s", resp.Machine, apiID)
	}
	ecoPor(t, tc)

	// Congelar api corta la sesión (invalidarSesiones -> broker).
	invalidarEnlaces = invalidarEnlacesPlataforma
	e.m.mu.Lock()
	e.m.byID[apiID].State = api.StateWarm
	e.m.mu.Unlock()
	e.m.invalidarSesiones(apiID, "frozen")
	if !cortadaTCP(tc) {
		t.Fatal("la sesión siguió tras congelar el destino")
	}
}
