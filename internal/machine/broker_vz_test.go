//go:build darwin

package machine

import (
	"net"
	"os"
	"path/filepath"
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

	// Ahora es el VMM apuntado de web, pero su ejecutable no es el kling-vz
	// del daemon (un PID reciclado por otro programa): nada.
	e.m.mu.Lock()
	e.m.byID[web].PID = os.Getpid()
	e.m.fcBin = "/usr/local/bin/kling-vz"
	e.m.mu.Unlock()
	if _, _, err := pedir(); err == nil {
		t.Fatal("un proceso que no es kling-vz obtuvo una conexión")
	}
	if len(eco.lista()) != 0 {
		t.Fatal("se marcó para un proceso que no es kling-vz")
	}

	// Y ahora este ejecutable es "el kling-vz" del daemon.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e.m.mu.Lock()
	e.m.fcBin = exe
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

// La identidad del otro extremo: su UID (LOCAL_PEERCRED) y la ruta de su
// ejecutable (proc_pidpath por proc_info), sin cgo.
func TestVZIdentidadDelOtro(t *testing.T) {
	a, b := parUnix(t)
	defer a.Close()
	defer b.Close()
	uid, err := uidDelOtro(a)
	if err != nil || uid != os.Geteuid() {
		t.Fatalf("uid del otro %d %v, quería %d", uid, err, os.Geteuid())
	}
	pid, err := pidDelOtro(a)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid del otro %d %v", pid, err)
	}
	ruta, err := rutaEjecutable(pid)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if ruta != exe {
		t.Fatalf("ejecutable %q, quería %q", ruta, exe)
	}
	if _, err := rutaEjecutable(1 << 30); err == nil {
		t.Fatal("un PID que no existe tiene ejecutable")
	}
	for _, c := range []struct {
		ruta, bin string
		ok        bool
	}{
		{"/opt/k/kling-vz", "kling-vz", true},
		{"/opt/k/kling-vz", "/opt/k/kling-vz", true},
		{"/opt/k/kling-vz", "/usr/bin/kling-vz", false},
		{"/opt/k/otro", "kling-vz", false},
		{"/opt/k/kling-vz", "", false},
	} {
		if got := mismoEjecutable(c.ruta, c.bin); got != c.ok {
			t.Errorf("mismoEjecutable(%q, %q) = %v", c.ruta, c.bin, got)
		}
	}
}

// Las conexiones cuyo PID todavía no es de ninguna máquina esperan, pero como
// mucho maxEsperandoIdentidad a la vez: la siguiente se rechaza en el acto.
func TestVZBrokerAcotaLasQueEsperan(t *testing.T) {
	m := newTestManager(t)
	exe, _ := os.Executable()
	m.fcBin = exe
	previo := plazoIdentificar
	plazoIdentificar = 10 * time.Second
	t.Cleanup(func() { plazoIdentificar = previo })
	for i := 0; i < maxEsperandoIdentidad; i++ {
		esperandoIdentidad <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < maxEsperandoIdentidad; i++ {
			<-esperandoIdentidad
		}
	})
	a, b := parUnix(t)
	defer a.Close()
	defer b.Close()
	inicio := time.Now()
	if _, err := m.identificarBroker(a); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("con los huecos llenos: %v", err)
	}
	if d := time.Since(inicio); d > time.Second {
		t.Fatalf("esperó %v en vez de rechazar en el acto", d)
	}
}
