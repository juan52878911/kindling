package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cada sesión acaba en una microVM Android: -max-sessions las acota.
func TestMCPMaxSesiones(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	s := &mcpServer{a: a, golden: "phone-golden", idle: time.Minute, ttl: time.Hour, maxSessions: 2, sessions: map[string]*mcpSession{}}
	srv := httptest.NewServer(s.handler("/sec/mcp"))
	defer srv.Close()
	for i, quiero := range []int{200, 200, 503} {
		c := &mcpClient{t: t, url: srv.URL + "/sec/mcp"}
		if _, code := c.rpc("initialize", map[string]any{}); code != quiero {
			t.Fatalf("initialize %d: %d, want %d", i+1, code, quiero)
		}
	}
}

// Sin sesión solo cabe un initialize: el cuerpo grande no se lee entero.
func TestMCPCuerpoGrandeSinSesion(t *testing.T) {
	a, _, _ := testApp(t)
	s := &mcpServer{a: a, golden: "g", sessions: map[string]*mcpSession{}}
	srv := httptest.NewServer(s.handler("/sec/mcp"))
	defer srv.Close()
	grande := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"` + strings.Repeat("a", maxCuerpoSinSesion) + `"}}`
	resp, err := http.Post(srv.URL+"/sec/mcp", "application/json", strings.NewReader(grande))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body without a session: %d", resp.StatusCode)
	}
	if len(s.sessions) != 0 {
		t.Fatal("a session was opened")
	}
}

// Un teléfono que no se pudo borrar al cerrar la sesión no se queda vivo: la
// siguiente vuelta de reap lo vuelve a intentar.
func TestMCPReintentaBorrarElTelefono(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	s := &mcpServer{a: a, golden: "phone-golden", idle: time.Minute, ttl: time.Hour, sessions: map[string]*mcpSession{}}
	srv := httptest.NewServer(s.handler("/sec/mcp"))
	defer srv.Close()
	c := &mcpClient{t: t, url: srv.URL + "/sec/mcp"}
	c.rpc("initialize", map[string]any{})
	nombre := c.tool("screen", map[string]any{})["content"].([]any)[1].(map[string]any)["text"].(string)

	f.mu.Lock()
	f.removeFails = 1
	f.mu.Unlock()
	if !s.closeSession(context.Background(), c.sid) {
		t.Fatal("session not closed")
	}
	if f.machine(nombre) == nil {
		t.Fatal("the remove did not fail (test setup)")
	}
	s.reap(context.Background())
	if f.machine(nombre) != nil {
		t.Fatalf("%s still alive after reap", nombre)
	}
}

// Si despertar un repuesto falla, el repuesto ya lleva dueño: se borra en vez
// de quedar vivo y fuera del fondo.
func TestClaimConResumeFallidoBorraElRepuesto(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.poolFill(ctx, "phone-golden", 1); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.thawFails = true
	f.mu.Unlock()
	var mu sync.Mutex
	if _, _, err := a.claim(ctx, "phone-golden", "mcp-aaa", &mu); err == nil {
		t.Fatal("claim with a failing resume succeeded")
	}
	if ps, _ := a.phones(ctx); len(ps) != 0 {
		t.Fatalf("%d phone(s) left after a failed claim", len(ps))
	}
}

// Al arrancar, el servidor borra los teléfonos con dueño que dejó un servidor
// anterior en su misma dirección; los de otra dirección y los repuestos no.
func TestMCPBarreHuerfanosDeSuDireccion(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	var mu sync.Mutex
	a.mcpKey = "127.0.0.1:8095"
	viejo, _, err := a.claim(ctx, "phone-golden", "mcp-viejo", &mu)
	if err != nil {
		t.Fatal(err)
	}
	a.mcpKey = "127.0.0.1:9999"
	otro, _, err := a.claim(ctx, "phone-golden", "mcp-otro", &mu)
	if err != nil {
		t.Fatal(err)
	}
	a.mcpKey = "127.0.0.1:8095"
	s := &mcpServer{a: a, golden: "phone-golden", sessions: map[string]*mcpSession{}}
	s.barrerHuerfanos(ctx)
	if f.machine(viejo.Name) != nil {
		t.Fatalf("%s (left by a previous run here) not removed", viejo.Name)
	}
	if f.machine(otro.Name) == nil {
		t.Fatalf("%s (another server's) removed", otro.Name)
	}
	if got := f.machine(otro.Name).m.Labels[labelMCP]; got != "127.0.0.1:9999" {
		t.Fatalf("claimed phone labeled %q", got)
	}
}

// El muro y el MCP no escuchan fuera de loopback.
func TestEscucharSoloLoopback(t *testing.T) {
	ln, err := escucharLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if ln, err := escucharLoopback("0.0.0.0:0"); err == nil {
		ln.Close()
		t.Fatal("listening on every interface accepted")
	}
}
