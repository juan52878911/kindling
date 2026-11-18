package daemon

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// R8: el socket del daemon no tenía tope de conexiones. Ahora hay uno global
// y, con política, otro por uid que no es admin; la conexión de más recibe un
// 429 y se cierra, y cerrar una deja sitio.
func TestConexionesConTope(t *testing.T) {
	vg, vu, va := maxConexiones, maxConexionesUID, plazasAdmin
	maxConexiones, maxConexionesUID, plazasAdmin = 3, 2, 0
	t.Cleanup(func() { maxConexiones, maxConexionesUID, plazasAdmin = vg, vu, va })

	dir, err := os.MkdirTemp("", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := servidorAuthz(t, politicaDePrueba(t))
	var uid atomic.Int32
	s.identificar = func(net.Conn) (Llamante, error) {
		u := int(uid.Load())
		return Llamante{UID: u, GID: u, Conocido: true}, nil
	}
	tope := s.escuchaConTope(ln)
	t.Cleanup(func() { tope.Close() })
	aceptadas := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := tope.Accept()
			if err != nil {
				return
			}
			aceptadas <- c
		}
	}()

	// marcar abre una conexión como u y dice si se aceptó o se rechazó.
	marcar := func(u int) (net.Conn, net.Conn, bool) {
		t.Helper()
		uid.Store(int32(u))
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		select {
		case srv := <-aceptadas:
			return c, srv, true
		case <-time.After(300 * time.Millisecond):
		}
		// Rechazada: lo que llega es un 429 y el cierre.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("la conexión rechazada no trae respuesta: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "too_many_requests") {
			t.Fatalf("rechazo: %d %s", resp.StatusCode, b)
		}
		return c, nil, false
	}

	_, a1, ok1 := marcar(uidA)
	_, _, ok2 := marcar(uidA)
	if !ok1 || !ok2 {
		t.Fatal("dentro de su parte, el inquilino tiene que entrar")
	}
	if _, _, ok := marcar(uidA); ok {
		t.Fatal("el inquilino pasó de su parte")
	}
	// El admin no tiene parte por uid, pero sí el tope global (3).
	if _, _, ok := marcar(uidAdmin); !ok {
		t.Fatal("el admin no entra con sitio global")
	}
	if _, _, ok := marcar(uidAdmin); ok {
		t.Fatal("se pasó del tope global")
	}
	// Cerrar una devuelve su hueco (una vez, aunque se cierre dos).
	a1.Close()
	a1.Close()
	if _, _, ok := marcar(uidA); !ok {
		t.Fatal("tras cerrar una, el inquilino no vuelve a entrar")
	}
	tope.mu.Lock()
	total, deA := tope.total, tope.porUID[uidA]
	tope.mu.Unlock()
	if total != 3 || deA != 2 {
		t.Fatalf("cuenta: total %d, de a %d; quería 3 y 2", total, deA)
	}
}

// R2 (ronda 5, relacionado): con el tope global lleno de inquilinos, el
// admin tampoco entraba. Las últimas plazasAdmin solo las toman los admin.
func TestConexionesPlazasDelAdmin(t *testing.T) {
	vg, vu, va := maxConexiones, maxConexionesUID, plazasAdmin
	maxConexiones, maxConexionesUID, plazasAdmin = 3, 3, 1
	t.Cleanup(func() { maxConexiones, maxConexionesUID, plazasAdmin = vg, vu, va })
	s := servidorAuthz(t, politicaDePrueba(t))
	l := s.escuchaConTope(nil)
	if !l.tomar(uidA, false) || !l.tomar(uidB, false) {
		t.Fatal("los inquilinos no entran con sitio")
	}
	if l.tomar(uidA, false) {
		t.Fatal("un inquilino tomó la plaza del admin")
	}
	if !l.tomar(sinUID, true) {
		t.Fatal("el admin no entra en su plaza")
	}
	if l.tomar(sinUID, true) {
		t.Fatal("se pasó del tope global")
	}
}

// conContexto usa lo que escuchaConTope ya leyó de la conexión.
func TestConContextoUsaLaIdentificacionDelListener(t *testing.T) {
	s := &Server{identificar: func(net.Conn) (Llamante, error) {
		t.Fatal("volvió a identificar la conexión")
		return Llamante{}, nil
	}}
	c := &conIdentificada{ll: Llamante{UID: 42, Conocido: true}, llOK: true}
	if ll := llamanteDe(s.conContexto(context.Background(), c)); ll.UID != 42 || !ll.Conocido {
		t.Fatalf("llamante: %+v", ll)
	}
}
