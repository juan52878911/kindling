package guest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func execLimOcupadas() int {
	execLim.mu.Lock()
	defer execLim.mu.Unlock()
	return execLim.n
}

func esperarOcupadas(t *testing.T, want int, plazo time.Duration) {
	t.Helper()
	fin := time.Now().Add(plazo)
	for execLimOcupadas() != want {
		if time.Now().After(fin) {
			t.Fatalf("plazas de exec ocupadas: %d, quería %d", execLimOcupadas(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Un exec en curso al volcar el dorado despierta en cada copia sin nadie al
// otro lado: su petición murió con el host que la hizo. El /resync de una
// copia (instance) lo mata y libera su plaza; si no, 64 así dejaban todas las
// copias del dorado contestando 429.
func TestResyncCopiaMataExecsDelDorado(t *testing.T) {
	falsos(t, nil, nil)
	esperarOcupadas(t, 0, 5*time.Second)

	hechos := make(chan struct{}, 2)
	// Las peticiones no se cortan nunca: como en la copia, donde la conexión
	// quedó congelada y nadie la cierra.
	go func() {
		b, _ := json.Marshal(execRequest{Cmd: []string{"sleep", "100"}})
		handleExec(nil, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader(b)))
		hechos <- struct{}{}
	}()
	go func() {
		b, _ := json.Marshal(api.ExecRequest{Cmd: []string{"sleep", "100"}})
		handleStreamExec(nil, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/exec/stream", bytes.NewReader(b)))
		hechos <- struct{}{}
	}()
	esperarOcupadas(t, 2, 5*time.Second)

	// Reanudar la misma máquina (o arrancarla) no corta nada: quien pidió el
	// exec sigue ahí.
	for _, kind := range []string{ResyncResume, ResyncBoot} {
		rr := postResync(t, cuerpoKind(t, time.Now().UnixNano(), kind))
		if rr.Code != http.StatusOK {
			t.Fatalf("resync %s: %d %s", kind, rr.Code, rr.Body)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := execLimOcupadas(); n != 2 {
		t.Fatalf("tras resume/boot quedan %d plazas ocupadas, quería 2", n)
	}

	rr := postResync(t, cuerpoKind(t, time.Now().UnixNano(), api.ResyncInstance))
	if rr.Code != http.StatusOK {
		t.Fatalf("resync instance: %d %s", rr.Code, rr.Body)
	}
	esperarOcupadas(t, 0, 5*time.Second)
	for range 2 {
		select {
		case <-hechos:
		case <-time.After(5 * time.Second):
			t.Fatal("un exec no acabó tras el resync de la copia")
		}
	}
}

// escrituraColgada es la conexión congelada de una copia por vsock: lo que se
// escribe en ella no vuelve hasta que se suelta.
type escrituraColgada struct {
	h     http.Header
	libre chan struct{}
}

func (e *escrituraColgada) Header() http.Header { return e.h }
func (e *escrituraColgada) WriteHeader(int)     {}
func (e *escrituraColgada) Write(p []byte) (int, error) {
	<-e.libre
	return len(p), nil
}

// Aunque el manejador se quede colgado escribiendo la respuesta en la
// conexión congelada, el /resync de una copia suelta las plazas.
func TestResyncCopiaSueltaPlazasConEscrituraColgada(t *testing.T) {
	falsos(t, nil, nil)
	esperarOcupadas(t, 0, 5*time.Second)

	nueva := func() *escrituraColgada {
		return &escrituraColgada{h: http.Header{}, libre: make(chan struct{})}
	}
	w1, w2 := nueva(), nueva()
	hechos := make(chan struct{}, 2)
	t.Cleanup(func() {
		close(w1.libre)
		close(w2.libre)
		for range 2 {
			<-hechos
		}
	})
	go func() {
		b, _ := json.Marshal(execRequest{Cmd: []string{"sleep", "100"}})
		handleExec(nil, w1, httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader(b)))
		hechos <- struct{}{}
	}()
	go func() {
		b, _ := json.Marshal(api.ExecRequest{Cmd: []string{"sleep", "100"}})
		handleStreamExec(nil, w2, httptest.NewRequest(http.MethodPost, "/exec/stream", bytes.NewReader(b)))
		hechos <- struct{}{}
	}()
	esperarOcupadas(t, 2, 5*time.Second)

	rr := postResync(t, cuerpoKind(t, time.Now().UnixNano(), api.ResyncInstance))
	if rr.Code != http.StatusOK {
		t.Fatalf("resync instance: %d %s", rr.Code, rr.Body)
	}
	esperarOcupadas(t, 0, 5*time.Second)
}
