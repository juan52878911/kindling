package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// El token se genera 0600, se relee igual y un fichero legible por otros se
// rechaza en vez de usarse.
func TestAITokenFichero(t *testing.T) {
	t.Setenv("KLING_AI_TOKEN", "")
	p := filepath.Join(t.TempDir(), "sub", "ai.token")
	if _, err := aiToken(p, false); err == nil {
		t.Fatal("missing token file without create should fail")
	}
	t1, err := aiToken(p, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, %v", st.Mode().Perm(), err)
	}
	t2, err := aiToken(p, true)
	if err != nil || t2 != t1 {
		t.Fatalf("reread token %q != %q (%v)", t2, t1, err)
	}
	_ = os.Chmod(p, 0o644)
	if _, err := aiToken(p, true); err == nil {
		t.Fatal("world-readable token accepted")
	}
	t.Setenv("KLING_AI_TOKEN", "from-env-0123456789")
	if got, _ := aiToken(p, true); got != "from-env-0123456789" {
		t.Fatalf("env token ignored: %q", got)
	}
}

// El socket nace 0600, no pisa a un gateway vivo y sí retira uno muerto.
func TestAISocket0600(t *testing.T) {
	dir, err := os.MkdirTemp("", "ai")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "ai.sock")
	ln, err := listenUnix0600(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", st.Mode().Perm())
	}
	if _, err := listenUnix0600(p); err == nil {
		t.Fatal("a second gateway took a live socket")
	}
	ln.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := listenUnix0600(p)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
	if _, err := listenUnix0600(filepath.Join(dir, strings.Repeat("a", 120))); err == nil {
		t.Fatal("overlong socket path accepted")
	}
}

// reinicioConPeticiones arranca un servidor con servirHastaApagar, mete n
// peticiones que tardan dur cada una, pide el apagado con todas en vuelo y,
// cuando servirHastaApagar vuelve, cierra el servidor como lo haría la salida
// del proceso. Devuelve cuántas de esas peticiones NO acabaron en 200.
func reinicioConPeticiones(tb testing.TB, n int, dur, drain time.Duration) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	var dentro sync.WaitGroup
	dentro.Add(n)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dentro.Done()
		time.Sleep(dur)
		_, _ = w.Write([]byte("ok"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vuelta := make(chan error, 1)
	go func() { vuelta <- servirHastaApagar(ctx, srv, ln, drain) }()

	cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	var fallos atomic.Int64
	var hechas sync.WaitGroup
	for range n {
		hechas.Add(1)
		go func() {
			defer hechas.Done()
			resp, err := cl.Get("http://" + ln.Addr().String() + "/")
			if err != nil {
				fallos.Add(1)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				fallos.Add(1)
			}
		}()
	}
	dentro.Wait()
	cancel()
	if err := <-vuelta; err != nil {
		tb.Fatal(err)
	}
	_ = srv.Close() // la salida del proceso: lo que quede en vuelo se corta
	hechas.Wait()
	return int(fallos.Load())
}

// BenchmarkReiniciosErrores mide los errores que ve un cliente con peticiones
// en vuelo durante reinicios del servicio (R8-03). Con -benchtime=20x son 20
// reinicios; el objetivo es 0 err/restart.
func BenchmarkReiniciosErrores(b *testing.B) {
	total := 0
	for range b.N {
		total += reinicioConPeticiones(b, 4, 50*time.Millisecond, 30*time.Second)
	}
	b.ReportMetric(float64(total)/float64(b.N), "err/restart")
}
