package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
)

// direccionLibre reserva un puerto efímero de loopback y lo suelta para que lo
// abra el hijo.
func direccionLibre(t *testing.T) (string, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	return addr, port
}

// conMMDS sustituye el almacén MMDS durante el test.
func conMMDS(t *testing.T, store **guest.MMDSStore) {
	t.Helper()
	antes := fetchMMDS
	fetchMMDS = func() (*guest.MMDSStore, error) { return *store, nil }
	t.Cleanup(func() { fetchMMDS = antes })
}

// puenteProxy arma un puente en modo http con el servidor HTTP falso como hijo.
func puenteProxy(t *testing.T) *bridge {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, port := direccionLibre(t)
	b := &bridge{argv: []string{exe}, idle: 0,
		env:      append(os.Environ(), envHTTPFalso+"="+addr),
		service:  &serviceSpec{Transport: "http", Port: port},
		sessions: map[string]*session{}}
	if err := b.startProxy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.stopProxyChild)
	return b
}

// pedir manda una petición por el puente y devuelve "<pid> <secreto>".
func pedir(t *testing.T, b *bridge, method string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	b.handle(rec, httptest.NewRequest(method, "/mcp", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s por el proxy: %d %s", method, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Si el servidor MCP HTTP compartido muere, la siguiente petición lo relanza.
// Antes el puente seguía apuntando a su puerto muerto: 502 hasta un /reset.
func TestProxyRelanzaAlServidorMuerto(t *testing.T) {
	var store *guest.MMDSStore
	conMMDS(t, &store)
	b := puenteProxy(t)

	primero := pedir(t, b, http.MethodGet)
	b.pmu.Lock()
	h := b.proxyChild
	b.pmu.Unlock()
	_ = h.cmd.Process.Kill()
	esperar(t, func() bool { return !h.vivo() })

	segundo := pedir(t, b, http.MethodGet)
	if segundo == primero {
		t.Fatalf("contestó el mismo proceso (%q) que se acaba de matar", segundo)
	}
}

// En modo http el hijo recibe los secretos comunes de MMDS, como cada sesión en
// modo stdio; y si cambian (un dorado que despierta con otro MMDS), el primer
// initialize sin sesiones vivas lo relanza con los nuevos.
func TestProxyInyectaLosSecretosDeMMDS(t *testing.T) {
	store := &guest.MMDSStore{Env: map[string]string{"SECRETO_FALSO": "uno"}}
	conMMDS(t, &store)
	b := puenteProxy(t)

	if r := pedir(t, b, http.MethodPost); !strings.HasSuffix(r, " uno") {
		t.Fatalf("el servidor HTTP no ve el secreto de MMDS: %q", r)
	}
	store = &guest.MMDSStore{Env: map[string]string{"SECRETO_FALSO": "dos"}}
	if r := pedir(t, b, http.MethodPost); !strings.HasSuffix(r, " dos") {
		t.Fatalf("tras cambiar MMDS sigue el entorno viejo: %q", r)
	}
}

// Un servidor HTTP que muere al arrancar falla enseguida, no a los 40 s de
// plazo del puerto.
func TestProxyHijoQueMuereAlArrancarFallaYa(t *testing.T) {
	var store *guest.MMDSStore
	conMMDS(t, &store)
	_, port := direccionLibre(t)
	b := &bridge{argv: []string{"false"}, service: &serviceSpec{Transport: "http", Port: port},
		sessions: map[string]*session{}}
	t0 := time.Now()
	if err := b.spawnProxyChild(); err == nil {
		t.Fatal("un hijo que sale sin abrir el puerto no puede dar el arranque por bueno")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("tardó %s en ver que el hijo había muerto", d)
	}
}

// El Chromium compartido que muere se relanza en la siguiente sesión, en vez
// de darlo por vivo y conectar las sesiones nuevas a un puerto muerto.
func TestNavegadorMuertoSeRelanza(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := direccionLibre(t)
	b := &bridge{sessions: map[string]*session{},
		env:     append(os.Environ(), envHTTPFalso+"="+addr),
		browser: &browserSpec{Sidecar: []string{exe}, ReadyURL: "http://" + addr + "/"}}
	t.Cleanup(b.stopBrowser)

	if err := b.ensureBrowser(); err != nil {
		t.Fatal(err)
	}
	h := b.bproc
	_ = h.cmd.Process.Kill()
	esperar(t, func() bool { return !h.vivo() })

	if err := b.ensureBrowser(); err != nil {
		t.Fatal(err)
	}
	if !b.bproc.vivo() || b.bproc == h {
		t.Fatal("ensureBrowser dio por bueno un navegador muerto")
	}
}
