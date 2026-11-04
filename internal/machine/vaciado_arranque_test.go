package machine

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un freeze justo detrás de `kling run`: el agente aún no escucha cuando se
// le pide que vacíe. Se le espera y se repite, en vez de avisar de que "el
// invitado no vació sus volúmenes antes de morir".
func TestVaciadoEsperaAlAgenteQueArranca(t *testing.T) {
	m := newTestManager(t)
	img := "conagente"
	ruta := m.imagePath(img)
	if err := os.MkdirAll(filepath.Dir(ruta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, []byte("ext4"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.bridgeOK.Store(bridgeFingerprint(ruta)+"\x00"+bridgeFingerprint(""), true)

	// Un puerto que aún no escucha nadie, y el agente que llega a los 300 ms.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	var vaciados atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/volume/sync" {
			vaciados.Add(1)
		}
		_, _ = w.Write([]byte("ok"))
	})}
	listo := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		listo <- err
		if err == nil {
			_ = srv.Serve(l)
		}
	}()
	defer srv.Close()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	ahora := time.Now()
	mc := &api.Machine{ID: newID(), Name: "recien", Image: img, State: api.StateRunning, StartedAt: &ahora,
		Forwards: map[string]string{strconv.Itoa(api.GuestPort): addr}}
	m.flushVolume(mc, true)
	if err := <-listo; err != nil {
		t.Skipf("could not listen again on %s: %v", addr, err)
	}
	if vaciados.Load() != 1 {
		t.Fatalf("%d flushes, want 1 (log: %s)", vaciados.Load(), buf.String())
	}
	if strings.Contains(buf.String(), "did not flush") {
		t.Fatalf("warned: %s", buf.String())
	}
}

// Una máquina cuyo agente ya contestó alguna vez no se espera: si no vacía,
// se avisa (y en el acto).
func TestVaciadoNoEsperaAUnAgenteConocido(t *testing.T) {
	m := newTestManager(t)
	ahora := time.Now()
	mc := m.addForTest(newID())
	m.mu.Lock()
	mc.State, mc.StartedAt = api.StateRunning, &ahora
	mc.Agent = &api.GuestAgent{Caps: []string{}}
	mc.Forwards = map[string]string{strconv.Itoa(api.GuestPort): "127.0.0.1:1"}
	copia := mc.Clone()
	m.mu.Unlock()
	t0 := time.Now()
	m.flushVolume(copia, true)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("waited %s for a known agent", d)
	}
}
