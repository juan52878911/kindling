package frontal

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Revocar varios puertos a la vez no debe perder ninguna revocación.
func TestRevRevocacionesConcurrentes(t *testing.T) {
	b := montarIngreso(t, nil)
	base := "/v1/sandboxes/" + b.id + "/endpoints"
	const n = 16
	toks := make([]string, n)
	for i := range toks {
		_, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000 + i, TTLSeconds: 600})
		toks[i] = ep.Token
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := peticion(t, b.srv, tokenAlice, "DELETE", fmt.Sprintf("%s/%d", base, 3000+i), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != 204 {
				t.Errorf("DELETE %d: %v %v", 3000+i, resp, err)
				return
			}
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	b.f.mu.Lock()
	lab := b.f.maquinas[b.sb.ID].Labels[LabelRevocados]
	b.f.mu.Unlock()
	rev := revocadosDe(lab, time.Now())
	if len(rev) != n {
		t.Errorf("%d DELETE contestaron 204 pero solo %d puertos quedaron revocados: %q", n, len(rev), lab)
	}
	vivos := 0
	for i, tok := range toks {
		if resp, _ := b.get(t, 3000+i, "/", map[string]string{cabeceraEndpoint: tok}); resp.StatusCode != 401 {
			vivos++
		}
	}
	if vivos > 0 {
		t.Errorf("%d tokens revocados (204) siguen entrando", vivos)
	}
}

// La etiqueta de revocados nunca debe pasar el tope del daemon (api.ValidateLabels).
func TestRevTopeDeLaEtiquetaDeRevocados(t *testing.T) {
	ahora := time.Now()
	lab := ""
	for i := 0; i < 2000; i++ {
		lab = revocadosCon(lab, 20000+i, ahora)
		if err := api.ValidateLabels(map[string]string{LabelPublico: "", LabelRevocados: lab}); err != nil {
			t.Fatalf("la revocación nº %d ya no cabe: %v", i+1, err)
		}
	}
	// Compactada, la marca sigue revocando lo emitido antes, de cualquier puerto.
	antes := ahora.Add(-time.Second).UnixMicro()
	rev := revocadosDe(lab, ahora)
	if !revocado(rev, 31999, antes) || revocado(rev, 31999, ahora.Add(time.Second).UnixMicro()) {
		t.Errorf("revocado() no distingue emitidos antes y después de la marca")
	}
}

// Una conexión abierta con un token de endpoint no sobrevive a revocarlo.
func TestRevWebSocketSobreviveARevocar(t *testing.T) {
	b := montarIngreso(t, nil)
	_, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 600})
	c, err := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n%s: %s\r\n\r\n", b.host(3000), cabeceraEndpoint, ep.Token)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("apretón: %v %v", resp, err)
	}
	r := pide(t, b.srv, tokenAlice, "DELETE", "/v1/sandboxes/"+b.id+"/endpoints/3000", nil)
	esperaCodigo(t, r, 204)
	r.Body.Close()
	time.Sleep(200 * time.Millisecond)
	fmt.Fprint(c, "hola")
	buf := make([]byte, len("eco:hola"))
	if _, err := io.ReadFull(br, buf); err == nil && string(buf) == "eco:hola" {
		t.Fatalf("el WebSocket abierto con el token revocado sigue pasando tráfico")
	}
}
