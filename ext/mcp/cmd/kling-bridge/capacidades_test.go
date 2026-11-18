package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
)

// Un cliente que declara capabilities {} —el agregador del gateway, /mcp/_all—
// no va a contestar un sampling. El puente se lo contesta al servidor con un
// error y la llamada termina; antes se le mandaba al cliente, que lo tiraba, y
// el servidor se quedaba esperando hasta el plazo de la petición.
func TestPeticionSinCapacidadDeclaradaSeContestaEnElPuente(t *testing.T) {
	b := testBridge()
	b.reqTimeout = 5 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(b.handle))
	srv.Client().Timeout = 10 * time.Second
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close(); b.closeAll() })

	resp := enviar(t, srv, "", mcp.AcceptMCP,
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"capabilities":{}}}`)
	resp.Body.Close()
	sid := resp.Header.Get(SessionHeader)
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d, sesión %q", resp.StatusCode, sid)
	}

	inicio := time.Now()
	resp = enviar(t, srv, sid, mcp.AcceptMCP,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muestrea"}}`)
	defer resp.Body.Close()
	cuerpo, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("al cliente le llegó un flujo (%s) con la petición que no puede contestar: %s", ct, cuerpo)
	}
	var fin map[string]any
	if err := json.Unmarshal(cuerpo, &fin); err != nil || textoDelResultado(fin) != "muestreado: " {
		t.Fatalf("resultado del tools/call: %s", cuerpo)
	}
	if d := time.Since(inicio); d > 2*time.Second {
		t.Fatalf("la llamada tardó %v: esperó al plazo en vez de contestarse", d)
	}
}

// Un CR entre elementos del JSON del servidor es válido en JSON pero corta la
// línea en SSE: el mensaje tiene que llegar a notes sin ninguno.
func TestMensajeDelServidorSinRetornos(t *testing.T) {
	s := newTestSession(t)
	defer s.close()
	// cat devuelve la línea: el servidor «emite» una notificación con un CR.
	if err := s.send([]byte("{\"jsonrpc\":\"2.0\",\r\"method\":\"notifications/message\",\"params\":{}}")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-s.notes:
		if strings.ContainsRune(string(m), '\r') {
			t.Fatalf("el mensaje llegó con un CR, que parte el evento SSE: %q", m)
		}
		var v map[string]any
		if json.Unmarshal(m, &v) != nil || v["method"] != "notifications/message" {
			t.Fatalf("el mensaje cambió al quitarle el CR: %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("la notificación no llegó")
	}
}

// Con la capacidad declarada la petición sí le llega al cliente.
func TestPeticionConCapacidadDeclaradaLlegaAlCliente(t *testing.T) {
	s := newTestSession(t)
	defer s.close()
	s.fijarCapacidades(map[string]json.RawMessage{"sampling": json.RawMessage(`{}`)})
	s.deliverServerMessage(json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage"}`),
		json.RawMessage(`7`), "sampling/createMessage")
	select {
	case m := <-s.notes:
		if !strings.Contains(string(m), "sampling/createMessage") {
			t.Fatalf("llegó otra cosa: %s", m)
		}
	default:
		t.Fatal("la petición de sampling no llegó al cliente que la declaró")
	}
}
