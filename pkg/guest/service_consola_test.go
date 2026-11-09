package guest

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// consolaFalsa es la consola (el UART) de un test: se escribe desde el bucle
// del servicio y se lee desde el test.
type consolaFalsa struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *consolaFalsa) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *consolaFalsa) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// A la consola solo va el principio de la salida de cada arranque del
// servicio; el resto, solo al log (`kling logs -service`). Cada byte por el
// UART emulado es una salida al VMM, y un servicio que habla mucho se comía
// la CPU del host por duplicado. Los avisos del agente sí llegan siempre.
func TestServiceConsolaAcotada(t *testing.T) {
	s := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "head -c 300000 /dev/zero | tr '\\0' Z; exit 3"}})
	con := &consolaFalsa{}
	s.console = con
	s.Start([]string{"PATH=/usr/bin:/bin"})
	waitUntil(t, "a restart", func() bool { return s.Status(0).Starts >= 2 })
	s.Stop()
	starts := s.Status(0).Starts

	log, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "Z"); n < 300000 { // el primer arranque entero; el segundo se para a medias
		t.Fatalf("the log has %d bytes of output, want all of it", n)
	}
	c := con.String()
	if n := strings.Count(c, "Z"); n < serviceConsoleMax || n > starts*serviceConsoleMax {
		t.Fatalf("console got %d bytes of output over %d starts, want %d per start", n, starts, serviceConsoleMax)
	}
	if !strings.Contains(c, "kling logs -service") || !strings.Contains(c, "restarting in") {
		t.Fatalf("console without the notices: %q", c[max(0, len(c)-400):])
	}
}
