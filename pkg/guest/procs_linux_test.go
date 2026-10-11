package guest

import (
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Con /proc de verdad: tras reiniciar, restartAndListen no vuelve hasta que el
// proceso nuevo escucha otra vez en su puerto (tarda 1 s en hacerlo).
func TestRestartAndListenConProcReal(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("sin python3")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	srv := "import socket,time\ntime.sleep(1)\ns=socket.socket()\ns.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)\n" +
		"s.bind(('127.0.0.1'," + strconv.Itoa(port) + "))\ns.listen()\nwhile True: s.accept()[0].close()"
	s := newTestService(t, api.ServiceSpec{Argv: []string{"python3", "-c", srv}})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	defer s.Stop()
	waitUntil(t, "the first listen", func() bool { return listeningPorts("/proc", s.Status(0).PID)[port] })

	t0 := time.Now()
	if err := s.restartAndListen("/proc", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < time.Second {
		t.Fatalf("volvió en %s, antes de que el proceso nuevo escuchara", d)
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatalf("tras restartAndListen el puerto no contesta: %v", err)
	}
	c.Close()
}
