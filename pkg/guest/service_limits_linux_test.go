package guest

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El servicio hereda el nofile del spec: Go restauraría el original en el
// hijo si el agente no llamara a Setrlimit.
func TestServiceInheritsUlimit(t *testing.T) {
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &cur); err != nil || cur.Max < 4099 {
		t.Skip("no room to set nofile=4099 in this process")
	}
	defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &cur) // subir el blando de vuelta
	resetServiceState()
	s := newTestService(t, api.ServiceSpec{
		Argv:    []string{"sh", "-c", "ulimit -n; sleep 5"},
		Ulimits: []string{"nofile=4099:4099"},
		Restart: api.RestartNo,
	})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	defer s.Stop()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if strings.HasPrefix(s.Status(4096).Log, "4099") {
			return
		}
	}
	t.Fatalf("the service didn't see nofile=4099: %q", s.Status(4096).Log)
}
