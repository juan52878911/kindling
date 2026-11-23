package hosts

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// daemonColgado acepta conexiones y no contesta a /info hasta que acaba el
// test: es el host remoto con la red partida o el daemon bloqueado. Cuenta
// cuántas sondas le llegan.
func daemonColgado(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	fin := make(chan struct{})
	var n atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-fin:
		case <-r.Context().Done():
		}
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix aquí: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() { close(fin); srv.Close() })
	return "unix://" + sock, &n
}

// conocido deja en la caché del registro un estado ya caducado, como si la
// última lectura fuera de hace más de TTLEstado.
func conocido(r *Registro, e Estado) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.estado[e.Nombre] = e
	r.visto[e.Nombre] = time.Now().Add(-2 * TTLEstado)
}

// Con un host que no contesta ya conocido como caído, una creación no tiene
// que esperar a su sonda cuando caduca la caché: antes cada petición que
// llegaba con la lectura caducada sondeaba en línea y esperaba el plazo.
func TestCandidatosNoEsperaAlHostColgado(t *testing.T) {
	colgado, _ := daemonColgado(t)
	reg := Nuevo(map[string]string{"sano": daemonFalso(t, 1024, true), "colgado": colgado})
	conocido(reg, Estado{Nombre: "sano", Vivo: true, DisponibleMB: 1024})
	conocido(reg, Estado{Nombre: "colgado", Error: "timeout"})

	empiezo := time.Now()
	cands := reg.Candidatos(context.Background(), nil)
	if d := time.Since(empiezo); d > time.Second {
		t.Errorf("Candidatos tardó %v con un host colgado ya conocido", d)
	}
	if len(cands) != 1 || cands[0].Nombre != "sano" {
		t.Errorf("candidatos = %v, quería solo el sano", cands)
	}
}

// Las peticiones que llegan mientras hay una sonda en curso a un host no
// lanzan otra cada una.
func TestCandidatosSondaUnicaPorHost(t *testing.T) {
	colgado, sondas := daemonColgado(t)
	reg := Nuevo(map[string]string{"colgado": colgado})
	conocido(reg, Estado{Nombre: "colgado", Error: "timeout"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.Candidatos(ctx, nil)
		}()
	}
	wg.Wait()
	// Dar tiempo a que lleguen las sondas que se hayan lanzado.
	time.Sleep(200 * time.Millisecond)
	if n := sondas.Load(); n != 1 {
		t.Errorf("%d sondas al mismo host a la vez, quería 1", n)
	}
}

// La primera lectura de varios hosts, que sí espera, los sondea en paralelo:
// dos hosts colgados cuestan un plazo, no dos.
func TestCandidatosPrimeraLecturaEnParalelo(t *testing.T) {
	if testing.Short() {
		t.Skip("espera el plazo de la sonda")
	}
	a, _ := daemonColgado(t)
	b, _ := daemonColgado(t)
	reg := Nuevo(map[string]string{"a": a, "b": b, "sano": daemonFalso(t, 1024, true)})
	empiezo := time.Now()
	cands := reg.Candidatos(context.Background(), nil)
	if d := time.Since(empiezo); d > 7*time.Second {
		t.Errorf("dos hosts colgados costaron %v; en paralelo serían un plazo de sonda", d)
	}
	if len(cands) != 1 {
		t.Errorf("candidatos = %d, quería 1", len(cands))
	}
}
