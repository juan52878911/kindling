package daemon

// Tests de N8/A4: plazo de progreso con el invitado (D-01), versión de
// firecracker cacheada (D-02), MaxBytesReader en handlers JSON (D-05) y el
// backoff corto del sondeo de puerto (A4). Ninguno necesita KVM ni root.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// falsoCuerpo es un io.ReadCloser de mentira: su Read no vuelve hasta que se
// lo cierra (como un net.Conn real al que se le corta la conexión), para
// probar progressBody sin abrir ninguna conexión de verdad ni dejar
// goroutines colgadas cuando el test acaba.
type falsoCuerpo struct {
	cerrar  chan struct{}
	cerrado bool
}

func nuevoFalsoCuerpo() *falsoCuerpo { return &falsoCuerpo{cerrar: make(chan struct{})} }

func (c *falsoCuerpo) Read(p []byte) (int, error) {
	<-c.cerrar
	return 0, io.ErrClosedPipe
}

func (c *falsoCuerpo) Close() error {
	c.cerrado = true
	select {
	case <-c.cerrar:
	default:
		close(c.cerrar)
	}
	return nil
}

// TestProgressBodyCortaSiNoHayDatos es D-01: un Read que no vuelve en
// guestProgressTimeout debe fallar y cerrar el cuerpo de abajo, en vez de
// dejar la lectura colgada para siempre.
func TestProgressBodyCortaSiNoHayDatos(t *testing.T) {
	viejo := guestProgressTimeout
	guestProgressTimeout = 30 * time.Millisecond
	t.Cleanup(func() { guestProgressTimeout = viejo })

	fc := nuevoFalsoCuerpo()
	pb := wrapGuestBody(fc)
	t.Cleanup(func() { pb.Close() })

	inicio := time.Now()
	_, err := pb.Read(make([]byte, 4))
	if err == nil {
		t.Fatal("un cuerpo que nunca manda nada debía fallar, no darse por completo")
	}
	if d := time.Since(inicio); d > time.Second {
		t.Fatalf("Read tardó %s en cortar; quería ~guestProgressTimeout (%s)", d, guestProgressTimeout)
	}
	if !fc.cerrado {
		t.Fatal("al expirar el plazo debía cerrarse el cuerpo real, para no dejar la goroutine de Read huérfana")
	}
}

// TestGuestClientCortaSiElInvitadoCalla es la prueba de extremo a extremo de
// D-01: un invitado que manda las cabeceras y luego no vuelve a escribir nada
// no debe poder tener el cuerpo de la respuesta abierto para siempre, una vez
// que quien lee lo hace a través de wrapGuestBody (como los tres sitios
// reales: proxyGuest, openGuestExec y handleFiles).
func TestGuestClientCortaSiElInvitadoCalla(t *testing.T) {
	viejo := guestProgressTimeout
	guestProgressTimeout = 50 * time.Millisecond
	t.Cleanup(func() { guestProgressTimeout = viejo })

	calla := make(chan struct{})
	// defer y no t.Cleanup: tiene que cerrarse ANTES de que se cierre srv (más
	// abajo), o el handler seguiría bloqueado en <-calla y srv.Close() (que
	// espera a que termine) se colgaría para siempre. Los defer de la propia
	// función corren antes que los t.Cleanup registrados, así que el orden
	// queda garantizado.
	defer close(calla)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-calla // nunca vuelve a escribir: el invitado hostil de D-01
	}))
	t.Cleanup(srv.Close)

	resp, err := guestClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body := wrapGuestBody(resp.Body)
	defer body.Close()

	leido := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(body)
		leido <- err
	}()
	select {
	case err := <-leido:
		if err == nil {
			t.Fatal("un invitado que se queda callado a mitad de respuesta debía cortar la lectura, no darla por completa")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("D-01: la lectura del cuerpo no expiró tras guestProgressTimeout; el invitado quedó colgando la conexión")
	}
}

// TestWaitPortBackoffCortoNoticiaRapido es A4: con el sondeo fijo de 200 ms
// de antes, reabrir el puerto a los 40 ms podía tardar hasta 200 ms en
// notarse. Con el backoff de 5-50 ms debe notarse mucho antes.
func TestWaitPortBackoffCortoNoticiaRapido(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // libera el puerto: de momento nadie escucha ahí

	listo := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		ln2, err := net.Listen("tcp", addr)
		if err != nil {
			// El puerto no quedó libre (otro proceso lo tomó entretanto); el
			// test falla más abajo por timeout, con un mensaje claro.
			return
		}
		defer ln2.Close()
		close(listo)
		conn, err := ln2.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	inicio := time.Now()
	if err := waitPort(context.Background(), addr, 500*time.Millisecond); err != nil {
		t.Fatalf("waitPort no encontró el puerto reabierto a los 40 ms: %v", err)
	}
	if d := time.Since(inicio); d > 150*time.Millisecond {
		t.Fatalf("waitPort tardó %s en notar el puerto reabierto a los 40 ms; "+
			"con el backoff corto (5-50 ms) debía notarlo bastante antes de 150 ms", d)
	}
	select {
	case <-listo:
	default:
		t.Fatal("el puerto de prueba nunca llegó a reabrirse; test no concluyente")
	}
}

// TestFirecrackerVersionCached es D-02: /info no debe volver a ejecutar el
// binario una vez que el daemon ya calculó su versión al arrancar.
func TestFirecrackerVersionCached(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "firecracker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Firecracker v1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := firecrackerVersion(bin); got != "Firecracker v1.2.3" {
		t.Fatalf("firecrackerVersion = %q", got)
	}

	root := t.TempDir()
	mgr := nuevoManager(t, root)
	s := &Server{mgr: mgr, root: root, fcVersion: firecrackerVersion(bin)}

	// Sin el binario, GET /info todavía debe contestar la versión: si la
	// recalculara, el exec fallaría y el campo saldría vacío.
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/info", nil))
	var info api.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatalf("decodificar /info: %v", err)
	}
	if info.Firecrack != "Firecracker v1.2.3" {
		t.Fatalf("D-02: /info debía servir la versión cacheada aunque el binario ya no exista: %q", info.Firecrack)
	}
}

// TestHandleRunCuerpoDemasiadoGrande es D-05: un handler JSON sin tope de
// tamaño dejaba pasar cualquier cuerpo por el socket.
func TestHandleRunCuerpoDemasiadoGrande(t *testing.T) {
	root := t.TempDir()
	mgr := nuevoManager(t, root)
	s := &Server{mgr: mgr, root: root}

	cuerpo := `{"name":"` + strings.Repeat("a", jsonMaxBody+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/machines", strings.NewReader(cuerpo))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /machines con %d bytes = %d, quería %d: %s", len(cuerpo), rr.Code, http.StatusRequestEntityTooLarge, rr.Body)
	}
}

// TestHandleRunCuerpoInvalidoSigueSiendo400 comprueba que decodeJSON no
// convierte cualquier fallo de decodificación en 413: solo el de tamaño.
func TestHandleRunCuerpoInvalidoSigueSiendo400(t *testing.T) {
	root := t.TempDir()
	mgr := nuevoManager(t, root)
	s := &Server{mgr: mgr, root: root}

	req := httptest.NewRequest(http.MethodPost, "/machines", strings.NewReader("esto no es json"))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /machines con JSON inválido = %d, quería 400: %s", rr.Code, rr.Body)
	}
}
