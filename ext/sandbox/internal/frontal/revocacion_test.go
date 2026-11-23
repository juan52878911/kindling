package frontal

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
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

// R4-5: una petición cuya resolución del sandbox se queda retenida mientras se
// revoca el puerto no puede entrar con las etiquetas de antes, ni sembrar la
// caché con ellas.
func TestRevPeticionEnVueloDuranteLaRevocacion(t *testing.T) {
	b := montarIngreso(t, nil)
	_, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 600})
	// Retiene el siguiente listado (el de la petición) con lo que hay ahora.
	b.f.mu.Lock()
	b.f.sandboxesEspera, b.f.sandboxesEmpezado = make(chan struct{}), make(chan struct{})
	b.f.sandboxesFoto = true
	b.f.sandboxesColgarN = b.f.sandboxesLlamadas + 1
	b.f.mu.Unlock()
	cod := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", b.srv.URL+"/", nil)
		req.Host = b.host(3000)
		req.Header.Set(cabeceraEndpoint, ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cod <- -1
			return
		}
		resp.Body.Close()
		cod <- resp.StatusCode
	}()
	select {
	case <-b.f.sandboxesEmpezado:
	case <-time.After(5 * time.Second):
		t.Fatal("la petición no llegó a listar el host")
	}
	r := pide(t, b.srv, tokenAlice, "DELETE", "/v1/sandboxes/"+b.id+"/endpoints/3000", nil)
	esperaCodigo(t, r, 204)
	r.Body.Close()
	close(b.f.sandboxesEspera)
	select {
	case c := <-cod:
		if c != 401 {
			t.Errorf("la petición en vuelo durante la revocación contestó %d, quería 401", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la petición no terminó")
	}
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: ep.Token}); resp.StatusCode != 401 {
		t.Errorf("tras el 204, el token revocado entra con %d (caché sembrada con lo de antes)", resp.StatusCode)
	}
}

// segundaReplica monta otro Servidor sobre el mismo daemon falso, como un
// gateway con replicas > 1: mismos tenants y secreto, nada de estado común.
func segundaReplica(t *testing.T, b *bancoIngreso) *httptest.Server {
	t.Helper()
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": b.f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}},
		Log:     testLog(t),
		Ingreso: IngresoOpciones{Dominio: dominioTest, Esquema: "http", Secreto: []byte("secreto-de-prueba-123")},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// R4-6: dos réplicas revocando a la vez no se pisan las marcas.
func TestRevDosReplicasNoPierdenRevocaciones(t *testing.T) {
	b := montarIngreso(t, nil)
	b.f.mu.Lock()
	b.f.etiquetasDemora = 15 * time.Millisecond
	b.f.mu.Unlock()
	otra := segundaReplica(t, b)
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
			srv := b.srv
			if i%2 == 1 {
				srv = otra
			}
			req := peticion(t, srv, tokenAlice, "DELETE", fmt.Sprintf("/v1/sandboxes/%s/endpoints/%d", b.id, 3000+i), nil)
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
	if rev := revocadosDe(lab, time.Now()); len(rev) != n {
		t.Errorf("%d DELETE dieron 204 entre dos réplicas pero solo %d puertos quedaron revocados: %q", n, len(rev), lab)
	}
}

// R4-6: una conexión abierta en una réplica se corta, como mucho a los 5 s,
// cuando la revocación se hizo en la otra.
func TestRevConexionEnOtraReplicaSeCorta(t *testing.T) {
	b := montarIngreso(t, nil)
	otra := segundaReplica(t, b)
	_, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 600})
	c, err := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(12 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n%s: %s\r\n\r\n", b.host(3000), cabeceraEndpoint, ep.Token)
	br := bufio.NewReader(c)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 101 {
		t.Fatalf("apretón: %v %v", resp, err)
	}
	r := pide(t, otra, tokenAlice, "DELETE", "/v1/sandboxes/"+b.id+"/endpoints/3000", nil)
	esperaCodigo(t, r, 204)
	r.Body.Close()
	ini := time.Now()
	// El servidor cierra la conexión: la lectura termina con EOF/reset.
	_, err = br.ReadByte()
	if err == nil {
		t.Fatalf("llegaron datos en vez del corte")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("la conexión revocada en la otra réplica sigue abierta tras %s", time.Since(ini))
	}
	// 5 s del repaso más el margen de una máquina cargada (CI, -race).
	if d := time.Since(ini); d > 7*time.Second {
		t.Errorf("el corte tardó %s, el tope es 5 s", d)
	}
}
