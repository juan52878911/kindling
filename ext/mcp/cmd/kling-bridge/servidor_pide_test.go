package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
)

// Un servidor MCP que hace sampling a mitad de un tools/call (ver el servidor
// falso de mcpfalso_test.go). Lo pide con id 1, el mismo que la llamada en
// vuelo: antes el puente lo entregaba como la RESPUESTA del tools/call, y la
// respuesta de verdad se tiraba.

const respuestaSampling = `{"jsonrpc":"2.0","id":1,"result":{"role":"assistant",` +
	`"content":{"type":"text","text":"hola"},"model":"falso"}}`

// puenteHTTP arranca el puente con el servidor falso detrás de un servidor
// HTTP de verdad (un ResponseRecorder no deja leer el flujo mientras se
// escribe) y abre la sesión.
func puenteHTTP(t *testing.T) (*bridge, *httptest.Server, string) {
	t.Helper()
	b := testBridge()
	b.reqTimeout = 5 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(b.handle))
	// Con plazo, flujos incluidos: sin el arreglo un test debe FALLAR, no
	// quedarse leyendo un flujo al que nunca llega nada.
	srv.Client().Timeout = 10 * time.Second
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close(); b.closeAll() })
	resp := enviar(t, srv, "", mcp.AcceptMCP,
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`)
	defer resp.Body.Close()
	sid := resp.Header.Get(SessionHeader)
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d, sesión %q", resp.StatusCode, sid)
	}
	return b, srv, sid
}

func enviar(t *testing.T, srv *httptest.Server, sid, accept, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// siguienteEvento lee el próximo evento SSE con datos y devuelve su JSON.
func siguienteEvento(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("el flujo SSE se cortó antes de tiempo: %v", err)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &m); err != nil {
				t.Fatalf("evento que no es JSON: %q", v)
			}
			return m
		}
	}
}

func textoDelResultado(m map[string]any) string {
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	c, _ := content[0].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

// Con un cliente que acepta SSE, el sampling llega por la respuesta del propio
// tools/call, con el id del servidor; la respuesta del cliente se reenvía al
// servidor y el resultado de la llamada cierra el mismo flujo.
func TestSamplingAMitadDeLlamadaVaPorElFlujoDeLaPeticion(t *testing.T) {
	_, srv, sid := puenteHTTP(t)

	resp := enviar(t, srv, sid, mcp.AcceptMCP,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muestrea"}}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cuerpo, _ := io.ReadAll(resp.Body)
		t.Fatalf("la llamada no abrió un flujo SSE: %d %s %s", resp.StatusCode, ct, cuerpo)
	}
	r := bufio.NewReader(resp.Body)

	pide := siguienteEvento(t, r)
	if pide["method"] != "sampling/createMessage" || pide["id"] != float64(1) {
		t.Fatalf("primer evento: %v; quería la petición de sampling del servidor con id 1", pide)
	}

	// La respuesta del cliente: POST aparte, mismo id, sin method.
	r2 := enviar(t, srv, sid, mcp.AcceptMCP, respuestaSampling)
	r2.Body.Close()
	if r2.StatusCode != http.StatusAccepted {
		t.Fatalf("la respuesta al sampling devolvió %d, quería 202", r2.StatusCode)
	}

	fin := siguienteEvento(t, r)
	if fin["id"] != float64(1) || textoDelResultado(fin) != "muestreado: hola" {
		t.Fatalf("resultado del tools/call: %v; quería «muestreado: hola»", fin)
	}
}

// Un cliente que solo acepta JSON recibe la petición del servidor por el GET
// SSE de la sesión, y la llamada se contesta como JSON normal.
func TestSamplingSinFlujoPOSTVaPorElGET(t *testing.T) {
	_, srv, sid := puenteHTTP(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set(SessionHeader, sid)
	req.Header.Set("Accept", "text/event-stream")
	get, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()

	type resultado struct {
		code int
		body []byte
	}
	hecho := make(chan resultado, 1)
	go func() {
		resp := enviar(t, srv, sid, "application/json",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muestrea"}}`)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		hecho <- resultado{resp.StatusCode, b}
	}()

	pide := siguienteEvento(t, bufio.NewReader(get.Body))
	if pide["method"] != "sampling/createMessage" {
		t.Fatalf("por el GET llegó %v; quería la petición de sampling", pide)
	}
	r2 := enviar(t, srv, sid, "application/json", respuestaSampling)
	r2.Body.Close()

	select {
	case res := <-hecho:
		var m map[string]any
		_ = json.Unmarshal(res.body, &m)
		if res.code != 200 || textoDelResultado(m) != "muestreado: hola" {
			t.Fatalf("tools/call: %d %s; quería «muestreado: hola»", res.code, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el tools/call no terminó")
	}
}

// Si nadie puede recibir la petición del servidor (sin flujo POST y con el
// búfer del GET lleno), el puente le contesta con un error en vez de dejarle
// esperando para siempre.
func TestPeticionDelServidorSinOyenteSeRechaza(t *testing.T) {
	s := newTestSession(t)
	defer s.close()
	for len(s.notes) < cap(s.notes) {
		s.notes <- json.RawMessage(`{}`)
	}
	// `cat` devuelve por stdout lo que el puente escribe en su stdin: el rechazo
	// vuelve como una respuesta con el id de la petición.
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[`"srv-1"`] = ch
	s.mu.Unlock()
	s.deliverServerMessage(json.RawMessage(`{"jsonrpc":"2.0","id":"srv-1","method":"roots/list"}`),
		json.RawMessage(`"srv-1"`))
	select {
	case m := <-ch:
		if !strings.Contains(string(m), `"error"`) {
			t.Fatalf("se esperaba un error JSON-RPC para el servidor, llegó %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("la petición del servidor se descartó sin contestarle")
	}
}

// Un cuerpo JSON con saltos de línea (indentado) es un solo mensaje: por stdio
// tiene que viajar en una sola línea o el servidor recibe trozos que no son JSON.
func TestCuerpoIndentadoLlegaEnUnaLinea(t *testing.T) {
	b, srv, sid := puenteHTTP(t)
	b.mu.Lock()
	b.sessions[sid].reqTimeout = 2 * time.Second
	b.mu.Unlock()

	resp := enviar(t, srv, sid, "application/json",
		"{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 5,\n  \"method\": \"tools/list\"\n}\n")
	defer resp.Body.Close()
	cuerpo, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(cuerpo), `"eco":"tools/list"`) {
		t.Fatalf("JSON indentado: %d %s", resp.StatusCode, cuerpo)
	}
}

// Una sesión cuyo servidor MCP murió es una sesión perdida: 404 para que el
// cliente rehaga el initialize, y fuera de la tabla. Antes se devolvía la sesión
// cerrada (502 en cada petición) y se le refrescaba lastUse, así que ni el
// cosechador la quitaba.
func TestSesionConServidorMuertoDa404(t *testing.T) {
	b, srv, sid := puenteHTTP(t)
	b.mu.Lock()
	s := b.sessions[sid]
	b.mu.Unlock()
	_ = s.cmd.Process.Kill()
	esperar(t, s.isClosed)

	resp := enviar(t, srv, sid, mcp.AcceptMCP, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sesión con el servidor muerto: %d; quería 404", resp.StatusCode)
	}
	b.mu.Lock()
	_, sigue := b.sessions[sid]
	b.mu.Unlock()
	if sigue {
		t.Fatal("la sesión muerta sigue en la tabla, contando para el tope")
	}
}
