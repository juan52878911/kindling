//go:build darwin

package peercred

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func escucharUnix(t *testing.T) net.Listener {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestUIDDelPropioProceso(t *testing.T) {
	l := escucharUnix(t)
	go func() {
		c, err := net.Dial("unix", l.Addr().String())
		if err == nil {
			defer c.Close()
			time.Sleep(200 * time.Millisecond)
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := UID(c)
	if err != nil {
		t.Fatal(err)
	}
	if uid != os.Geteuid() {
		t.Fatalf("UID = %d, want %d", uid, os.Geteuid())
	}
}

// Con el usuario real (el mismo), SameUser deja pasar; con otro uid o sin
// poder identificarlo, cierra la conexión sin entregarla y sigue aceptando.
func TestSameUserRechazaOtrosUsuarios(t *testing.T) {
	l := escucharUnix(t)
	var avisos atomic.Int32
	var ultimo atomic.Value
	su := SameUser(l, func(f string, a ...any) {
		avisos.Add(1)
		ultimo.Store(f)
	}).(*sameUser)
	var n atomic.Int32
	su.peerUID = func(c net.Conn) (int, error) {
		switch n.Add(1) {
		case 1:
			return su.uid + 1, nil
		case 2:
			return -1, errors.New("sin credenciales")
		}
		return UID(c)
	}
	aceptadas := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := su.Accept()
			if err != nil {
				close(aceptadas)
				return
			}
			aceptadas <- c
		}
	}()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("unix", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("conexión %d rechazada: Read = %v, want EOF", i+1, err)
		}
		c.Close()
	}
	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case a := <-aceptadas:
		a.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("la conexión del mismo usuario no se aceptó")
	}
	select {
	case a, ok := <-aceptadas:
		if ok {
			a.Close()
			t.Fatal("se entregó una conexión rechazada")
		}
	default:
	}
	su.mu.Lock()
	r := su.rechazos
	su.mu.Unlock()
	if r != 2 {
		t.Fatalf("rechazos = %d, want 2", r)
	}
	// Un aviso por minuto como mucho.
	if avisos.Load() != 1 || !strings.Contains(ultimo.Load().(string), "uid %d") {
		t.Fatalf("avisos = %d (%v), want 1 con el uid", avisos.Load(), ultimo.Load())
	}
}
