package transport

import (
	"bytes"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// listenUnix levanta un socket Unix temporal. Solo vive durante el test.
//
// El path del socket tiene que ser corto: macOS rechaza con EINVAL paths de
// más de 104 bytes y el TempDir por defecto vive bajo /var/folders/.../T/, que
// se acerca al límite cuando se añade el nombre del test. Redirigimos TMPDIR
// a /tmp dentro del test para que TempDir genere rutas aptas para socket.
func listenUnix(t *testing.T) (net.Listener, string) {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	dir := t.TempDir()
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("no pude escuchar en %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, sock
}

// gatedReader es un io.Reader que bloquea hasta que se llama a Close. Simula
// un stdin colgado (sin EOF ni datos), que es exactamente el escenario donde
// el bug del ServeStdio original se manifiesta: una de las goroutines de
// io.Copy termina, pero la otra depende de algo que Close() sobre el socket
// no desbloquea, y queda huérfana hasta que el proceso muere.
type gatedReader struct {
	closed chan struct{}
}

func newGatedReader() *gatedReader { return &gatedReader{closed: make(chan struct{})} }

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.closed
	return 0, io.EOF
}

// Close libera la lectura pendiente.
func (g *gatedReader) Close() {
	select {
	case <-g.closed:
	default:
		close(g.closed)
	}
}

// TestServeStdioCopiaDatos es el camino feliz: lo escrito en in aparece al otro
// lado del socket, y lo escrito al socket aparece en out.
func TestServeStdioCopiaDatos(t *testing.T) {
	ln, sock := listenUnix(t)

	clientCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		clientCh <- c
		_, _ = io.Copy(io.Discard, c)
		_, _ = c.Write([]byte("pong"))
		_ = c.Close()
	}()

	stdin := strings.NewReader("ping")
	stdout := &bytes.Buffer{}

	done := make(chan error, 1)
	go func() { done <- ServeStdio(sock, stdin, stdout) }()

	c := <-clientCh
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ServeStdio terminó con: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio no terminó")
	}

	if !strings.Contains(stdout.String(), "pong") {
		t.Fatalf("stdout debería contener pong, contiene %q", stdout.String())
	}
}

// TestServeStdioDrenaSegundaGoroutine es el test específico del leak. El
// escenario:
//
//   - in es un gatedReader: bloquea indefinidamente hasta que el test lo
//     libera. Simula un stdin colgado.
//   - El "daemon" cierra c al instante: el io.Copy(out, c) del puente ve EOF,
//     termina y envía su error al canal done.
//   - En la versión rota de ServeStdio, main lee UN valor de done, retorna y
//     deja la otra goroutine (io.Copy(c, in)) viva leyendo del gatedReader
//     hasta que el proceso muera. ServeStdio retorna "temprano".
//   - En la versión arreglada, main lee DOS valores: tras el primero, queda
//     bloqueado en el segundo hasta que la otra goroutine termine.
//
// El test verifica que ServeStdio NO retorna antes de que ambas goroutines
// internas hayan salido.
func TestServeStdioDrenaSegundaGoroutine(t *testing.T) {
	ln, sock := listenUnix(t)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()

	gated := newGatedReader()
	defer gated.Close() // seguridad: si el test falla antes, no dejamos A viva
	stdout := &bytes.Buffer{}

	done := make(chan error, 1)
	go func() { done <- ServeStdio(sock, gated, stdout) }()

	// Damos tiempo a que ServeStdio arranque las dos goroutines internas y a
	// que B termine (c es cerrado por el daemon).
	time.Sleep(100 * time.Millisecond)

	// ServeStdio NO debe haber retornado: la versión rota lo haría en este
	// punto, dejando A huérfana.
	select {
	case <-done:
		t.Fatal("ServeStdio retornó prematuramente: la segunda goroutine de io.Copy quedó viva")
	default:
	}

	// Liberamos A para que la versión arreglada pueda completar.
	gated.Close()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("error inesperado: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio no retornó tras liberar la segunda goroutine")
	}

	// Tras el drain, las goroutines internas tienen que haber salido.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baselineGoroutines() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("quedan goroutines colgadas tras ServeStdio: ahora=%d baseline=%d",
		runtime.NumGoroutine(), baselineGoroutines())
}

// TestServeStdioErrorRealPropagado: cuando uno de los io.Copy retorna un error
// real (no EOF), debe propagarse. Antes del fix ambos errores se descartaban.
func TestServeStdioErrorRealPropagado(t *testing.T) {
	ln, sock := listenUnix(t)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()

	stdin := strings.NewReader("forzar-error-de-escritura")
	stdout := &bytes.Buffer{}

	done := make(chan error, 1)
	go func() { done <- ServeStdio(sock, stdin, stdout) }()

	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			return
		}
		if !strings.Contains(err.Error(), "broken pipe") &&
			!strings.Contains(err.Error(), "closed") &&
			!strings.Contains(err.Error(), "use of closed") {
			t.Fatalf("error inesperado: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio no terminó")
	}
}

// TestServeStdioSocketInexistente: el error de conexión se devuelve limpio y
// no se lanzan goroutines que tengamos que esperar.
func TestServeStdioSocketInexistente(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "no-existe.sock")
	before := runtime.NumGoroutine()
	err := ServeStdio(sock, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("esperaba error al conectar a socket inexistente")
	}
	time.Sleep(50 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > before+1 {
		// +1 de margen por el runtime.
		t.Fatalf("posible leak tras socket inexistente: antes=%d después=%d",
			before, got)
	}
}

// baselineGoroutines devuelve un número estable de goroutines "del runtime".
// Se usa como suelo para los tests de leak: si Servestdio+drena deja más
// goroutines que esto, hay un leak.
func baselineGoroutines() int {
	return runtime.NumGoroutine()
}
