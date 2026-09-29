//go:build darwin

package footprint

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// El protocolo del freno sin VM: acepta la tubería, no para nada si ningún
// auxiliar de Apple la tiene (nunca señaliza a otro proceso) y termina al
// cerrarse el socket.
func TestFrenoSinAuxiliar(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	fin := make(chan int, 1)
	go func() { fin <- ServeFreno(fds[1]) }()
	mio := os.NewFile(uintptr(fds[0]), "freno")
	c, err := net.FileConn(mio)
	mio.Close()
	if err != nil {
		t.Fatal(err)
	}
	f := &Freno{c: c.(*net.UnixConn)}

	if err := f.Freeze(true); err == nil {
		t.Fatal("paró algo sin saber qué VM es")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := f.Track(w.Fd()); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := f.Freeze(true); err == nil {
		t.Fatal("paró algo aunque ningún auxiliar tiene la tubería")
	}
	if err := f.Freeze(false); err != nil {
		t.Fatalf("soltar sin nada parado: %v", err)
	}
	// El freno no se queda con la tubería: cerrar el extremo de escritura da
	// fin de fichero al que lee.
	w.Close()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := r.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("la tubería sigue abierta en el freno: n=%d err=%v", n, err)
	}

	f.c.Close()
	select {
	case code := <-fin:
		if code != 0 {
			t.Fatalf("salida %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("el freno no terminó al cerrarse el socket")
	}
}
