package publicar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// responde espera hasta plazo a que 127.0.0.1:h conteste por el eco.
func responde(t *testing.T, h int, plazo time.Duration) bool {
	t.Helper()
	limite := time.Now().Add(plazo)
	for {
		if got, err := pedir(t, "127.0.0.1:"+strconv.Itoa(h), "x"); err == nil && got == "eco:x" {
			return true
		}
		if time.Now().After(limite) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Tras reiniciar, un puerto del host que otro proceso tiene un momento no se
// lleva por delante los demás de la máquina ni se borra de publish.json: se
// reabre cuando se libera. Antes era todo o nada y el fichero se reescribía
// con lo que quedó, así que la publicación se perdía para siempre.
func TestReinicioConUnPuertoOcupado(t *testing.T) {
	srv := eco(t)
	fw := map[string]string{"80": srv.Addr().String(), "81": srv.Addr().String()}
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80,81"}},
	}}
	ruta := filepath.Join(t.TempDir(), "publish.json")
	p1 := New(f, ruta, t.Logf)
	t.Cleanup(p1.Esperar)
	ctx1, cancel1 := context.WithCancel(context.Background())
	p1.Arrancar(ctx1, nil)
	ma, _ := f.Get("a")
	h1, h2 := puertoLibre(t), puertoLibre(t)
	if _, err := p1.Publicar(ma, []api.PortPublish{{Host: h1, Guest: 80}, {Host: h2, Guest: 81}}); err != nil {
		t.Fatal(err)
	}
	p1.Esperar()
	p1.guardar()
	cancel1()
	limite := time.Now().Add(2 * time.Second)
	for {
		if _, err := pedir(t, "127.0.0.1:"+strconv.Itoa(h2), "x"); err != nil {
			break
		}
		if time.Now().After(limite) {
			t.Fatal("el puerto sigue abierto con el publicador parado")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Otro proceso toma h2 mientras el daemon reinicia.
	otro, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(h2))
	if err != nil {
		t.Fatal(err)
	}
	p2 := New(f, ruta, t.Logf)
	p2.barrido = 20 * time.Millisecond
	t.Cleanup(p2.Esperar)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	p2.Arrancar(ctx2, nil)
	if !responde(t, h1, time.Second) {
		otro.Close()
		t.Fatal("h1 no se publicó otra vez: el puerto ocupado se llevó los demás")
	}
	p2.Esperar()
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fmt.Sprintf(`"host": %d`, h2)) {
		otro.Close()
		t.Fatalf("publish.json perdió h2:\n%s", b)
	}
	otro.Close()
	if !responde(t, h2, 3*time.Second) {
		t.Fatal("h2 no se reabrió al liberarse")
	}
	if l := p2.Lista(); len(l) != 1 || len(l[0].Ports) != 2 {
		t.Fatalf("Lista = %+v", l)
	}
}

// Si al fallar Publicar el puerto que se sustituía no se puede reabrir, la
// máquina no queda publicada con cero puertos y el puerto se reintenta en el
// barrido, en vez de quedar en publish.json sin escuchar nunca más.
func TestPublicarFallidoNoPierdeElSustituido(t *testing.T) {
	srv := eco(t)
	fw := map[string]string{"80": srv.Addr().String(), "81": srv.Addr().String()}
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80,81"}},
	}}
	p := New(f, filepath.Join(t.TempDir(), "publish.json"), t.Logf)
	p.barrido = 20 * time.Millisecond
	t.Cleanup(p.Esperar)
	var negar atomic.Int64 // puerto del host que escuchar se niega a abrir
	p.escuchar = func(a string) (net.Listener, error) {
		if _, puerto, _ := net.SplitHostPort(a); puerto == strconv.FormatInt(negar.Load(), 10) {
			return nil, errors.New("ocupado en el test")
		}
		return net.Listen("tcp", a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	ma, _ := f.Get("a")
	h1 := puertoLibre(t)
	if _, err := p.Publicar(ma, []api.PortPublish{{Host: h1, Guest: 80}}); err != nil {
		t.Fatal(err)
	}
	// Se sustituye h1 y se pide otro puerto que no se puede abrir: h1 se
	// cierra, falla, y al reabrirlo también falla.
	otro, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer otro.Close()
	negar.Store(int64(h1))
	if _, err := p.Publicar(ma, []api.PortPublish{{Host: h1, Guest: 80}, {Host: otro.Addr().(*net.TCPAddr).Port, Guest: 81}}); !errors.Is(err, ErrOcupado) {
		t.Fatalf("err = %v", err)
	}
	p.mu.Lock()
	mq, esta := p.por["a"]
	vacia := esta && len(mq.puertos) == 0
	p.mu.Unlock()
	if vacia {
		t.Fatal("la máquina quedó publicada con cero puertos")
	}
	negar.Store(0)
	if !responde(t, h1, 3*time.Second) {
		t.Fatal("h1 no se reabrió en el barrido")
	}
}
