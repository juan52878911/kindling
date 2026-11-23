package guest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// H1: el segundo tiempo vence su tope (E/S colgada), la puerta se abre, entra
// un exec nuevo, y cuando la E/S se destraba el segundo tiempo rezagado lo
// corta con cortarExecs: mata un comando de ESTA máquina.
func TestR6SegundoTiempoRezagadoCortaExecNuevo(t *testing.T) {
	falsos(t, nil, nil)
	ri, tope := renovarIdentidad, topeSegundoTiempo
	renovarIdentidad = func() []string { return nil }
	topeSegundoTiempo = 150 * time.Millisecond
	t.Cleanup(func() { renovarIdentidad, topeSegundoTiempo = ri, tope })

	// El .tmp del fichero de generación es un FIFO: os.WriteFile se queda en
	// open(2) hasta que alguien lo abra para leer (E/S colgada).
	fifo := generationFile + ".tmp"
	if err := os.MkdirAll(strings.TrimSuffix(fifo, "/generation.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("resync: %d %s", rr.Code, rr.Body)
	}
	fin := time.Now().Add(3 * time.Second)
	for puertaResync.Cerrada() {
		if time.Now().After(fin) {
			t.Fatal("la puerta no se abrió tras el tope")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Puerta abierta: llega un exec de esta máquina.
	var cortado atomic.Bool
	quitar := registrarVivo(func() { cortado.Store(true) }, func() {})
	defer quitar()

	// La E/S se destraba.
	f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	f.Read(buf)
	f.Close()
	time.Sleep(300 * time.Millisecond)
	if cortado.Load() {
		t.Fatal("FALLO DEMOSTRADO: el segundo tiempo rezagado cortó un exec admitido después de abrir la puerta")
	}
}

// H2: una sesión cuyo entorno pasa de SessionMaxEnv+4096 (+ el búfer de la
// tubería): el lector de la captura deja de leer, el volcador se bloquea al
// escribir y el exec no acaba hasta su plazo, aunque el comando ya terminó.
func TestR6SesionEntornoGrandeCuelgaHastaElPlazo(t *testing.T) {
	limpiarProcesos()
	t.Cleanup(func() { limpiarProcesos() })
	if _, _, err := crearSesion(api.SessionRequest{ID: "gorda"}); err != nil {
		t.Fatal(err)
	}
	guion := `A=$(printf '%0100000d' 0); B=$A; C=$A; D=$A; export A B C D; echo listo`
	rec := httptest.NewRecorder()
	t0 := time.Now()
	ev := runStreaming(context.Background(), os.Environ(),
		api.ExecRequest{Cmd: []string{"sh", "-c", guion}, Session: "gorda"}, 4*time.Second, 1<<20, newEmitter(rec))
	t.Logf("tardó %s; evento final %+v; salida %q", time.Since(t0).Round(time.Millisecond), ev, rec.Body.String())
	if ev.TimedOut {
		t.Fatalf("FALLO DEMOSTRADO: `echo listo` con 400 KiB de entorno exportado agotó el plazo (%s)", time.Since(t0).Round(time.Millisecond))
	}
}

// H3: la caché de direcciones propias no ve una dirección añadida después
// (solo se invalida en un /resync con restauración): una petición con ese
// origen, sin ruta propia en la tabla principal, pasa por "host".
func TestR6DireccionAnadidaDespuesPasaPorHost(t *testing.T) {
	leer, rutas := leerDireccionesPropias, rutasInvitado
	t.Cleanup(func() { leerDireccionesPropias, rutasInvitado = leer, rutas; invalidarDireccionesPropias() })
	propias := []net.IP{net.ParseIP("172.16.0.2")}
	leerDireccionesPropias = func() []net.IP { return propias }
	rutasInvitado = func() ([]rutaIPv4, error) {
		return []rutaIPv4{
			{gw: [4]byte{172, 16, 0, 1}, flags: rtfUp | rtfGateway},
			{dst: [4]byte{172, 16, 0, 0}, mask: [4]byte{255, 255, 255, 252}, flags: rtfUp},
		}, nil
	}
	invalidarDireccionesPropias()
	pide := func(origen string) bool {
		r := httptest.NewRequest(http.MethodPost, "/exec", nil)
		r.RemoteAddr = origen + ":40000"
		la := &net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 1}
		return requestFromGuest(r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(la))))
	}
	pide("100.64.0.7") // primera petición: llena la caché
	// El servicio levanta una interfaz (VPN, dummy) con una /32 nueva.
	propias = append(propias, net.ParseIP("100.99.0.9"))
	if !pide("100.99.0.9") {
		t.Fatal("FALLO DEMOSTRADO: una petición desde una dirección PROPIA añadida tras la primera lectura se toma por el host")
	}
}
