package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatewayFalso imita al gateway MCP: sesiones con id, 202 a las notificaciones,
// el eco, DELETE. Con sse=true contesta en text/event-stream. Los servicios en
// lleno devuelven 503 al initialize, como el tope de réplicas.
type gatewayFalso struct {
	sse      bool
	lleno    map[string]bool
	n        atomic.Int64
	mu       sync.Mutex
	abiertas map[string]bool
	borradas int
	token    string
}

func (g *gatewayFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		return
	}
	if g.token != "" && r.Header.Get("Authorization") != "Bearer "+g.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	svc := strings.TrimPrefix(r.URL.Path, "/mcp/")
	sid := r.Header.Get(sessionHeader)
	if r.Method == http.MethodDelete {
		g.mu.Lock()
		if !g.abiertas[sid] {
			g.mu.Unlock()
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		delete(g.abiertas, sid)
		g.borradas++
		g.mu.Unlock()
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &msg)
	if msg.Method == "initialize" {
		if g.lleno[svc] {
			http.Error(w, "could not place session: all replicas full", http.StatusServiceUnavailable)
			return
		}
		sid = fmt.Sprintf("s-%d", g.n.Add(1))
		g.mu.Lock()
		g.abiertas[sid] = true
		g.mu.Unlock()
		w.Header().Set(sessionHeader, sid)
	} else {
		g.mu.Lock()
		ok := g.abiertas[sid]
		g.mu.Unlock()
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var res any = map[string]any{"protocolVersion": "2025-06-18"}
	if msg.Method == "tools/call" {
		res = map[string]any{"content": []any{map[string]any{"type": "text", "text": msg.Params.Arguments["text"]}}}
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": res})
	if g.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: %s\n\n", out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func nuevoFalso(sse bool, llenos ...string) *gatewayFalso {
	g := &gatewayFalso{sse: sse, lleno: map[string]bool{}, abiertas: map[string]bool{}, token: "t0k"}
	for _, s := range llenos {
		g.lleno[s] = true
	}
	return g
}

func clienteDe(srv *httptest.Server) *mcpClient {
	return &mcpClient{http: srv.Client(), gateway: srv.URL, token: "t0k", timeout: 5 * time.Second}
}

func TestRunLoadJSONySSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		g := nuevoFalso(sse)
		srv := httptest.NewServer(g)
		res := runLoad(t.Context(), clienteDe(srv), []string{"a", "b", "c"}, 20, callSpec{Tool: "echo", Args: json.RawMessage(`{"text":"x"}`)}, 3)
		srv.Close()
		porServicio := map[string]int{}
		for _, r := range res {
			if r.Err != nil {
				t.Fatalf("sse=%v: %v", sse, r.Err)
			}
			if len(r.Steady) != 3 || r.TTFRMS <= 0 || r.TTFRMS < r.InitMS {
				t.Errorf("sse=%v: medidas raras %+v", sse, r)
			}
			porServicio[r.Service]++
		}
		if porServicio["a"] != 7 || porServicio["b"] != 7 || porServicio["c"] != 6 {
			t.Errorf("reparto rotatorio: %v", porServicio)
		}
		if g.borradas != 20 || len(g.abiertas) != 0 {
			t.Errorf("sse=%v: %d DELETE, %d sesiones sin cerrar", sse, g.borradas, len(g.abiertas))
		}
	}
}

// Un 503 del gateway (tope de réplicas) se cuenta por fase y código, y la
// celda pasa a DEGRADED: más del 1 % de sesiones fallidas.
func TestErroresPorCodigoYDegradado(t *testing.T) {
	g := nuevoFalso(false, "lleno")
	srv := httptest.NewServer(g)
	defer srv.Close()
	o := &options{gateway: srv.URL, services: []string{"ok", "lleno"}, sessions: 10, calls: 1, timeout: time.Second, tool: "echo"}
	res := runLoad(t.Context(), clienteDe(srv), o.services, o.sessions, callSpec{Tool: "echo", Args: json.RawMessage(`{}`)}, o.calls)
	r := buildReport(o, time.Now(), HostInfo{}, res, time.Second)
	if r.SessionsOK != 5 || r.SessionsFailed != 5 || r.Errors["init:http_503"] != 5 {
		t.Fatalf("report: ok=%d failed=%d errors=%v", r.SessionsOK, r.SessionsFailed, r.Errors)
	}
	if r.Status != "DEGRADED" || r.ErrorRatePct != 50 {
		t.Errorf("status=%s rate=%v", r.Status, r.ErrorRatePct)
	}
	if !strings.Contains(r.mdRow(), "| 5/10 |") || !strings.Contains(r.mdRow(), "DEGRADED") {
		t.Errorf("fila md: %s", r.mdRow())
	}
	// Las sesiones fallidas en el initialize no dejan nada que cerrar.
	if g.borradas != 5 {
		t.Errorf("%d DELETE, quería 5", g.borradas)
	}
}

func TestSinTokenEs401(t *testing.T) {
	g := nuevoFalso(false)
	srv := httptest.NewServer(g)
	defer srv.Close()
	c := clienteDe(srv)
	c.token = ""
	r := c.runSession(t.Context(), "a", callSpec{Tool: "echo", Args: json.RawMessage(`{}`)}, 0)
	if r.Err == nil || r.Err.Stage != "init" || r.Err.Code != "http_401" {
		t.Fatalf("err = %v", r.Err)
	}
}

func TestRPCCheck(t *testing.T) {
	casos := map[string]string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`:                              "",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`: "rpc_-32601",
		`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[]}}`:   "tool_error",
		`{"jsonrpc":"2.0","id":1}`:                                          "no_result",
		`<html>bad gateway</html>`:                                          "bad_json",
	}
	for in, want := range casos {
		if got, _ := rpcCheck([]byte(in)); got != want {
			t.Errorf("rpcCheck(%s) = %q, quería %q", in, got, want)
		}
	}
}

func TestParseServices(t *testing.T) {
	got, err := parseServices("tb-0..2, extra ,x1..1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "tb-0,tb-1,tb-2,extra,x1" {
		t.Errorf("parseServices = %v", got)
	}
	for _, bad := range []string{"", " , ", "tb-3..1", "a,a", "tb-0..1,tb-1", "a/b", "tb-0..5000"} {
		if _, err := parseServices(bad); err == nil {
			t.Errorf("parseServices(%q) no falló", bad)
		}
	}
}

func TestParseFlagsLimites(t *testing.T) {
	for _, args := range [][]string{
		{"-services", "a", "-sessions", "201"},
		{"-services", "a", "-timeout", "121s"},
		{"-services", "a", "-timeout", "0s"},
		{"-services", "a", "-args", "[1]"},
		{"-sessions", "1"},
	} {
		if _, _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("parseFlags(%v) no falló", args)
		}
	}
	o, _, err := parseFlags([]string{"-services", "a", "-sessions", "300", "-max-sessions", "300"}, io.Discard)
	if err != nil || o.sessions != 300 {
		t.Errorf("-max-sessions no se respetó: %v %v", o, err)
	}
}
